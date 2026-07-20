package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/sausheong/runtime/internal/identity"
)

// AdminStore is the subset of *identity.Store the admin API needs. Exported so
// cmd/runtimed can pass the store (or nil, in open mode) into buildRoot.
type AdminStore interface {
	CreateTenant(ctx context.Context, id, name string) error
	TenantExists(ctx context.Context, id string) (bool, error)
	UpsertUser(ctx context.Context, tenantID, subject string, role identity.Role) error
	DeleteUser(ctx context.Context, tenantID, subject string) error
	ListUsers(ctx context.Context, tenantID string) ([]identity.UserRow, error)
	UsersBySubject(ctx context.Context, subject string) ([]identity.UserRow, error)
	InsertServiceKey(ctx context.Context, id, tenantID, hash string, role identity.Role, label string) error
	RevokeKey(ctx context.Context, tenantID, id string) error
	ListKeys(ctx context.Context, tenantID string) ([]identity.KeyRow, error)
	ListTenants(ctx context.Context) ([]identity.TenantRow, error)
	InsertRegistrationToken(ctx context.Context, tokenID, agentID, hash string) error
	ListRegistrationTokens(ctx context.Context) ([]identity.RegTokenRow, error)
	RevokeRegistrationToken(ctx context.Context, tokenID string) error
}

