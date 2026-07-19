package controlplane

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/sausheong/runtime/internal/config"
	"github.com/sausheong/runtime/internal/gateway"
	"github.com/sausheong/runtime/internal/identity"
)

// UpstreamStore is the persistence the onboarding API needs (satisfied by
// *gateway.UpstreamStore).
type UpstreamStore interface {
	InsertUpstream(ctx context.Context, r gateway.UpstreamRow) error
	ListUpstreams(ctx context.Context, tenant string) ([]gateway.UpstreamRow, error)
	GetUpstream(ctx context.Context, id string) (gateway.UpstreamRow, bool, error)
	DeleteUpstream(ctx context.Context, tenant, id string) error
}

// GatewayMutator is the live-manager surface the API drives (satisfied by
// *gateway.Manager).
type GatewayMutator interface {
	Add(cfg config.GatewayServer) error
	Remove(name string)
	Status(tenant string) []gateway.UpstreamStatus
}

// UpstreamParams is the validated request to register an upstream.
type UpstreamParams struct {
	Name       string   `json:"name"`
	Command    string   `json:"command"` // present ONLY to detect+reject stdio via self-service
	URL        string   `json:"url"`
	OpenAPI    string   `json:"openapi"`
	BaseURL    string   `json:"base_url"`
	Operations []string `json:"operations"`
	CredSecret string   `json:"cred_secret"`
	CredHeader string   `json:"cred_header"`
}

// CredTypeFunc reports the credential type (identity.CredTypeStatic or
// identity.CredTypeOAuth2) for a (tenant, name) pair. Backed by
// *identity.Broker.CredType. A nil CredTypeFunc means the broker is unavailable
// and the oauth2-on-openapi registration check is skipped (dial-time
// fail-closed remains the backstop).
type CredTypeFunc func(ctx context.Context, tenant, name string) (string, error)

// checkOAuth2Openapi enforces that an oauth2 credential is only attached to an
// openapi upstream. credType may be nil (broker unavailable) ⇒ skip the check;
// dial-time fail-closed remains the backstop. Returns nil for static creds and
// for oauth2 creds on openapi upstreams. A credType lookup error (e.g. the cred
// has not been created yet) is treated as "unknown" and does not block — dial
// fails closed.
func checkOAuth2Openapi(ctx context.Context, credType CredTypeFunc, tenant string, p UpstreamParams) error {
	if credType == nil || p.CredSecret == "" {
		return nil
	}
	ct, err := credType(ctx, tenant, p.CredSecret)
	if err != nil {
		return nil // unknown cred (e.g. not created yet) — don't block; dial fails closed
	}
	if ct == identity.CredTypeOAuth2 && p.OpenAPI == "" {
		return fmt.Errorf("credential %q is an oauth2 credential and is only valid on an openapi upstream", p.CredSecret)
	}
	return nil
}

// RegisterUpstreamShared validates params, persists the row, and adds it to the
// live manager. Shared by the API and the console (HTTP-agnostic: no
// http.ResponseWriter — callers map the returned error onto a status code).
// tenant is the owning tenant (already resolved by the caller). credType is an
// optional broker-backed lookup enforcing oauth2-creds-only-on-openapi; nil
// skips that check. Returns the stored row.
func RegisterUpstreamShared(ctx context.Context, store UpstreamStore, mut GatewayMutator, credType CredTypeFunc, tenant string, p UpstreamParams) (gateway.UpstreamRow, error) {
	if p.Name == "" || strings.Contains(p.Name, "__") {
		return gateway.UpstreamRow{}, fmt.Errorf("name required and must not contain %q", "__")
	}
	if p.Command != "" {
		return gateway.UpstreamRow{}, fmt.Errorf("stdio (command) upstreams are not allowed via self-service")
	}
	transport, err := resolveTransport(p)
	if err != nil {
		return gateway.UpstreamRow{}, err
	}
	if (p.CredSecret == "") != (p.CredHeader == "") {
		return gateway.UpstreamRow{}, fmt.Errorf("cred_secret and cred_header must both be set or both omitted")
	}
	if err := checkOAuth2Openapi(ctx, credType, tenant, p); err != nil {
		return gateway.UpstreamRow{}, err
	}
	id, err := genID("gwu")
	if err != nil {
		return gateway.UpstreamRow{}, err
	}
	row := gateway.UpstreamRow{
		ID: id, TenantID: tenant, Name: p.Name, Transport: transport,
		URL: p.URL, OpenAPI: p.OpenAPI, BaseURL: p.BaseURL, Operations: p.Operations,
		CredSecret: p.CredSecret, CredHeader: p.CredHeader,
	}
	if err := store.InsertUpstream(ctx, row); err != nil {
		if isDuplicateUpstream(err) {
			return gateway.UpstreamRow{}, fmt.Errorf("an upstream named %q already exists in this tenant", p.Name)
		}
		return gateway.UpstreamRow{}, err
	}
	if err := mut.Add(row.ToConfig()); err != nil {
		// roll back persistence so DB and live state stay consistent
		if delErr := store.DeleteUpstream(ctx, tenant, id); delErr != nil {
			slog.Warn("onboarding: rollback of upstream row failed after manager add error",
				"tenant", tenant, "id", id, "err", delErr)
		}
		return gateway.UpstreamRow{}, err
	}
	return row, nil
}

