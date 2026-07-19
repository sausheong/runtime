package controlplane

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/sausheong/runtime/internal/policy"
)

// PolicyStore is the tenant-policy persistence surface the admin layer needs.
// Implemented by *policy.Store (and *policy.MemStore in tests).
type PolicyStore interface {
	Insert(ctx context.Context, r policy.Row) error
	List(ctx context.Context, tenant string) ([]policy.Row, error)
	Delete(ctx context.Context, tenant, name string) (bool, error)
}

// RegisterPolicyShared validates and persists one tenant-layer Cedar policy.
// HTTP-agnostic (no ResponseWriter) so the API and console share it; callers
// map the returned error onto a status code. tenant is already resolved.
func RegisterPolicyShared(ctx context.Context, st PolicyStore, tenant, name, cedarText string) error {
	if name == "" {
		return errors.New("name required")
	}
	// Insert validates the Cedar text (parseable + exactly one statement) and
	// rejects duplicates; surface its error verbatim (it carries the parser
	// message, which the UI shows the author).
	return st.Insert(ctx, policy.Row{Tenant: tenant, Name: name, CedarText: cedarText})
}

// ListPoliciesShared returns a tenant's policies (or all when tenant=="").
func ListPoliciesShared(ctx context.Context, st PolicyStore, tenant string) ([]policy.Row, error) {
	return st.List(ctx, tenant)
}

// RemovePolicyShared deletes a policy scoped to its tenant. No existence
// oracle: a missing policy is not an error (callers return 204 regardless).
func RemovePolicyShared(ctx context.Context, st PolicyStore, tenant, name string) error {
	_, err := st.Delete(ctx, tenant, name)
	return err
}

// RegisterPolicyAdmin mounts /admin/policies. Admin-scoped; writes pinned to
// the caller's tenant via effectiveTenant. When ps is nil the feature is
// disabled and handlers return 503.
func RegisterPolicyAdmin(mux *http.ServeMux, store AdminStore, ps PolicyStore) {
	mux.HandleFunc("POST /admin/policies", func(w http.ResponseWriter, r *http.Request) {
		p, ok := requireAdmin(w, r)
		if !ok {
			return
		}
		if ps == nil {
			http.Error(w, "policy engine not configured", http.StatusServiceUnavailable)
			return
		}
		var body struct {
			Name      string `json:"name"`
			CedarText string `json:"cedar_text"`
			Tenant    string `json:"tenant"`
		}
		if !decode(w, r, &body) {
			return
		}
		tenant, ok := effectiveTenant(w, r, store, p, body.Tenant)
		if !ok {
			return
		}
		if err := RegisterPolicyShared(r.Context(), ps, tenant, body.Name, body.CedarText); err != nil {
			// Validation/duplicate errors are client errors carrying the Cedar
			// parser message; 400 with the message so authors can fix the text.
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		slog.Info("policy registered", "tenant", tenant, "name", body.Name)
		writeJSON(w, http.StatusCreated, map[string]string{"name": body.Name})
	})

	mux.HandleFunc("GET /admin/policies", func(w http.ResponseWriter, r *http.Request) {
		p, ok := requireAdmin(w, r)
		if !ok {
			return
		}
		if ps == nil {
			http.Error(w, "policy engine not configured", http.StatusServiceUnavailable)
			return
		}
		tenant := p.TenantID // "" for superuser ⇒ all tenants
		if p.Superuser {
			if q := r.URL.Query().Get("tenant"); q != "" {
				tenant = q
			}
		}
		rows, err := ListPoliciesShared(r.Context(), ps, tenant)
		if err != nil {
			serverError(w, "list policies", err)
			return
		}
		writeJSON(w, http.StatusOK, rows)
	})

	mux.HandleFunc("DELETE /admin/policies/{name}", func(w http.ResponseWriter, r *http.Request) {
		p, ok := requireAdmin(w, r)
		if !ok {
			return
		}
		if ps == nil {
			http.Error(w, "policy engine not configured", http.StatusServiceUnavailable)
			return
		}
		// Delete is tenant-scoped; a superuser may target another tenant via
		// ?tenant=. Cross-tenant/missing deletes are no-oracle 204.
		tenant := p.TenantID
		if p.Superuser {
			if q := r.URL.Query().Get("tenant"); q != "" {
				tenant = q
			}
		}
		if err := RemovePolicyShared(r.Context(), ps, tenant, r.PathValue("name")); err != nil {
			serverError(w, "remove policy", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