// RegisterAdmin mounts the /admin/* routes on mux. Every handler requires an
// admin Principal (set by the identity middleware) and scopes writes to that
// principal's tenant; tenant creation additionally requires a superuser.
func RegisterAdmin(mux *http.ServeMux, s AdminStore, agentTenants map[string]string) {
	mux.HandleFunc("POST /admin/tenants", func(w http.ResponseWriter, r *http.Request) {
		p, ok := requireAdmin(w, r)
		if !ok {
			return
		}
		if !p.Superuser {
			http.Error(w, "forbidden: superuser required", http.StatusForbidden)
			return
		}
		var body struct{ ID, Name string }
		if !decode(w, r, &body) {
			return
		}
		if body.ID == "" {
			http.Error(w, "id required", http.StatusBadRequest)
			return
		}
		if err := s.CreateTenant(r.Context(), body.ID, body.Name); err != nil {
			serverError(w, "create tenant", err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"id": body.ID})
	})

	mux.HandleFunc("POST /admin/users", func(w http.ResponseWriter, r *http.Request) {
		p, ok := requireAdmin(w, r)
		if !ok {
			return
		}
		var body struct{ Subject, Role, Tenant string }
		if !decode(w, r, &body) {
			return
		}
		role, err := identity.RoleFromString(body.Role)
		if err != nil || body.Subject == "" {
			http.Error(w, "subject and valid role required", http.StatusBadRequest)
			return
		}
		tenant, ok := effectiveTenant(w, r, s, p, body.Tenant)
		if !ok {
			return
		}
		if err := s.UpsertUser(r.Context(), tenant, body.Subject, role); err != nil {
			serverError(w, "upsert user", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"subject": body.Subject})
	})

	mux.HandleFunc("GET /admin/users", func(w http.ResponseWriter, r *http.Request) {
		p, ok := requireAdmin(w, r)
		if !ok {
			return
		}
		rows, err := s.ListUsers(r.Context(), p.TenantID)
		if err != nil {
			serverError(w, "list users", err)
			return
		}
		writeJSON(w, http.StatusOK, rows)
	})

	mux.HandleFunc("DELETE /admin/users/{subject}", func(w http.ResponseWriter, r *http.Request) {
		p, ok := requireAdmin(w, r)
		if !ok {
			return
		}
		if err := s.DeleteUser(r.Context(), p.TenantID, r.PathValue("subject")); err != nil {
			serverError(w, "delete user", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("POST /admin/keys", func(w http.ResponseWriter, r *http.Request) {
		p, ok := requireAdmin(w, r)
		if !ok {
			return
		}
		var body struct{ Label, Role, Tenant string }
		if !decode(w, r, &body) {
			return
		}
		role, err := identity.RoleFromString(body.Role)
		if err != nil {
			http.Error(w, "valid role required", http.StatusBadRequest)
			return
		}
		tenant, ok := effectiveTenant(w, r, s, p, body.Tenant)
		if !ok {
			return
		}
		id, plaintext, err := MintAgentKey(r.Context(), s, tenant, role, body.Label)
		if err != nil {
			serverError(w, "mint key", err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"id": id, "plaintext": plaintext})
	})

	mux.HandleFunc("DELETE /admin/keys/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, ok := requireAdmin(w, r)
		if !ok {
			return
		}
		if err := s.RevokeKey(r.Context(), p.TenantID, r.PathValue("id")); err != nil {
			serverError(w, "revoke key", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("GET /admin/keys", func(w http.ResponseWriter, r *http.Request) {
		p, ok := requireAdmin(w, r)
		if !ok {
			return
		}
		rows, err := s.ListKeys(r.Context(), p.TenantID)
		if err != nil {
			serverError(w, "list keys", err)
			return
		}
		writeJSON(w, http.StatusOK, rows)
	})

	mux.HandleFunc("POST /admin/register-tokens", func(w http.ResponseWriter, r *http.Request) {
		p, ok := requireAdmin(w, r)
		if !ok {
			return
		}
		var body struct{ Agent string }
		if !decode(w, r, &body) {
			return
		}
		if body.Agent == "" {
			http.Error(w, "agent required", http.StatusBadRequest)
			return
		}
		tenant, known := agentTenants[body.Agent]
		if !known {
			http.Error(w, "unknown agent", http.StatusBadRequest)
			return
		}
		// A non-superuser admin may only mint for agents in their own tenant
		// (the token grants access to that agent's tenant's brokered secrets).
		if !p.Superuser && tenant != p.TenantID {
			http.Error(w, "forbidden: agent belongs to another tenant", http.StatusForbidden)
			return
		}
		mk, err := identity.MintServiceKey()
		if err != nil {
			serverError(w, "mint registration token", err)
			return
		}
		if err := s.InsertRegistrationToken(r.Context(), mk.ID, body.Agent, mk.Hash); err != nil {
			serverError(w, "insert registration token", err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"id": mk.ID, "plaintext": mk.Plaintext})
	})

	mux.HandleFunc("GET /admin/register-tokens", func(w http.ResponseWriter, r *http.Request) {
		p, ok := requireAdmin(w, r)
		if !ok {
			return
		}
		rows, err := s.ListRegistrationTokens(r.Context())
		if err != nil {
			serverError(w, "list registration tokens", err)
			return
		}
		// Non-superusers see only tokens for agents in their tenant.
		if !p.Superuser {
			filtered := rows[:0]
			for _, rw := range rows {
				if agentTenants[rw.AgentID] == p.TenantID {
					filtered = append(filtered, rw)
				}
			}
			rows = filtered
		}
		writeJSON(w, http.StatusOK, rows)
	})

	mux.HandleFunc("DELETE /admin/register-tokens/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, ok := requireAdmin(w, r)
		if !ok {
			return
		}
		id := r.PathValue("id")
		// Superusers may revoke any token. Non-superusers are tenant-scoped
		// (parity with DELETE /admin/keys, which uses RevokeKey's tenant filter):
		// we resolve the token's owning agent via ListRegistrationTokens and
		// revoke only when it belongs to an agent in the caller's tenant.
		// When the token is absent OR owned by another tenant we return the
		// same 204 without revoking — revoke is idempotent either way, and a
		// uniform response avoids an existence oracle for other tenants' tokens.
		if !p.Superuser {
			rows, err := s.ListRegistrationTokens(r.Context())
			if err != nil {
				serverError(w, "list registration tokens", err)
				return
			}
			owned := false
			for _, rw := range rows {
				if rw.TokenID == id && agentTenants[rw.AgentID] == p.TenantID {
					owned = true
					break
				}
			}
			if !owned {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		if err := s.RevokeRegistrationToken(r.Context(), id); err != nil {
			serverError(w, "revoke registration token", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

// SecretAdmin is the write surface for tenant secrets, implemented by
// *identity.Broker (it seals before persisting). A nil SecretAdmin means the
// feature is disabled (no master key) and handlers return 503.
type SecretAdmin interface {
	SetSecret(ctx context.Context, tenant, name, plaintext string) error
	SetOAuth2(ctx context.Context, tenant, name string, cfg identity.OAuth2Config) error
	SetOBO(ctx context.Context, tenant, name string, cfg identity.OBOConfig) error
	ListSecretNames(ctx context.Context, tenant string) ([]identity.SecretMeta, error)
	ListSecrets(ctx context.Context, tenant string) ([]identity.SecretMeta, error)
	DeleteSecret(ctx context.Context, tenant, name string) error
	RotateSecrets(ctx context.Context, tenant string) (identity.RotateStats, error)
}

// envNameRe restricts secret names to valid env-var identifiers so an injected
// var can't smuggle '=' or newlines into the child environment.
var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// reservedEnvPrefixes are env-name prefixes owned by the platform. A brokered
// tenant secret is appended AFTER the fixed control block in envDelta (last
// duplicate wins in exec.Cmd, and the /register fold-to-map has the same
// last-write-wins shape), so a tenant-admin secret named e.g.
// RUNTIME_AGENT_LIMITS could shadow an operator-imposed control var. Names
// with these prefixes are rejected at creation on every path (API + console)
// and skipped defense-in-depth at injection time (envDelta).
var reservedEnvPrefixes = []string{"RUNTIME_", "DBOS__"}

// HasReservedEnvPrefix reports whether name collides with a platform-owned
// env-var prefix. Exported for the console's secret-creation handler.
func HasReservedEnvPrefix(name string) bool {
	for _, p := range reservedEnvPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// ReservedEnvPrefixError formats the uniform rejection message for a
// reserved-prefix secret name (shared by the API and console handlers).
func ReservedEnvPrefixError(name string) string {
	return fmt.Sprintf("secret name %q uses a reserved prefix (RUNTIME_, DBOS__)", name)
}

// RegisterSecretAdmin mounts /admin/secrets on mux. store is reused only for
// effectiveTenant's tenant validation. When sa is nil the handlers return 503.
func RegisterSecretAdmin(mux *http.ServeMux, store AdminStore, sa SecretAdmin) {
	mux.HandleFunc("POST /admin/secrets", func(w http.ResponseWriter, r *http.Request) {
		p, ok := requireAdmin(w, r)
		if !ok {
			return
		}
		if sa == nil {
			http.Error(w, "secrets not configured", http.StatusServiceUnavailable)
			return
		}
		var body struct {
			Name, Value, Tenant, Type string
			TokenURL                  string   `json:"token_url"`
			ClientID                  string   `json:"client_id"`
			ClientSecret              string   `json:"client_secret"`
			Scopes                    []string `json:"scopes"`
			Audience                  string   `json:"audience"`
			SubjectTokenType          string   `json:"subject_token_type"`
			RequestedTokenType        string   `json:"requested_token_type"`
		}
		if !decode(w, r, &body) {
			return
		}
		if !envNameRe.MatchString(body.Name) {
			http.Error(w, "valid name (env identifier) required", http.StatusBadRequest)
			return
		}
		if HasReservedEnvPrefix(body.Name) {
			http.Error(w, ReservedEnvPrefixError(body.Name), http.StatusBadRequest)
			return
		}
		tenant, ok := effectiveTenant(w, r, store, p, body.Tenant)
		if !ok {
			return
		}
		if body.Type == identity.CredTypeOAuth2 {
			cfg := identity.OAuth2Config{
				TokenURL: body.TokenURL, ClientID: body.ClientID, ClientSecret: body.ClientSecret,
				Scopes: body.Scopes, Audience: body.Audience,
			}
			if err := cfg.Validate(); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if err := sa.SetOAuth2(r.Context(), tenant, body.Name, cfg); err != nil {
				serverError(w, "set oauth2 credential", err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{"name": body.Name, "type": identity.CredTypeOAuth2})
			return
		}
		if body.Type == identity.CredTypeOBO {
			cfg := identity.OBOConfig{
				TokenURL: body.TokenURL, ClientID: body.ClientID, ClientSecret: body.ClientSecret,
				Scopes: body.Scopes, Audience: body.Audience,
				SubjectTokenType: body.SubjectTokenType, RequestedTokenType: body.RequestedTokenType,
			}
			if err := cfg.Validate(); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if err := sa.SetOBO(r.Context(), tenant, body.Name, cfg); err != nil {
				serverError(w, "set obo credential", err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{"name": body.Name, "type": identity.CredTypeOBO})
			return
		}
		if body.Value == "" {
			http.Error(w, "non-empty value required", http.StatusBadRequest)
			return
		}
		if err := sa.SetSecret(r.Context(), tenant, body.Name, body.Value); err != nil {
			serverError(w, "set secret", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"name": body.Name})
	})

	mux.HandleFunc("GET /admin/secrets", func(w http.ResponseWriter, r *http.Request) {
		p, ok := requireAdmin(w, r)
		if !ok {
			return
		}
		if sa == nil {
			http.Error(w, "secrets not configured", http.StatusServiceUnavailable)
			return
		}
		metas, err := sa.ListSecrets(r.Context(), p.TenantID)
		if err != nil {
			serverError(w, "list secrets", err)
			return
		}
		writeJSON(w, http.StatusOK, metas)
	})

	mux.HandleFunc("DELETE /admin/secrets/{name}", func(w http.ResponseWriter, r *http.Request) {
		p, ok := requireAdmin(w, r)
		if !ok {
			return
		}
		if sa == nil {
			http.Error(w, "secrets not configured", http.StatusServiceUnavailable)
			return
		}
		if err := sa.DeleteSecret(r.Context(), p.TenantID, r.PathValue("name")); err != nil {
			serverError(w, "delete secret", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("POST /admin/secrets/rotate", func(w http.ResponseWriter, r *http.Request) {
		p, ok := requireAdmin(w, r)
		if !ok {
			return
		}
		if sa == nil {
			http.Error(w, "secrets not configured", http.StatusServiceUnavailable)
			return
		}
		var body struct{ Tenant string }
		if !decode(w, r, &body) {
			return
		}
		// Superuser with no explicit tenant rotates every tenant.
		if p.Superuser && body.Tenant == "" {
			trs, err := store.ListTenants(r.Context())
			if err != nil {
				serverError(w, "list tenants", err)
				return
			}
			out := make([]identity.RotateStats, 0, len(trs))
			for _, tr := range trs {
				st, err := sa.RotateSecrets(r.Context(), tr.ID)
				if err != nil {
					serverError(w, "rotate secrets", err)
					return
				}
				out = append(out, st)
			}
			writeJSON(w, http.StatusOK, out)
			return
		}
		tenant, ok := effectiveTenant(w, r, store, p, body.Tenant)
		if !ok {
			return
		}
		st, err := sa.RotateSecrets(r.Context(), tenant)
		if err != nil {
			serverError(w, "rotate secrets", err)
			return
		}
		writeJSON(w, http.StatusOK, []identity.RotateStats{st})
	})
}

// effectiveTenant returns the tenant a write should target. Non-superusers are
// always pinned to their own tenant (bodyTenant is ignored). A superuser
// (TenantID == "", e.g. the bootstrap/legacy key) MAY target any existing
// tenant via bodyTenant; it must supply one, and it must exist. Returns ok=false
// after writing an error response.
func effectiveTenant(w http.ResponseWriter, r *http.Request, s AdminStore, p identity.Principal, bodyTenant string) (string, bool) {
	if !p.Superuser {
		return p.TenantID, true
	}
	if bodyTenant == "" {
		http.Error(w, "superuser must specify a tenant", http.StatusBadRequest)
		return "", false
	}
	exists, err := s.TenantExists(r.Context(), bodyTenant)
	if err != nil {
		serverError(w, "tenant exists check", err)
		return "", false
	}
	if !exists {
		http.Error(w, "unknown tenant", http.StatusBadRequest)
		return "", false
	}
	return bodyTenant, true
}

// requireAdmin extracts the Principal and enforces the admin role.
func requireAdmin(w http.ResponseWriter, r *http.Request) (identity.Principal, bool) {
	p, ok := PrincipalFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return identity.Principal{}, false
	}
	if p.Role != identity.RoleAdmin {
		http.Error(w, "forbidden: admin required", http.StatusForbidden)
		return identity.Principal{}, false
	}
	return p, true
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return false
	}
	return true
}

// serverError logs the real error and returns a generic 500 (so DB/driver
// internals never reach the client).
func serverError(w http.ResponseWriter, op string, err error) {
	slog.Error("admin: "+op+" failed", "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