// isDuplicateUpstream reports whether err is a unique-constraint violation
// (duplicate (tenant_id, name)). Matches the pq/pgx duplicate-key signal
// without depending on a specific driver error type.
func isDuplicateUpstream(err error) bool {
	return err != nil && strings.Contains(err.Error(), "duplicate key")
}

// RemoveUpstreamShared removes a row (scoped to tenant) and detaches it from the
// manager. Idempotent; no existence oracle. HTTP-agnostic for console reuse.
func RemoveUpstreamShared(ctx context.Context, store UpstreamStore, mut GatewayMutator, tenant, id string) error {
	row, ok, err := store.GetUpstream(ctx, id)
	if err != nil {
		return err
	}
	if ok && row.TenantID == tenant {
		if err := store.DeleteUpstream(ctx, tenant, id); err != nil {
			return err
		}
		mut.Remove(row.Name)
	}
	return nil
}

func resolveTransport(p UpstreamParams) (string, error) {
	n := 0
	transport := ""
	if p.URL != "" {
		n++
		transport = "http"
	}
	if p.OpenAPI != "" {
		n++
		transport = "openapi"
	}
	if n != 1 {
		return "", fmt.Errorf("exactly one of url or openapi is required")
	}
	if transport == "http" && (p.BaseURL != "" || len(p.Operations) > 0) {
		return "", fmt.Errorf("base_url/operations are only valid with openapi")
	}
	return transport, nil
}

func genID(prefix string) (string, error) {
	var b [9]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return prefix + "-" + hex.EncodeToString(b[:]), nil
}

// MintAgentKey mints a service key for tenant and stores its hash. Returns the
// id and one-time plaintext. Shared by POST /admin/keys and the console.
func MintAgentKey(ctx context.Context, store AdminStore, tenant string, role identity.Role, label string) (id, plaintext string, err error) {
	mk, err := identity.MintServiceKey()
	if err != nil {
		return "", "", err
	}
	if err := store.InsertServiceKey(ctx, mk.ID, tenant, mk.Hash, role, label); err != nil {
		return "", "", err
	}
	return mk.ID, mk.Plaintext, nil
}

// RegisterUpstreamAdmin mounts /admin/upstreams. Admin-scoped; writes pinned to
// the caller's tenant via effectiveTenant. When us/mut are nil the feature is
// disabled and handlers return 503.
func RegisterUpstreamAdmin(mux *http.ServeMux, store AdminStore, us UpstreamStore, mut GatewayMutator, credType CredTypeFunc) {
	mux.HandleFunc("POST /admin/upstreams", func(w http.ResponseWriter, r *http.Request) {
		p, ok := requireAdmin(w, r)
		if !ok {
			return
		}
		if us == nil || mut == nil {
			http.Error(w, "gateway onboarding not configured", http.StatusServiceUnavailable)
			return
		}
		var body struct {
			UpstreamParams
			Tenant string `json:"tenant"`
		}
		if !decode(w, r, &body) {
			return
		}
		tenant, ok := effectiveTenant(w, r, store, p, body.Tenant)
		if !ok {
			return
		}
		row, err := RegisterUpstreamShared(r.Context(), us, mut, credType, tenant, body.UpstreamParams)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		slog.Info("onboarding: upstream registered", "tenant", tenant, "id", row.ID, "transport", row.Transport)
		writeJSON(w, http.StatusCreated, map[string]string{"id": row.ID, "name": row.Name})
	})

	mux.HandleFunc("GET /admin/upstreams", func(w http.ResponseWriter, r *http.Request) {
		p, ok := requireAdmin(w, r)
		if !ok {
			return
		}
		if us == nil {
			http.Error(w, "gateway onboarding not configured", http.StatusServiceUnavailable)
			return
		}
		tenant := p.TenantID // "" for superuser ⇒ all
		rows, err := us.ListUpstreams(r.Context(), tenant)
		if err != nil {
			serverError(w, "list upstreams", err)
			return
		}
		writeJSON(w, http.StatusOK, rows)
	})

	mux.HandleFunc("DELETE /admin/upstreams/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, ok := requireAdmin(w, r)
		if !ok {
			return
		}
		if us == nil || mut == nil {
			http.Error(w, "gateway onboarding not configured", http.StatusServiceUnavailable)
			return
		}
		if err := RemoveUpstreamShared(r.Context(), us, mut, p.TenantID, r.PathValue("id")); err != nil {
			serverError(w, "remove upstream", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
