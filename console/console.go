// Package console serves the read-only operator web UI.
package console

import (
	"context"
	"embed"
	"html/template"
	"io/fs"
	"net/http"

	"github.com/sausheong/runtime/controlplane"
	"github.com/sausheong/runtime/internal/agentstore"
	"github.com/sausheong/runtime/internal/identity"
	"github.com/sausheong/runtime/internal/store"
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

// Onboarding bundles the dependencies for the self-service onboarding page.
// nil ⇒ onboarding disabled (open mode / no identity); the page 404s.
type Onboarding struct {
	Upstreams controlplane.UpstreamStore
	Mutator   controlplane.GatewayMutator
	Admin     controlplane.AdminStore
	Secrets   controlplane.SecretAdmin
	Agents    controlplane.AgentStore    // dynamic managed-agent persistence; nil ⇒ section hidden
	AgentMgr  *controlplane.AgentManager // live attach/detach; nil ⇒ section hidden
}

// Handler returns the console's HTTP handler. The read-only views render the
// agents overview from the registry and link to the control-plane API + SSE
// endpoints it is mounted beside. When onb is non-nil it additionally mounts the
// self-service onboarding page and its CSRF-guarded, admin-gated POST handlers.
// st (the control-plane store) is retained for compatibility; observability
// tallies and the activity feed now read each agent's own HTTP API.
func Handler(reg *controlplane.Registry, st store.Store, oidc OIDCConfig, onb *Onboarding) http.Handler {
	mux := http.NewServeMux()
	csrf := newCSRF()
	var aclient agentClient = httpAgentClient{}

	mux.Handle("GET /ui/static/", http.StripPrefix("/ui/static/", http.FileServerFS(staticFS)))

	// landing renders the public front door (hero + Google sign-in, or a token
	// form when OIDC is off). Served at both / and /ui/login. With OIDC on, the
	// Google button links to the IdP authorize URL carrying a random `state`,
	// which is also set in a short-lived cookie and verified in /ui/callback to
	// defeat login-CSRF (see oauthstate.go).
	landing := func(w http.ResponseWriter, r *http.Request) {
		data := map[string]any{"GoogleEnabled": false, "GoogleURL": ""}
		if oidc.Enabled && oidc.AuthCodeURL != nil {
			state, err := newOAuthState()
			if err != nil {
				http.Error(w, "login init failed", http.StatusInternalServerError)
				return
			}
			setOAuthStateCookie(w, state)
			data["GoogleEnabled"] = true
			data["GoogleURL"] = oidc.AuthCodeURL(state)
		}
		render(w, "landing.html", data)
	}
	mux.HandleFunc("GET /{$}", landing)
	mux.HandleFunc("GET /ui/login", landing)

	mux.HandleFunc("POST /ui/login", func(w http.ResponseWriter, r *http.Request) {
		// Console is OIDC-only when an IdP is configured: the paste-a-token path is
		// disabled so a service key can't be turned into a browser session here.
		// (The edge middleware also rejects non-OIDC cookies on /ui; this closes
		// the door at the setter too.) Service keys remain valid for the API.
		if oidc.Enabled {
			http.Error(w, "token login disabled; sign in with Google", http.StatusForbidden)
			return
		}
		_ = r.ParseForm()
		setSessionCookie(w, r.FormValue("token"))
		http.Redirect(w, r, "/ui", http.StatusSeeOther)
	})

	mux.HandleFunc("POST /ui/logout", func(w http.ResponseWriter, r *http.Request) {
		clearSessionCookie(w)
		http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
	})

	mux.HandleFunc("GET /ui/callback", func(w http.ResponseWriter, r *http.Request) {
		if oidc.Exchange == nil {
			http.Error(w, "oidc not configured", http.StatusBadRequest)
			return
		}
		// Login-CSRF defense: the ?state= echoed by the IdP must match the
		// rt_oauth_state cookie set when we issued the authorize URL. Reject
		// before spending the code. Clear the one-time cookie either way.
		if !validOAuthState(r) {
			clearOAuthStateCookie(w)
			http.Error(w, "invalid login state", http.StatusBadRequest)
			return
		}
		clearOAuthStateCookie(w)
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

	mux.HandleFunc("GET /ui/observability", func(w http.ResponseWriter, r *http.Request) {
		agents := visibleAgents(r, reg)
		fleet := buildFleetObs(r.Context(), reg, aclient, httpProbe, agents)
		if onb != nil {
			// onb is non-nil only when identity is on, and IdentityMiddleware then
			// always injects a principal on a successful request — so the else
			// (open-mode, all tenants) branch is effectively unreachable here;
			// kept for safety and to mirror principalCanSeeTenant's open-mode rule.
			if p, ok := controlplane.PrincipalFromContext(r.Context()); ok {
				fleet.Upstreams = onb.Mutator.Status(p.TenantID)
			} else {
				fleet.Upstreams = onb.Mutator.Status("")
			}
		}
		render(w, "observability.html", map[string]any{"Fleet": fleet})
	})

	mux.HandleFunc("GET /ui/agents/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		ap, ok := reg.Get(id)
		if !ok || !principalCanSeeTenant(r, ap.Tenant) {
			http.NotFound(w, r)
			return
		}
		// Name/Model are omitted: reg.Get returns an AgentProcess (no display
		// name), and the agent panel renders health/sessions, not the name. The
		// page heading already shows the id. Avoid setting Name to the id, which
		// would mislead any future template that surfaces Obs.Name.
		obs := buildAgentObs(r.Context(), reg, aclient, httpProbe, controlplane.AgentInfo{
			ID: ap.AgentID, Tenant: ap.Tenant,
		})
		feed := buildAgentFeed(r.Context(), reg, aclient, ap.AgentID, 10, 50)
		metrics := buildAgentMetrics(r.Context(), reg, aclient, ap.AgentID)
		render(w, "agent.html", map[string]any{"AgentID": id, "Obs": obs, "Feed": feed, "Metrics": metrics})
	})

	mux.HandleFunc("GET /ui/agents/{id}/sessions/{sid}", func(w http.ResponseWriter, r *http.Request) {
		render(w, "session.html", map[string]any{
			"AgentID":   r.PathValue("id"),
			"SessionID": r.PathValue("sid"),
		})
	})

	if onb != nil {
		mux.HandleFunc("GET /ui/onboarding", func(w http.ResponseWriter, r *http.Request) {
			p, ok := controlplane.PrincipalFromContext(r.Context())
			if !ok || p.Role != identity.RoleAdmin {
				http.Error(w, "forbidden: admin required", http.StatusForbidden)
				return
			}
			// One-time flash from a prior POST-redirect-GET; clear it on display.
			flash := ""
			if c, err := r.Cookie("rt_flash"); err == nil {
				flash = c.Value
				http.SetCookie(w, &http.Cookie{
					Name: "rt_flash", Value: "", Path: "/ui/onboarding",
					MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode,
				})
			}
			ups, _ := onb.Upstreams.ListUpstreams(r.Context(), p.TenantID)
			// Secrets is the one optional dep: nil when no keyring is configured
			// (RUNTIME_SECRETS_KEYS unset) even though gateway upstreams enable the
			// onboarding page. Guard the listing so an admin can still mint keys and
			// register credential-less upstreams without panicking the request.
			var secs []identity.SecretMeta
			if onb.Secrets != nil {
				secs, _ = onb.Secrets.ListSecretNames(r.Context(), p.TenantID)
			}
			allKeys, _ := onb.Admin.ListKeys(r.Context(), p.TenantID)
			// Show only active keys: a revoked key is dead and only clutters the
			// list. It stays auditable via the API (GET /admin/keys returns it).
			keys := make([]identity.KeyRow, 0, len(allKeys))
			for _, k := range allKeys {
				if !k.Revoked {
					keys = append(keys, k)
				}
			}
			users, _ := onb.Admin.ListUsers(r.Context(), p.TenantID)
			var agents []agentstore.AgentRow
			if onb.Agents != nil {
				agents, _ = onb.Agents.List(r.Context(), p.TenantID)
			}
			render(w, "onboarding.html", map[string]any{
				"CSRF": csrf.issue(sessionValue(r)), "Tenant": p.TenantID,
				"Upstreams": ups, "Secrets": secs, "Keys": keys, "Users": users, "Flash": flash,
				"SecretsEnabled": onb.Secrets != nil,
				"Agents":         agents, "AgentsEnabled": onb.Agents != nil && onb.AgentMgr != nil,
			})
		})

		guard := func(fn func(p identity.Principal, w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) {
				p, ok := controlplane.PrincipalFromContext(r.Context())
				if !ok || p.Role != identity.RoleAdmin {
					http.Error(w, "forbidden: admin required", http.StatusForbidden)
					return
				}
				_ = r.ParseForm()
				if !csrf.verify(sessionValue(r), r.FormValue("csrf_token")) {
					http.Error(w, "invalid csrf token", http.StatusForbidden)
					return
				}
				fn(p, w, r)
			}
		}

		mux.HandleFunc("POST /ui/onboarding/keys", guard(func(p identity.Principal, w http.ResponseWriter, r *http.Request) {
			role, err := identity.RoleFromString(r.FormValue("role"))
			if err != nil {
				http.Error(w, "valid role required", http.StatusBadRequest)
				return
			}
			_, plaintext, err := controlplane.MintAgentKey(r.Context(), onb.Admin, p.TenantID, role, r.FormValue("label"))
			if err != nil {
				http.Error(w, "mint failed", http.StatusInternalServerError)
				return
			}
			flashRedirect(w, r, "key:"+plaintext)
		}))

		mux.HandleFunc("POST /ui/onboarding/keys/{id}/delete", guard(func(p identity.Principal, w http.ResponseWriter, r *http.Request) {
			id := r.PathValue("id")
			// RevokeKey is tenant-scoped, so an admin can only revoke keys in their
			// own tenant; a cross-tenant id is a silent no-op. (The console is
			// OIDC-only, so the caller is never authenticated by a service key — there
			// is no "current key" to self-revoke and lock out.)
			if err := onb.Admin.RevokeKey(r.Context(), p.TenantID, id); err != nil {
				http.Error(w, "revoke key failed", http.StatusInternalServerError)
				return
			}
			flashRedirect(w, r, "key revoked")
		}))

		mux.HandleFunc("POST /ui/onboarding/users", guard(func(p identity.Principal, w http.ResponseWriter, r *http.Request) {
			subject := r.FormValue("subject")
			if subject == "" {
				http.Error(w, "subject required", http.StatusBadRequest)
				return
			}
			role, err := identity.RoleFromString(r.FormValue("role"))
			if err != nil {
				http.Error(w, "valid role required", http.StatusBadRequest)
				return
			}
			// Anti-lockout: an admin must not demote their own subject below admin.
			if subject == p.Subject && role != identity.RoleAdmin {
				http.Error(w, "cannot demote yourself", http.StatusBadRequest)
				return
			}
			if err := onb.Admin.UpsertUser(r.Context(), p.TenantID, subject, role); err != nil {
				http.Error(w, "upsert user failed", http.StatusInternalServerError)
				return
			}
			flashRedirect(w, r, "user saved")
		}))

		mux.HandleFunc("POST /ui/onboarding/users/{subject}/delete", guard(func(p identity.Principal, w http.ResponseWriter, r *http.Request) {
			subject := r.PathValue("subject")
			// Anti-lockout: an admin must not remove their own subject.
			if subject == p.Subject {
				http.Error(w, "cannot remove yourself", http.StatusBadRequest)
				return
			}
			if err := onb.Admin.DeleteUser(r.Context(), p.TenantID, subject); err != nil {
				http.Error(w, "delete user failed", http.StatusInternalServerError)
				return
			}
			flashRedirect(w, r, "user removed")
		}))

		mux.HandleFunc("POST /ui/onboarding/secrets", guard(func(p identity.Principal, w http.ResponseWriter, r *http.Request) {
			if onb.Secrets == nil {
				http.Error(w, "secrets broker not configured (set RUNTIME_SECRETS_KEYS)", http.StatusServiceUnavailable)
				return
			}
			if err := onb.Secrets.SetSecret(r.Context(), p.TenantID, r.FormValue("name"), r.FormValue("value")); err != nil {
				http.Error(w, "set secret failed", http.StatusBadRequest)
				return
			}
			flashRedirect(w, r, "secret set")
		}))

		mux.HandleFunc("POST /ui/onboarding/upstreams", guard(func(p identity.Principal, w http.ResponseWriter, r *http.Request) {
			params := controlplane.UpstreamParams{
				Name: r.FormValue("name"), URL: r.FormValue("url"),
				OpenAPI: r.FormValue("openapi"), BaseURL: r.FormValue("base_url"),
				CredSecret: r.FormValue("cred_secret"), CredHeader: r.FormValue("cred_header"),
			}
			if _, err := controlplane.RegisterUpstreamShared(r.Context(), onb.Upstreams, onb.Mutator, p.TenantID, params); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			flashRedirect(w, r, "upstream registered")
		}))

		mux.HandleFunc("POST /ui/onboarding/upstreams/{id}/delete", guard(func(p identity.Principal, w http.ResponseWriter, r *http.Request) {
			if err := controlplane.RemoveUpstreamShared(r.Context(), onb.Upstreams, onb.Mutator, p.TenantID, r.PathValue("id")); err != nil {
				http.Error(w, "remove failed", http.StatusInternalServerError)
				return
			}
			flashRedirect(w, r, "upstream removed")
		}))

		// Managed agents: register/deregister/enable/disable/re-attach remote
		// agents at runtime. Mounted only when the store + live manager exist.
		if onb.Agents != nil && onb.AgentMgr != nil {
			mux.HandleFunc("POST /ui/onboarding/agents", guard(func(p identity.Principal, w http.ResponseWriter, r *http.Request) {
				params := controlplane.AgentParams{
					ID: r.FormValue("id"), Name: r.FormValue("name"),
					Model: r.FormValue("model"), URL: r.FormValue("url"),
					AuthSecret: r.FormValue("auth_secret"),
				}
				if _, err := controlplane.RegisterAgentShared(r.Context(), onb.Agents, onb.AgentMgr, p.TenantID, params); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				flashRedirect(w, r, "agent registered")
			}))

			mux.HandleFunc("POST /ui/onboarding/agents/{id}/delete", guard(func(p identity.Principal, w http.ResponseWriter, r *http.Request) {
				if err := controlplane.DeregisterAgentShared(r.Context(), onb.Agents, onb.AgentMgr, p.TenantID, r.PathValue("id")); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				flashRedirect(w, r, "agent deregistered")
			}))

			setAgentEnabled := func(enabled bool, msg string) http.HandlerFunc {
				return guard(func(p identity.Principal, w http.ResponseWriter, r *http.Request) {
					id := r.PathValue("id")
					if !onb.AgentMgr.IsManaged(id) {
						http.Error(w, "agent is not dynamically managed", http.StatusBadRequest)
						return
					}
					if err := onb.Agents.SetEnabled(r.Context(), p.TenantID, id, enabled); err != nil {
						http.Error(w, "update failed", http.StatusInternalServerError)
						return
					}
					onb.AgentMgr.SetEnabled(id, enabled)
					flashRedirect(w, r, msg)
				})
			}
			mux.HandleFunc("POST /ui/onboarding/agents/{id}/enable", setAgentEnabled(true, "agent enabled"))
			mux.HandleFunc("POST /ui/onboarding/agents/{id}/disable", setAgentEnabled(false, "agent disabled"))

			mux.HandleFunc("POST /ui/onboarding/agents/{id}/restart", guard(func(p identity.Principal, w http.ResponseWriter, r *http.Request) {
				id := r.PathValue("id")
				if !onb.AgentMgr.IsManaged(id) {
					http.Error(w, "agent is not dynamically managed", http.StatusBadRequest)
					return
				}
				onb.AgentMgr.Reattach(id)
				flashRedirect(w, r, "agent re-attached")
			}))
		}
	}

	return mux
}

// sessionValue returns the runtime_token cookie value, which the CSRF token is
// bound to. Invariant: in identity mode an admin principal is derived FROM this
// cookie, so a present principal implies a non-empty session value — i.e. the
// CSRF token is never bound to the empty string for a real admin. (If that ever
// changes, all admins would share the HMAC of "" and tokens would cross-forge.)
func sessionValue(r *http.Request) string {
	if c, err := r.Cookie("runtime_token"); err == nil {
		return c.Value
	}
	return ""
}

// flashRedirect performs POST-redirect-GET to the onboarding page with a one-time
// message in a short-lived cookie (not persisted server-side; cleared on display).
func flashRedirect(w http.ResponseWriter, r *http.Request, msg string) {
	http.SetCookie(w, &http.Cookie{Name: "rt_flash", Value: msg, Path: "/ui/onboarding", MaxAge: 30, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/ui/onboarding", http.StatusSeeOther)
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

// clearSessionCookie expires the runtime_token cookie, logging the user out. The
// Name/Path must match setSessionCookie so the browser overwrites the same cookie.
func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: "runtime_token", Value: "",
		Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: -1,
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
