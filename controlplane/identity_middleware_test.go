package controlplane

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sausheong/runtime/internal/identity"
)

// stubAuthndr lets the test inject the Principal/err the Authenticator returns.
type stubAuthndr struct {
	p   identity.Principal
	err error
}

func (s stubAuthndr) Authenticate(_ context.Context, _ *http.Request) (identity.Principal, error) {
	return s.p, s.err
}

func okPrincipalHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := PrincipalFromContext(r.Context())
		w.Header().Set("X-Tenant", p.TenantID)
		w.WriteHeader(200)
	})
}

func testAZ() *identity.Authorizer {
	return identity.NewAuthorizer(map[string]string{"a1": "alpha", "b1": "beta"})
}

func TestIdentityMW_UnauthenticatedIs401(t *testing.T) {
	mw := IdentityMiddleware(okPrincipalHandler(), stubAuthndr{err: identity.ErrUnauthenticated}, testAZ(), nil)
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, httptest.NewRequest("GET", "/agents/a1/sessions", nil))
	if rec.Code != 401 {
		t.Fatalf("code=%d want 401", rec.Code)
	}
}

func TestIdentityMW_NotProvisionedIs403(t *testing.T) {
	mw := IdentityMiddleware(okPrincipalHandler(), stubAuthndr{err: identity.ErrNotProvisioned}, testAZ(), nil)
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, httptest.NewRequest("GET", "/agents/a1/sessions", nil))
	if rec.Code != 403 {
		t.Fatalf("code=%d want 403", rec.Code)
	}
}

func TestIdentityMW_CrossTenantIs404(t *testing.T) {
	mw := IdentityMiddleware(okPrincipalHandler(),
		stubAuthndr{p: identity.Principal{TenantID: "alpha", Role: identity.RoleOperator}}, testAZ(), nil)
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, httptest.NewRequest("GET", "/agents/b1/sessions", nil))
	if rec.Code != 404 {
		t.Fatalf("code=%d want 404", rec.Code)
	}
}

func TestIdentityMW_ViewerInvokeIs403(t *testing.T) {
	mw := IdentityMiddleware(okPrincipalHandler(),
		stubAuthndr{p: identity.Principal{TenantID: "alpha", Role: identity.RoleViewer}}, testAZ(), nil)
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, httptest.NewRequest("POST", "/agents/a1/sessions", nil))
	if rec.Code != 403 {
		t.Fatalf("viewer POST: code=%d want 403", rec.Code)
	}
}

func TestIdentityMW_OperatorInvokeOK(t *testing.T) {
	mw := IdentityMiddleware(okPrincipalHandler(),
		stubAuthndr{p: identity.Principal{TenantID: "alpha", Role: identity.RoleOperator}}, testAZ(), nil)
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, httptest.NewRequest("POST", "/agents/a1/sessions", nil))
	if rec.Code != 200 || rec.Header().Get("X-Tenant") != "alpha" {
		t.Fatalf("operator POST: code=%d tenant=%q", rec.Code, rec.Header().Get("X-Tenant"))
	}
}

func TestIdentityMW_ConsoleOIDCOnly_BouncesServiceKeyFromUI(t *testing.T) {
	// A valid service-key principal authenticates, but on /ui it must be bounced
	// to the Google sign-in (303 -> /ui/login), not allowed into the console.
	mw := IdentityMiddlewareConsoleOIDCOnly(okPrincipalHandler(),
		stubAuthndr{p: identity.Principal{TenantID: "alpha", Role: identity.RoleAdmin, Kind: identity.KindServiceKey}},
		testAZ(), nil)
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, httptest.NewRequest("GET", "/ui", nil))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/ui/login" {
		t.Fatalf("service-key on /ui: code=%d loc=%q want 303 -> /ui/login", rec.Code, rec.Header().Get("Location"))
	}
}

func TestIdentityMW_ConsoleOIDCOnly_AllowsOIDCOnUI(t *testing.T) {
	mw := IdentityMiddlewareConsoleOIDCOnly(okPrincipalHandler(),
		stubAuthndr{p: identity.Principal{TenantID: "alpha", Role: identity.RoleViewer, Kind: identity.KindOIDC}},
		testAZ(), nil)
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, httptest.NewRequest("GET", "/ui", nil))
	if rec.Code != 200 {
		t.Fatalf("oidc on /ui: code=%d want 200", rec.Code)
	}
}

func TestIdentityMW_ConsoleOIDCOnly_ServiceKeyStillWorksOnAPI(t *testing.T) {
	// The OIDC-only restriction is console-scoped: a service key must still drive
	// the API (here, an authorized read).
	mw := IdentityMiddlewareConsoleOIDCOnly(okPrincipalHandler(),
		stubAuthndr{p: identity.Principal{TenantID: "alpha", Role: identity.RoleOperator, Kind: identity.KindServiceKey}},
		testAZ(), nil)
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, httptest.NewRequest("POST", "/agents/a1/sessions", nil))
	if rec.Code != 200 {
		t.Fatalf("service-key on API: code=%d want 200", rec.Code)
	}
}

