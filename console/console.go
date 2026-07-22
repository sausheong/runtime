// Package console serves the read-only operator web UI.
package console

import (
	"context"
	"embed"
	"encoding/json"
	"html/template"
	"io/fs"
	"net/http"
	"strconv"
	"strings"

	"github.com/sausheong/runtime/controlplane"
	"github.com/sausheong/runtime/internal/agentstore"
	"github.com/sausheong/runtime/internal/eval"
	"github.com/sausheong/runtime/internal/identity"
	"github.com/sausheong/runtime/internal/policy"
	"github.com/sausheong/runtime/internal/quota"
	"github.com/sausheong/runtime/internal/store"
)

//go:embed templates/*.html static/*
var assets embed.FS

// tmplFuncs are the helpers available in every template.
var tmplFuncs = template.FuncMap{
	// comma groups an integer with thousands separators: 1234567 -> "1,234,567".
	// Keeps large token counts legible in the narrow stat tiles.
	"comma": comma,
}

var tmpl = template.Must(template.New("").Funcs(tmplFuncs).ParseFS(assets, "templates/*.html"))

// comma formats any integer with thousands separators. Accepts int/int64 (the
// two width types the templates pass: SessionTally is int, AgentMetrics is int64).
func comma(v any) string {
	var n int64
	switch x := v.(type) {
	case int64:
		n = x
	case int:
		n = int64(x)
	default:
		return ""
	}
	s := strconv.FormatInt(n, 10)
	neg := ""
	if n < 0 {
		neg, s = "-", s[1:]
	}
	// Insert a comma every 3 digits from the right.
	out := make([]byte, 0, len(s)+len(s)/3)
	for i, d := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, d)
	}
	return neg + string(out)
}

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
	Upstreams    controlplane.UpstreamStore
	Mutator      controlplane.GatewayMutator
	Admin        controlplane.AdminStore
	Secrets      controlplane.SecretAdmin
	Agents       controlplane.AgentStore    // dynamic managed-agent persistence; nil ⇒ section hidden
	AgentMgr     *controlplane.AgentManager // live attach/detach; nil ⇒ section hidden
	Policies     controlplane.PolicyStore   // tenant Cedar policy store; nil ⇒ section hidden
	Quotas       controlplane.QuotaStore    // gateway rate-quota store; nil ⇒ section hidden
	EvalStore    eval.EvalStore             // golden-set store; nil ⇒ section hidden
	EvalPolicies eval.PolicyStoreAPI        // per-agent online-eval policy store; nil ⇒ section hidden
	// Eval-run launch deps (observability page). A nil EvalStore hides the whole
	// eval-runs section; Invoker/Judge/SignalCtx are only consulted at launch.
	EvalInvoker   eval.Invoker              // registry-backed agent invoker for launched runs
	EvalJudge     eval.Judge                // optional LLM judge (nil ⇒ judge cases fail-the-case)
	EvalMetrics   eval.Metricer             // eval run/case counters; nil ⇒ no metrics (nil-safe in Execute)
	EvalSignalCtx context.Context           // server signal ctx: a launched run outlives the request
	CredType      controlplane.CredTypeFunc // broker-backed cred-type lookup; nil ⇒ oauth2-on-openapi check skipped
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
		clearTenantCookie(w)
		http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
	})

	// Tenant picker for multi-tenant OIDC users. Reachable pending (partial
	// principal, middleware lets this path through) or resolved (Switch tenant).
	mux.HandleFunc("GET /ui/select-tenant", func(w http.ResponseWriter, r *http.Request) {
		p, ok := controlplane.PrincipalFromContext(r.Context())
		if !ok || onb == nil || onb.Admin == nil {
			http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
			return
		}
		rows, err := onb.Admin.UsersBySubject(r.Context(), p.Subject)
		if err != nil {
			http.Error(w, "could not load tenants", http.StatusInternalServerError)
			return
		}
		if len(rows) == 1 {
			setTenantCookie(w, rows[0].TenantID)
			http.Redirect(w, r, "/ui", http.StatusSeeOther)
			return
		}
		render(w, "select-tenant.html", map[string]any{
			"Memberships": rows, "CSRF": csrf.issue(sessionValue(r)),
		})
	})

	mux.HandleFunc("POST /ui/select-tenant", func(w http.ResponseWriter, r *http.Request) {
		p, ok := controlplane.PrincipalFromContext(r.Context())
		if !ok || onb == nil || onb.Admin == nil {
			http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
			return
		}
		if !csrf.verify(sessionValue(r), r.FormValue("csrf_token")) {
			http.Error(w, "invalid csrf token", http.StatusForbidden)
			return
		}
		want := r.FormValue("tenant")
		rows, err := onb.Admin.UsersBySubject(r.Context(), p.Subject)
		if err != nil {
			http.Error(w, "could not load tenants", http.StatusInternalServerError)
			return
		}
		member := false
		for _, u := range rows {
			if u.TenantID == want {
				member = true
				break
			}
		}
		if !member {
			http.Error(w, "not a member of that tenant", http.StatusForbidden)
			return
		}
		setTenantCookie(w, want)
		http.Redirect(w, r, "/ui", http.StatusSeeOther)
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
		data := map[string]any{"Fleet": fleet, "Agents": agents}
		if onb != nil {
			// onb is non-nil only when identity is on, and IdentityMiddleware then
			// always injects a principal on a successful request — so the else
			// (open-mode, all tenants) branch is effectively unreachable here;
			// kept for safety and to mirror principalCanSeeTenant's open-mode rule.
			if p, ok := controlplane.PrincipalFromContext(r.Context()); ok {
				fleet.Upstreams = onb.Mutator.Status(p.TenantID)
				// Eval runs: the launch form + runs table, tenant-scoped. Only when a
				// golden-set store is wired (nil ⇒ section hidden, never a panic).
				if onb.EvalStore != nil {
					runs, _ := onb.EvalStore.ListRuns(r.Context(), p.TenantID)
					sets, _ := onb.EvalStore.ListSets(r.Context(), p.TenantID)
					data["EvalRuns"] = runs
					data["EvalSets"] = sets
					data["EvalRunsEnabled"] = true
					// The launch form is a state-changing POST → carry a CSRF token
					// bound to this session (the observability GET otherwise has none).
					data["CSRF"] = csrf.issue(sessionValue(r))
				}
			} else {
				fleet.Upstreams = onb.Mutator.Status("")
			}
			data["Fleet"] = fleet
		}
		render(w, "observability.html", data)
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
				secs, _ = onb.Secrets.ListSecrets(r.Context(), p.TenantID)
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
			var policies []policy.Row
			if onb.Policies != nil {
				policies, _ = onb.Policies.List(r.Context(), p.TenantID)
			}
			var quotas []quota.Rule
			if onb.Quotas != nil {
				quotas, _ = controlplane.ListQuotasShared(r.Context(), onb.Quotas, p.TenantID)
			}
			var evalSets []eval.Set
			if onb.EvalStore != nil {
				evalSets, _ = onb.EvalStore.ListSets(r.Context(), p.TenantID)
			}
			var evalPolicies []eval.Policy
			if onb.EvalPolicies != nil {
				evalPolicies, _ = onb.EvalPolicies.ListPolicies(r.Context(), p.TenantID)
			}
			// A freshly minted key arrives as "key:<plaintext>" — the one time it
			// is ever shown. Split it out so the template can give it a distinct,
			// copy-it-now treatment instead of a generic success flash.
			newKey := ""
			if k, ok := strings.CutPrefix(flash, "key:"); ok {
				newKey = k
				flash = ""
			}
			render(w, "onboarding.html", map[string]any{
				"CSRF": csrf.issue(sessionValue(r)), "Tenant": p.TenantID,
				"Upstreams": ups, "Secrets": secs, "Keys": keys, "Users": users,
				"Flash": flash, "NewKey": newKey,
				"SecretsEnabled": onb.Secrets != nil,
				"Agents":         agents, "AgentsEnabled": onb.Agents != nil && onb.AgentMgr != nil,
				"Policies": policies, "PoliciesEnabled": onb.Policies != nil,
				"Quotas": quotas, "QuotasEnabled": onb.Quotas != nil,
				"EvalSets": evalSets, "EvalSetsEnabled": onb.EvalStore != nil,
				"EvalPolicies": evalPolicies, "EvalPoliciesEnabled": onb.EvalPolicies != nil,
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
			flashRedirect(w, r, "Key "+id+" revoked. Anything using it can no longer authenticate.")
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
			flashRedirect(w, r, "User "+subject+" saved as "+string(role)+".")
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
			flashRedirect(w, r, "User "+subject+" removed.")
		}))

		mux.HandleFunc("POST /ui/onboarding/secrets", guard(func(p identity.Principal, w http.ResponseWriter, r *http.Request) {
			if onb.Secrets == nil {
				http.Error(w, "secrets broker not configured (set RUNTIME_SECRETS_KEYS)", http.StatusServiceUnavailable)
				return
			}
			// Reserved-prefix guard: a tenant secret named RUNTIME_* / DBOS__*
			// would shadow platform control vars at spawn (see
			// controlplane.HasReservedEnvPrefix). Reject at creation, same as
			// the /admin/secrets API path.
			if controlplane.HasReservedEnvPrefix(r.FormValue("name")) {
				http.Error(w, controlplane.ReservedEnvPrefixError(r.FormValue("name")), http.StatusBadRequest)
				return
			}
			// The name field is shared by both credential forms; branch on the
			// form's type. An oauth2 cred seals a client_credentials config;
			// anything else (absent/empty/"static") stays a static secret.
			if r.FormValue("type") == identity.CredTypeOAuth2 {
				cfg := identity.OAuth2Config{
					TokenURL:     r.FormValue("token_url"),
					ClientID:     r.FormValue("client_id"),
					ClientSecret: r.FormValue("client_secret"),
					Scopes:       splitScopes(r.FormValue("scopes")),
					Audience:     r.FormValue("audience"),
				}
				if err := cfg.Validate(); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				if err := onb.Secrets.SetOAuth2(r.Context(), p.TenantID, r.FormValue("name"), cfg); err != nil {
					http.Error(w, "set oauth2 credential failed", http.StatusBadRequest)
					return
				}
				flashRedirect(w, r, "OAuth2 credential "+r.FormValue("name")+" saved.")
				return
			}
			if r.FormValue("type") == identity.CredTypeOBO {
				cfg := identity.OBOConfig{
					TokenURL:           r.FormValue("token_url"),
					ClientID:           r.FormValue("client_id"),
					ClientSecret:       r.FormValue("client_secret"),
					Scopes:             splitScopes(r.FormValue("scopes")),
					Audience:           r.FormValue("audience"),
					SubjectTokenType:   r.FormValue("subject_token_type"),
					RequestedTokenType: r.FormValue("requested_token_type"),
				}
				if err := cfg.Validate(); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				if err := onb.Secrets.SetOBO(r.Context(), p.TenantID, r.FormValue("name"), cfg); err != nil {
					http.Error(w, "set obo credential failed", http.StatusBadRequest)
					return
				}
				flashRedirect(w, r, "OBO credential "+r.FormValue("name")+" saved.")
				return
			}
			if err := onb.Secrets.SetSecret(r.Context(), p.TenantID, r.FormValue("name"), r.FormValue("value")); err != nil {
				http.Error(w, "set secret failed", http.StatusBadRequest)
				return
			}
			flashRedirect(w, r, "Credential "+r.FormValue("name")+" saved.")
		}))

		// Rotate re-encrypts every tenant secret under the current primary key.
		// The flash carries only counts (never a value) — a rotate re-seals, it
		// does not reveal.
		mux.HandleFunc("POST /ui/onboarding/secrets/rotate", guard(func(p identity.Principal, w http.ResponseWriter, r *http.Request) {
			if onb.Secrets == nil {
				http.Error(w, "secrets broker not configured (set RUNTIME_SECRETS_KEYS)", http.StatusServiceUnavailable)
				return
			}
			st, err := onb.Secrets.RotateSecrets(r.Context(), p.TenantID)
			if err != nil {
				http.Error(w, "rotate failed", http.StatusInternalServerError)
				return
			}
			flashRedirect(w, r, "Keyring rotated. "+strconv.Itoa(st.Rotated)+" of "+strconv.Itoa(st.Total)+" secrets re-sealed with the current key.")
		}))

		mux.HandleFunc("POST /ui/onboarding/upstreams", guard(func(p identity.Principal, w http.ResponseWriter, r *http.Request) {
			params := controlplane.UpstreamParams{
				Name: r.FormValue("name"), URL: r.FormValue("url"),
				OpenAPI: r.FormValue("openapi"), BaseURL: r.FormValue("base_url"),
				CredSecret: r.FormValue("cred_secret"), CredHeader: r.FormValue("cred_header"),
			}
			if _, err := controlplane.RegisterUpstreamShared(r.Context(), onb.Upstreams, onb.Mutator, onb.CredType, p.TenantID, params); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			flashRedirect(w, r, "Upstream "+params.Name+" registered. Its tools are now federated into the gateway.")
		}))

		mux.HandleFunc("POST /ui/onboarding/upstreams/{id}/delete", guard(func(p identity.Principal, w http.ResponseWriter, r *http.Request) {
			if err := controlplane.RemoveUpstreamShared(r.Context(), onb.Upstreams, onb.Mutator, p.TenantID, r.PathValue("id")); err != nil {
				http.Error(w, "remove failed", http.StatusInternalServerError)
				return
			}
			flashRedirect(w, r, "Upstream "+r.PathValue("id")+" removed.")
		}))

		// Tenant Cedar policies: add/delete gateway authorization rules. Mounted
		// only when the policy engine is on (onb.Policies non-nil). An invalid
		// Cedar text surfaces the parser error via the flash (author fixes it).
		if onb.Policies != nil {
			mux.HandleFunc("POST /ui/onboarding/policies", guard(func(p identity.Principal, w http.ResponseWriter, r *http.Request) {
				name := r.FormValue("name")
				if err := controlplane.RegisterPolicyShared(r.Context(), onb.Policies, p.TenantID, name, r.FormValue("cedar_text")); err != nil {
					// Parser/validation errors are the author's to fix: show the
					// message on the page rather than a bare 400.
					flashRedirect(w, r, "Policy rejected: "+err.Error())
					return
				}
				flashRedirect(w, r, "Policy "+name+" saved. It now gates gateway tool calls in this tenant.")
			}))

			mux.HandleFunc("POST /ui/onboarding/policies/{name}/delete", guard(func(p identity.Principal, w http.ResponseWriter, r *http.Request) {
				if err := controlplane.RemovePolicyShared(r.Context(), onb.Policies, p.TenantID, r.PathValue("name")); err != nil {
					http.Error(w, "remove failed", http.StatusInternalServerError)
					return
				}
				flashRedirect(w, r, "Policy "+r.PathValue("name")+" removed.")
			}))
		}

		// Tenant gateway quotas: per-upstream request-rate limits. Mounted only
		// when the quota store is on (onb.Quotas non-nil). The console is
		// tenant-admin-only, so Insert ALWAYS uses the caller's own tenant and
		// never the "*" wildcard (that is a superuser-only CLI capability).
		if onb.Quotas != nil {
			mux.HandleFunc("POST /ui/onboarding/quotas", guard(func(p identity.Principal, w http.ResponseWriter, r *http.Request) {
				upstream := r.FormValue("upstream")
				rate, err := strconv.Atoi(r.FormValue("rate"))
				if upstream == "" || err != nil || rate < 0 {
					http.Error(w, "upstream and non-negative rate required", http.StatusBadRequest)
					return
				}
				if err := onb.Quotas.Insert(r.Context(), quota.Rule{Tenant: p.TenantID, Upstream: upstream, RatePerMin: rate}); err != nil {
					http.Error(w, "save quota failed", http.StatusInternalServerError)
					return
				}
				flashRedirect(w, r, "Quota for upstream "+upstream+" set to "+strconv.Itoa(rate)+"/min.")
			}))

			mux.HandleFunc("POST /ui/onboarding/quotas/delete", guard(func(p identity.Principal, w http.ResponseWriter, r *http.Request) {
				upstream := r.FormValue("upstream")
				if _, err := onb.Quotas.Delete(r.Context(), p.TenantID, upstream); err != nil {
					http.Error(w, "delete quota failed", http.StatusInternalServerError)
					return
				}
				flashRedirect(w, r, "Quota for upstream "+upstream+" removed.")
			}))
		}

		// Golden-set eval sets: add/delete tenant-scoped sets whose cases are pasted
		// as a JSON array into a textarea (mirrors the CLI --file). Mounted only when
		// the eval store is on (onb.EvalStore non-nil). Malformed JSON or a set that
		// fails ValidateSet is a 400 (mirrors the quota bad-rate handling); the
		// tenant is ALWAYS the caller's own, never form-supplied.
		if onb.EvalStore != nil {
			mux.HandleFunc("POST /ui/onboarding/eval-sets", guard(func(p identity.Principal, w http.ResponseWriter, r *http.Request) {
				name := r.FormValue("name")
				var cases []eval.Case
				if err := json.Unmarshal([]byte(r.FormValue("cases")), &cases); err != nil {
					http.Error(w, "cases must be a valid JSON array", http.StatusBadRequest)
					return
				}
				if err := eval.ValidateSet(name, cases); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				if err := onb.EvalStore.PutSet(r.Context(), eval.Set{Tenant: p.TenantID, Name: name, Cases: cases}); err != nil {
					http.Error(w, "save set failed", http.StatusInternalServerError)
					return
				}
				flashRedirect(w, r, "Eval set "+name+" saved ("+strconv.Itoa(len(cases))+" cases).")
			}))

			mux.HandleFunc("POST /ui/onboarding/eval-sets/{name}/delete", guard(func(p identity.Principal, w http.ResponseWriter, r *http.Request) {
				name := r.PathValue("name")
				if _, err := onb.EvalStore.DeleteSet(r.Context(), p.TenantID, name); err != nil {
					http.Error(w, "delete set failed", http.StatusInternalServerError)
					return
				}
				flashRedirect(w, r, "Eval set "+name+" removed.")
			}))
		}

		// Online eval policies: add/delete per-agent sampling policies whose criteria
		// are pasted as a JSON array into a textarea. Mounted only when the policy
		// store is on (onb.EvalPolicies non-nil). A non-numeric rate, malformed JSON,
		// or a policy that fails ValidatePolicy (rate outside 0..100, no criteria,
		// bad scorer/regex) is a 400; the tenant is ALWAYS the caller's own.
		if onb.EvalPolicies != nil {
			mux.HandleFunc("POST /ui/onboarding/eval-policies", guard(func(p identity.Principal, w http.ResponseWriter, r *http.Request) {
				agent := r.FormValue("agent")
				rate, err := strconv.Atoi(r.FormValue("rate"))
				if err != nil {
					http.Error(w, "rate must be 0-100", http.StatusBadRequest)
					return
				}
				var criteria []eval.Criterion
				if err := json.Unmarshal([]byte(r.FormValue("criteria")), &criteria); err != nil {
					http.Error(w, "criteria must be a valid JSON array", http.StatusBadRequest)
					return
				}
				pol := eval.Policy{Tenant: p.TenantID, AgentID: agent, SampleRate: rate, Criteria: criteria}
				if err := eval.ValidatePolicy(pol); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				if err := onb.EvalPolicies.PutPolicy(r.Context(), pol); err != nil {
					http.Error(w, "save policy failed", http.StatusInternalServerError)
					return
				}
				flashRedirect(w, r, "Online eval policy for "+agent+" saved (rate "+strconv.Itoa(rate)+"%).")
			}))

			mux.HandleFunc("POST /ui/onboarding/eval-policies/{agent}/delete", guard(func(p identity.Principal, w http.ResponseWriter, r *http.Request) {
				agent := r.PathValue("agent")
				if _, err := onb.EvalPolicies.DeletePolicy(r.Context(), p.TenantID, agent); err != nil {
					http.Error(w, "delete policy failed", http.StatusInternalServerError)
					return
				}
				flashRedirect(w, r, "Online eval policy for "+agent+" removed.")
			}))
		}

		// Eval runs (observability page): launch a golden-set run against an agent,
		// then drill into its per-case results. Mounted only when the golden-set
		// store is wired. The launch mirrors the /admin/evals/runs create sequence:
		// validate set exists + agent visible, mint a run id, CreateRun, then run
		// asynchronously on the SIGNAL ctx (a run must outlive the request).
		if onb.EvalStore != nil {
			mux.HandleFunc("POST /ui/observability/eval-runs", guard(func(p identity.Principal, w http.ResponseWriter, r *http.Request) {
				set := r.FormValue("set")
				agent := r.FormValue("agent")
				if set == "" || agent == "" {
					http.Error(w, "set and agent required", http.StatusBadRequest)
					return
				}
				// The set must exist under the caller's tenant.
				if _, found, err := onb.EvalStore.GetSet(r.Context(), p.TenantID, set); err != nil {
					http.Error(w, "get set failed", http.StatusInternalServerError)
					return
				} else if !found {
					http.Error(w, "unknown set", http.StatusBadRequest)
					return
				}
				// The agent must be visible to the caller (own tenant, or superuser).
				info, ok := reg.Get(agent)
				if !ok || !(p.Superuser || info.Tenant == p.TenantID) {
					http.Error(w, "unknown or invisible agent", http.StatusBadRequest)
					return
				}
				id, err := controlplane.MintEvalRunID()
				if err != nil {
					http.Error(w, "mint run id failed", http.StatusInternalServerError)
					return
				}
				if err := onb.EvalStore.CreateRun(r.Context(), eval.Run{
					RunID: id, Tenant: p.TenantID, SetName: set, AgentID: agent, Status: eval.StatusPending,
				}); err != nil {
					http.Error(w, "create run failed", http.StatusInternalServerError)
					return
				}
				// Launch on the server signal ctx, never r.Context(): the goroutine
				// must outlive this request. EvalMetrics gives console-launched runs
				// the SAME eval counters as the CLI/admin path (parity); it and the
				// judge are nil-safe in Execute. Default a missing signal ctx to
				// Background so a run can never fire on a nil ctx.
				runCtx := onb.EvalSignalCtx
				if runCtx == nil {
					runCtx = context.Background()
				}
				go eval.Execute(runCtx, onb.EvalStore, onb.EvalInvoker, onb.EvalJudge, id, onb.EvalMetrics)
				observabilityRedirect(w, r)
			}))

			// Per-run results drill-in. GET (not state-changing) so no CSRF, but a
			// principal is required and a cross-tenant run 404s (no oracle).
			mux.HandleFunc("GET /ui/observability/eval-runs/{id}", func(w http.ResponseWriter, r *http.Request) {
				p, ok := controlplane.PrincipalFromContext(r.Context())
				if !ok {
					http.NotFound(w, r)
					return
				}
				run, found, err := onb.EvalStore.GetRun(r.Context(), r.PathValue("id"))
				if err != nil {
					http.Error(w, "get run failed", http.StatusInternalServerError)
					return
				}
				if !found || !(p.Superuser || run.Tenant == p.TenantID) {
					http.NotFound(w, r)
					return
				}
				results, _ := onb.EvalStore.ListResults(r.Context(), run.RunID)
				render(w, "eval-run.html", map[string]any{"Run": run, "Results": results})
			})
		}

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
				flashRedirect(w, r, "Agent "+params.ID+" registered. The control plane is now health-checking and routing to it.")
			}))

			mux.HandleFunc("POST /ui/onboarding/agents/{id}/delete", guard(func(p identity.Principal, w http.ResponseWriter, r *http.Request) {
				if err := controlplane.DeregisterAgentShared(r.Context(), onb.Agents, onb.AgentMgr, p.TenantID, r.PathValue("id")); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				flashRedirect(w, r, "Agent "+r.PathValue("id")+" deregistered. The control plane no longer manages it; its process keeps running on its host.")
			}))

			setAgentEnabled := func(enabled bool, outcome string) http.HandlerFunc {
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
					flashRedirect(w, r, "Agent "+id+" "+outcome)
				})
			}
			mux.HandleFunc("POST /ui/onboarding/agents/{id}/enable", setAgentEnabled(true, "enabled. It is back in routing."))
			mux.HandleFunc("POST /ui/onboarding/agents/{id}/disable", setAgentEnabled(false, "disabled. It is out of routing but still managed; its process keeps running."))

			mux.HandleFunc("POST /ui/onboarding/agents/{id}/restart", guard(func(p identity.Principal, w http.ResponseWriter, r *http.Request) {
				id := r.PathValue("id")
				if !onb.AgentMgr.IsManaged(id) {
					http.Error(w, "agent is not dynamically managed", http.StatusBadRequest)
					return
				}
				onb.AgentMgr.Reattach(id)
				flashRedirect(w, r, "Agent "+id+" re-attached. Its health is being re-checked now.")
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

// observabilityRedirect performs POST-redirect-GET back to the observability
// page after a launch, so a browser refresh does not re-submit the run.
func observabilityRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/ui/observability", http.StatusSeeOther)
}

// splitScopes parses the oauth2 scopes form field, which lets an admin separate
// scopes with commas and/or whitespace. Empty fragments are dropped so a
// trailing comma or double space does not yield a blank scope.
func splitScopes(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
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
