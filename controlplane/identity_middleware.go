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
//
// onReject (nil-safe) fires once with the status code at every rejection write
// path — rejected requests never reach the inner handler chain, so this hook is
// the only way edge metrics can observe them.
func IdentityMiddleware(next http.Handler, a authenticator, az *identity.Authorizer, onReject func(status int)) http.Handler {
	return identityMiddleware(next, a, az, onReject, false)
}

// IdentityMiddlewareConsoleOIDCOnly is IdentityMiddleware with the console
// restricted to OIDC sessions: an authenticated request to a /ui path whose
// principal did not come from the IdP (service key, bootstrap, legacy — e.g. a
// manually-set runtime_token cookie) is bounced to /ui/login. API paths are
// unaffected, so service keys and the break-glass bootstrap key still work for
// scripts. Only meaningful when OIDC is enabled; otherwise there is no way to
// obtain a console session and the UI would be unreachable.
func IdentityMiddlewareConsoleOIDCOnly(next http.Handler, a authenticator, az *identity.Authorizer, onReject func(status int)) http.Handler {
	return identityMiddleware(next, a, az, onReject, true)
}

func identityMiddleware(next http.Handler, a authenticator, az *identity.Authorizer, onReject func(status int), consoleOIDCOnly bool) http.Handler {
	reject := func(status int) {
		if onReject != nil {
			onReject(status)
		}
	}
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
				reject(http.StatusSeeOther)
				http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
				return
			}
			switch {
			case errors.Is(err, identity.ErrNotProvisioned):
				reject(http.StatusForbidden)
				http.Error(w, "forbidden", http.StatusForbidden)
			default:
				reject(http.StatusUnauthorized)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
			}
			return
		}

		// Console is OIDC-only: a valid non-OIDC credential (service-key/bootstrap
		// cookie) authenticates for the API but must not drive the browser console.
		// Bounce to /ui/login, which renders the Google sign-in (it is exempt, so
		// this does not loop).
		if consoleOIDCOnly && strings.HasPrefix(cleanPath, "/ui") && p.Kind != identity.KindOIDC {
			reject(http.StatusSeeOther)
			http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
			return
		}

		if id := agentIDFromPath(cleanPath); id != "" {
			if azErr := az.Authorize(p, id, actionForRequest(r.Method, cleanPath)); azErr != nil {
				status := authzStatus(azErr)
				reject(status)
				http.Error(w, authzMessage(status), status)
				return
			}
		}

		ctx := context.WithValue(r.Context(), principalKey, p)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func isExempt(path string) bool {
	// "/" is the public landing page (hero + Google sign-in) — it must render to
	// an unauthenticated visitor, so it is exempt. Note path.Clean has already run,
	// so this matches only the exact root, never an unauthenticated deeper path.
	//
	// /ui/callback is exempt because the OIDC redirect lands here with ?code=...
	// and no session cookie yet (the callback handler sets it after exchanging the
	// code). Gating it would redirect to /ui/login, which re-initiates OIDC — an
	// infinite loop. The handler validates the code itself, so this is safe.
	return path == "/" || path == "/healthz" || path == "/ui/login" || path == "/ui/callback" ||
		strings.HasPrefix(path, "/ui/static/")
}

// authzStatus maps Authorizer errors to HTTP codes (404 hides cross-tenant
// existence; 403 is an in-tenant permission denial).
func authzStatus(err error) int {
	if errors.Is(err, identity.ErrNotFound) {
		return http.StatusNotFound
	}
	return http.StatusForbidden
}

// authzMessage is the response body matching an authzStatus code.
func authzMessage(status int) string {
	if status == http.StatusNotFound {
		return "not found"
	}
	return "forbidden"
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