func TestIdentityMW_HealthzExempt(t *testing.T) {
	mw := IdentityMiddleware(okPrincipalHandler(), stubAuthndr{err: identity.ErrUnauthenticated}, testAZ(), nil)
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Fatalf("healthz must be exempt: code=%d", rec.Code)
	}
}

func TestIdentityMW_RootExempt(t *testing.T) {
	// "/" is the public landing page; an unauthenticated visitor must reach the
	// inner handler, not get a 401.
	mw := IdentityMiddleware(okPrincipalHandler(), stubAuthndr{err: identity.ErrUnauthenticated}, testAZ(), nil)
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 200 {
		t.Fatalf("root must be exempt: code=%d", rec.Code)
	}
}

func TestIdentityMW_CallbackExempt(t *testing.T) {
	// The OIDC callback arrives with ?code=... and NO cookie yet (the cookie is
	// set by the callback handler). If the middleware gated it, an unauthenticated
	// callback would redirect to /ui/login, which (OIDC on) bounces back to the
	// IdP — an infinite loop. The callback must reach the inner handler.
	mw := IdentityMiddleware(okPrincipalHandler(), stubAuthndr{err: identity.ErrUnauthenticated}, testAZ(), nil)
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, httptest.NewRequest("GET", "/ui/callback?code=abc", nil))
	if rec.Code != 200 {
		t.Fatalf("callback must be exempt: code=%d", rec.Code)
	}
}

func TestIdentityMW_UIRedirectsWhenUnauthenticated(t *testing.T) {
	mw := IdentityMiddleware(okPrincipalHandler(), stubAuthndr{err: identity.ErrUnauthenticated}, testAZ(), nil)
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, httptest.NewRequest("GET", "/ui/agents/x", nil))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/ui/login" {
		t.Fatalf("ui redirect: code=%d loc=%q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestIdentityMW_OnRejectFiresForUnauthenticated(t *testing.T) {
	var got []int
	mw := IdentityMiddleware(okPrincipalHandler(), stubAuthndr{err: identity.ErrUnauthenticated}, testAZ(),
		func(status int) { got = append(got, status) })
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, httptest.NewRequest("GET", "/agents/a1/sessions", nil))
	if rec.Code != 401 {
		t.Fatalf("code=%d want 401", rec.Code)
	}
	if len(got) != 1 || got[0] != 401 {
		t.Fatalf("onReject calls=%v want exactly one 401", got)
	}
}

func TestActionForRequest(t *testing.T) {
	cases := []struct {
		method, path string
		want         identity.Action
	}{
		{"GET", "/agents/a1/sessions", identity.ActionRead},
		{"GET", "/agents/a1/sessions/s1/stream", identity.ActionRead},
		{"POST", "/agents/a1/sessions", identity.ActionInvoke},
		{"GET", "/agents", identity.ActionRead},
	}
	for _, c := range cases {
		if got := actionForRequest(c.method, c.path); got != c.want {
			t.Errorf("%s %s: action=%s want %s", c.method, c.path, got, c.want)
		}
	}
}

func TestAgentIDFromPath(t *testing.T) {
	if id := agentIDFromPath("/agents/a1/sessions/s2"); id != "a1" {
		t.Errorf("agentIDFromPath = %q want a1", id)
	}
	if id := agentIDFromPath("/agents"); id != "" {
		t.Errorf("bare /agents should yield empty id, got %q", id)
	}
}

func TestIdentityMW_TraversalCannotBypassExemption(t *testing.T) {
	// A ".."-laden path that prefix-matches /ui/static must NOT be treated as
	// exempt: it cleans to an agent path and must be authenticated.
	mw := IdentityMiddleware(okPrincipalHandler(), stubAuthndr{err: identity.ErrUnauthenticated}, testAZ(), nil)
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, httptest.NewRequest("GET", "/ui/static/../../agents/a1/sessions", nil))
	// cleanPath = /agents/a1/sessions ; not /ui ; unauthenticated → 401 (not 200, not redirect).
	if rec.Code != 401 {
		t.Fatalf("traversal bypass: code=%d want 401", rec.Code)
	}
}

func TestActionForRequest_NonGetIsInvoke(t *testing.T) {
	// Defense-in-depth: an unknown mutating verb must be invoke, not read.
	if actionForRequest("DELETE", "/agents/a1/sessions/s1") != identity.ActionInvoke {
		t.Fatal("DELETE must classify as invoke")
	}
	if actionForRequest("HEAD", "/agents/a1/sessions") != identity.ActionRead {
		t.Fatal("HEAD must classify as read")
	}
}
