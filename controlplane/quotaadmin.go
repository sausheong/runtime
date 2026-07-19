package controlplane

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/sausheong/runtime/internal/quota"
)

// QuotaStore is the persistence surface the quota admin layer needs.
// Implemented by *quota.Store and *quota.MemStore.
type QuotaStore interface {
	Insert(ctx context.Context, r quota.Rule) error
	List(ctx context.Context, tenant string) ([]quota.Rule, error)
	Delete(ctx context.Context, tenant, upstream string) (bool, error)
}

// RegisterQuotaShared validates RBAC and persists one quota. superuser may
// target any tenant incl. the "*" wildcard; a non-superuser may write only its
// own tenant and never "*". tenant is the RESOLVED write-target tenant.
func RegisterQuotaShared(ctx context.Context, st QuotaStore, callerTenant string, superuser bool, tenant, upstream string, rate int) error {
	if tenant == "" || upstream == "" {
		return errors.New("tenant and upstream required")
	}
	if !superuser {
		if tenant == "*" {
			return errors.New("only a superuser may set a '*' tenant quota")
		}
		if tenant != callerTenant {
			return errors.New("cannot set a quota for another tenant")
		}
	}
	return st.Insert(ctx, quota.Rule{Tenant: tenant, Upstream: upstream, RatePerMin: rate})
}

func ListQuotasShared(ctx context.Context, st QuotaStore, tenant string) ([]quota.Rule, error) {
	return st.List(ctx, tenant)
}

func RemoveQuotaShared(ctx context.Context, st QuotaStore, tenant, upstream string) error {
	_, err := st.Delete(ctx, tenant, upstream)
	return err
}

// RegisterQuotaAdmin mounts /admin/quotas. Admin-scoped; nil store ⇒ 503.
func RegisterQuotaAdmin(mux *http.ServeMux, store AdminStore, qs QuotaStore) {
	mux.HandleFunc("POST /admin/quotas", func(w http.ResponseWriter, r *http.Request) {
		p, ok := requireAdmin(w, r)
		if !ok {
			return
		}
		if qs == nil {
			http.Error(w, "quotas not configured", http.StatusServiceUnavailable)
			return
		}
		var body struct {
			Tenant     string `json:"tenant"`
			Upstream   string `json:"upstream"`
			RatePerMin int    `json:"rate_per_min"`
		}
		if !decode(w, r, &body) {
			return
		}
		// An empty tenant defaults to the caller's own tenant (convenience).
		// Any explicit value is passed through to RegisterQuotaShared so its
		// RBAC guards run: a superuser may name any tenant (incl. "*"), while a
		// non-superuser naming "*" or another tenant is REJECTED (not rewritten).
		target := body.Tenant
		if target == "" {
			target = p.TenantID
		}
		if err := RegisterQuotaShared(r.Context(), qs, p.TenantID, p.Superuser, target, body.Upstream, body.RatePerMin); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		slog.Info("quota registered", "tenant", target, "upstream", body.Upstream, "rate", body.RatePerMin)
		writeJSON(w, http.StatusCreated, map[string]any{"tenant": target, "upstream": body.Upstream, "rate_per_min": body.RatePerMin})
	})

	mux.HandleFunc("GET /admin/quotas", func(w http.ResponseWriter, r *http.Request) {
		p, ok := requireAdmin(w, r)
		if !ok {
			return
		}
		if qs == nil {
			http.Error(w, "quotas not configured", http.StatusServiceUnavailable)
			return
		}
		tenant := p.TenantID
		if p.Superuser {
			tenant = r.URL.Query().Get("tenant") // "" ⇒ all
		}
		rows, err := ListQuotasShared(r.Context(), qs, tenant)
		if err != nil {
			serverError(w, "list quotas", err)
			return
		}
		writeJSON(w, http.StatusOK, rows)
	})

	mux.HandleFunc("DELETE /admin/quotas", func(w http.ResponseWriter, r *http.Request) {
		p, ok := requireAdmin(w, r)
		if !ok {
			return
		}
		if qs == nil {
			http.Error(w, "quotas not configured", http.StatusServiceUnavailable)
			return
		}
		upstream := r.URL.Query().Get("upstream")
		tenant := p.TenantID
		if p.Superuser {
			if q := r.URL.Query().Get("tenant"); q != "" {
				tenant = q
			}
		}
		if err := RemoveQuotaShared(r.Context(), qs, tenant, upstream); err != nil {
			serverError(w, "remove quota", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
