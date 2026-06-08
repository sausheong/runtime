package controlplane

import (
	"context"
	"errors"
	"net/http"
	"path"
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
		// Decide exemption and authorization on the cleaned path so an attacker
		// cannot slip past an exempt prefix with ".." segments (the middleware
		// runs before the mux, so r.URL.Path is not yet normalized).
		cleanPath := path.Clean(r.URL.Path)
		if isExempt(cleanPath) {
			next.ServeHTTP(w, r)
			return
		}
		p, err := a.Authenticate(r.Context(), r)
		if err != nil {
			if strings.HasPrefix(cleanPath, "/ui") {
				http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
				return
			}
			switch {
			case errors.Is(err, identity.ErrNotProvisioned):
				http.Error(w, "forbidden", http.StatusForbidden)
			default:
				http.Error(w, "unauthorized", http.StatusUnauthorized)
			}
			return
		}

		if id := agentIDFromPath(cleanPath); id != "" {
			if azErr := az.Authorize(p, id, actionForRequest(r.Method, cleanPath)); azErr != nil {
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
	switch {
	case errors.Is(err, identity.ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, identity.ErrForbidden):
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

// actionForRequest derives the coarse Action from method+path. GET/HEAD are
// reads; every other method (incl. POST /sessions and any future mutating verb)
// is treated as invoke, so a new write endpoint can never silently fall through
// to read-level (viewer) access.
func actionForRequest(method, path string) identity.Action {
	if method == http.MethodGet || method == http.MethodHead {
		return identity.ActionRead
	}
	return identity.ActionInvoke
}
