// Package console serves the read-only operator web UI.
package console

import (
	"context"
	"embed"
	"html/template"
	"io/fs"
	"net/http"

	"github.com/sausheong/runtime/controlplane"
)

//go:embed templates/*.html static/*
var assets embed.FS

var tmpl = template.Must(template.ParseFS(assets, "templates/*.html"))

// staticFS scopes the static file server to the static/ subtree only, so an
// encoded path-traversal request (e.g. /ui/static/..%2ftemplates/...) cannot
// escape into the templates embedded alongside it.
var staticFS = mustSub(assets, "static")

func mustSub(f fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(f, dir)
	if err != nil {
		panic(err)
	}
	return sub
}

// OIDCConfig configures the console's OIDC login. Zero value = OIDC disabled
// (paste-token fallback shown).
type OIDCConfig struct {
	AuthCodeURL func(state string) string                                             // builds the IdP authorize URL
	Exchange    func(ctx context.Context, code string) (rawIDToken string, err error) // code -> raw ID token (validated downstream by the request Authenticator)
	Enabled     bool
}

// Handler returns the console's HTTP handler. Read-only: it renders the agents
// overview from the registry and links to the control-plane API + SSE endpoints
// it is mounted beside.
func Handler(reg *controlplane.Registry, oidc OIDCConfig) http.Handler {
	mux := http.NewServeMux()

	mux.Handle("GET /ui/static/", http.StripPrefix("/ui/static/", http.FileServerFS(staticFS)))

	mux.HandleFunc("GET /ui/login", func(w http.ResponseWriter, r *http.Request) {
		if oidc.Enabled && oidc.AuthCodeURL != nil {
			// M1 limitation: the OAuth `state` is a fixed placeholder and is not
			// validated in the callback, so this flow does not yet protect against
			// login-CSRF. Acceptable for a read-only console behind edge auth; a
			// later milestone should generate a random state (+ nonce) and verify
			// it in /ui/callback.
			http.Redirect(w, r, oidc.AuthCodeURL("state"), http.StatusSeeOther)
			return
		}
		render(w, "login.html", nil)
	})

	mux.HandleFunc("POST /ui/login", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		setSessionCookie(w, r.FormValue("token"))
		http.Redirect(w, r, "/ui", http.StatusSeeOther)
	})

	mux.HandleFunc("GET /ui/callback", func(w http.ResponseWriter, r *http.Request) {
		if oidc.Exchange == nil {
			http.Error(w, "oidc not configured", http.StatusBadRequest)
			return
		}
		idToken, err := oidc.Exchange(r.Context(), r.URL.Query().Get("code"))
		if err != nil {
			http.Error(w, "login failed", http.StatusUnauthorized)
			return
		}
		setSessionCookie(w, idToken)
		http.Redirect(w, r, "/ui", http.StatusSeeOther)
	})

	mux.HandleFunc("GET /ui", func(w http.ResponseWriter, r *http.Request) {
		render(w, "overview.html", map[string]any{"Agents": visibleAgents(r, reg)})
	})

	mux.HandleFunc("GET /ui/agents/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		ap, ok := reg.Get(id)
		if !ok || !principalCanSeeTenant(r, ap.Tenant) {
			http.NotFound(w, r)
			return
		}
		render(w, "agent.html", map[string]any{"AgentID": id})
	})

	mux.HandleFunc("GET /ui/agents/{id}/sessions/{sid}", func(w http.ResponseWriter, r *http.Request) {
		render(w, "session.html", map[string]any{
			"AgentID":   r.PathValue("id"),
			"SessionID": r.PathValue("sid"),
		})
	})

	return mux
}

// setSessionCookie writes the runtime_token cookie the identity Authenticator
// reads. HttpOnly + SameSite=Lax. Secure is intentionally NOT set so the console
// works over plain HTTP for local/internal use; terminate TLS upstream in
// production (and set Secure there if exposing the console).
func setSessionCookie(w http.ResponseWriter, value string) {
	http.SetCookie(w, &http.Cookie{
		Name: "runtime_token", Value: value,
		Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
}

// principalCanSeeTenant reports whether the request's principal may see resources
// owned by the given tenant, applying the same rule as the control-plane API:
// open mode (no principal) or superuser → all tenants; otherwise only the
// principal's own tenant.
func principalCanSeeTenant(r *http.Request, tenant string) bool {
	p, hasP := controlplane.PrincipalFromContext(r.Context())
	if !hasP || p.Superuser {
		return true
	}
	return tenant == p.TenantID
}

// visibleAgents returns the agents the request's principal may see, applying the
// tenant rule from principalCanSeeTenant (open mode / superuser → all; else only
// the principal's tenant).
func visibleAgents(r *http.Request, reg *controlplane.Registry) []controlplane.AgentInfo {
	all := reg.List()
	out := make([]controlplane.AgentInfo, 0, len(all))
	for _, a := range all {
		if principalCanSeeTenant(r, a.Tenant) {
			out = append(out, a)
		}
	}
	return out
}

func render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
