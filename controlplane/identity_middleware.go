package controlplane

import (
	"context"
	"net/http"
	"strings"

	"github.com/sausheong/runtime/internal/identity"
)

// authenticator is the subset of *identity.Authenticator the middleware needs
// (an interface so tests can stub it).
type authenticator interface {
	Authenticate(ctx context.Context, r *http.Request) (identity.Principal, error)
}

// IdentityMiddleware authenticates every request to a Principal and authorizes
// it against the target agent's tenant + the derived action. Exemptions match
// the open-mode middleware: /healthz, /ui/login, /ui/static/*. Errors map to
// 401 (unauthenticated), 403 (forbidden / not provisioned), 404 (cross-tenant or
// unknown agent). For /ui paths, an auth failure redirects to /ui/login.
func IdentityMiddleware(next http.Handler, a authenticator, az *identity.Authorizer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isExempt(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		p, err := a.Authenticate(r.Context(), r)
		if err != nil {
			if strings.HasPrefix(r.URL.Path, "/ui") {
				http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
				return
			}
			switch err {
			case identity.ErrNotProvisioned:
				http.Error(w, "forbidden", http.StatusForbidden)
			default:
				http.Error(w, "unauthorized", http.StatusUnauthorized)
			}
			return
		}

		// Authorize agent-scoped requests (the /agents/{id}/... subtree). Other
		// paths (/agents list, /admin/*, /ui) are authorized by their handlers.
		if id := agentIDFromPath(r.URL.Path); id != "" {
			if azErr := az.Authorize(p, id, actionForRequest(r.Method, r.URL.Path)); azErr != nil {
				writeAuthzError(w, azErr)
				return
			}
		}

		ctx := context.WithValue(r.Context(), principalKey, p)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func isExempt(path string) bool {
	return path == "/healthz" || path == "/ui/login" || strings.HasPrefix(path, "/ui/static/")
}

// writeAuthzError maps Authorizer errors to HTTP codes (404 hides cross-tenant
// existence; 403 is an in-tenant permission denial).
func writeAuthzError(w http.ResponseWriter, err error) {
	switch err {
	case identity.ErrNotFound:
		http.Error(w, "not found", http.StatusNotFound)
	case identity.ErrForbidden:
		http.Error(w, "forbidden", http.StatusForbidden)
	default:
		http.Error(w, "forbidden", http.StatusForbidden)
	}
}

// agentIDFromPath extracts {id} from /agents/{id}/... ; "" for /agents or others.
func agentIDFromPath(path string) string {
	const p = "/agents/"
	if !strings.HasPrefix(path, p) {
		return ""
	}
	rest := strings.TrimPrefix(path, p)
	if rest == "" {
		return ""
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return rest[:i]
	}
	return rest
}

// actionForRequest derives the coarse Action from method+path.
func actionForRequest(method, path string) identity.Action {
	if method == http.MethodPost && strings.HasSuffix(path, "/sessions") {
		return identity.ActionInvoke
	}
	return identity.ActionRead
}
