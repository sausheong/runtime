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

// RegisterUpstreamShared validates params, persists the row, and adds it to the
// live manager. Shared by the API and the console (HTTP-agnostic: no
// http.ResponseWriter — callers map the returned error onto a status code).
// tenant is the owning tenant (already resolved by the caller). Returns the
// stored row.
func RegisterUpstreamShared(ctx context.Context, store UpstreamStore, mut GatewayMutator, tenant string, p UpstreamParams) (gateway.UpstreamRow, error) {
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
func RegisterUpstreamAdmin(mux *http.ServeMux, store AdminStore, us UpstreamStore, mut GatewayMutator) {
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
		row, err := RegisterUpstreamShared(r.Context(), us, mut, tenant, body.UpstreamParams)
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
