package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/sausheong/runtime/internal/identity"
)

// AdminStore is the subset of *identity.Store the admin API needs. Exported so
// cmd/runtimed can pass the store (or nil, in open mode) into buildRoot.
type AdminStore interface {
	CreateTenant(ctx context.Context, id, name string) error
	UpsertUser(ctx context.Context, tenantID, subject string, role identity.Role) error
	DeleteUser(ctx context.Context, tenantID, subject string) error
	ListUsers(ctx context.Context, tenantID string) ([]identity.UserRow, error)
	InsertServiceKey(ctx context.Context, id, tenantID, hash string, role identity.Role, label string) error
	RevokeKey(ctx context.Context, tenantID, id string) error
	ListKeys(ctx context.Context, tenantID string) ([]identity.KeyRow, error)
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
		var body struct{ Subject, Role string }
		if !decode(w, r, &body) {
			return
		}
		role, err := identity.RoleFromString(body.Role)
		if err != nil || body.Subject == "" {
			http.Error(w, "subject and valid role required", http.StatusBadRequest)
			return
		}
		if err := s.UpsertUser(r.Context(), p.TenantID, body.Subject, role); err != nil {
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
		var body struct{ Label, Role string }
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
		if err := s.InsertServiceKey(r.Context(), mk.ID, p.TenantID, mk.Hash, role, body.Label); err != nil {
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
