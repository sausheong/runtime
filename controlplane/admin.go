package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"

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
	InsertServiceKey(ctx context.Context, id, tenantID, hash string, role identity.Role, label string) error
	RevokeKey(ctx context.Context, tenantID, id string) error
	ListKeys(ctx context.Context, tenantID string) ([]identity.KeyRow, error)
	ListTenants(ctx context.Context) ([]identity.TenantRow, error)
}

// RegisterAdmin mounts the /admin/* routes on mux. Every handler requires an
// admin Principal (set by the identity middleware) and scopes writes to that
// principal's tenant; tenant creation additionally requires a superuser.
func RegisterAdmin(mux *http.ServeMux, s AdminStore) {
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
		mk, err := identity.MintServiceKey()
		if err != nil {
			serverError(w, "mint key", err)
			return
		}
		tenant, ok := effectiveTenant(w, r, s, p, body.Tenant)
		if !ok {
			return
		}
		if err := s.InsertServiceKey(r.Context(), mk.ID, tenant, mk.Hash, role, body.Label); err != nil {
			serverError(w, "insert key", err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"id": mk.ID, "plaintext": mk.Plaintext})
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
}

// SecretAdmin is the write surface for tenant secrets, implemented by
// *identity.Broker (it seals before persisting). A nil SecretAdmin means the
// feature is disabled (no master key) and handlers return 503.
type SecretAdmin interface {
	SetSecret(ctx context.Context, tenant, name, plaintext string) error
	ListSecretNames(ctx context.Context, tenant string) ([]identity.SecretMeta, error)
	DeleteSecret(ctx context.Context, tenant, name string) error
	RotateSecrets(ctx context.Context, tenant string) (identity.RotateStats, error)
}

// envNameRe restricts secret names to valid env-var identifiers so an injected
// var can't smuggle '=' or newlines into the child environment.
var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

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
		var body struct{ Name, Value, Tenant string }
		if !decode(w, r, &body) {
			return
		}
		if body.Value == "" || !envNameRe.MatchString(body.Name) {
			http.Error(w, "valid name (env identifier) and non-empty value required", http.StatusBadRequest)
			return
		}
		tenant, ok := effectiveTenant(w, r, store, p, body.Tenant)
		if !ok {
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
		metas, err := sa.ListSecretNames(r.Context(), p.TenantID)
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
