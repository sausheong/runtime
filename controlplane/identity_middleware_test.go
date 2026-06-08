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
	mw := IdentityMiddleware(okPrincipalHandler(), stubAuthndr{err: identity.ErrUnauthenticated}, testAZ())
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, httptest.NewRequest("GET", "/agents/a1/sessions", nil))
	if rec.Code != 401 {
		t.Fatalf("code=%d want 401", rec.Code)
	}
}

func TestIdentityMW_NotProvisionedIs403(t *testing.T) {
	mw := IdentityMiddleware(okPrincipalHandler(), stubAuthndr{err: identity.ErrNotProvisioned}, testAZ())
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, httptest.NewRequest("GET", "/agents/a1/sessions", nil))
	if rec.Code != 403 {
		t.Fatalf("code=%d want 403", rec.Code)
	}
}

func TestIdentityMW_CrossTenantIs404(t *testing.T) {
	mw := IdentityMiddleware(okPrincipalHandler(),
		stubAuthndr{p: identity.Principal{TenantID: "alpha", Role: identity.RoleOperator}}, testAZ())
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, httptest.NewRequest("GET", "/agents/b1/sessions", nil))
	if rec.Code != 404 {
		t.Fatalf("code=%d want 404", rec.Code)
	}
}

func TestIdentityMW_ViewerInvokeIs403(t *testing.T) {
	mw := IdentityMiddleware(okPrincipalHandler(),
		stubAuthndr{p: identity.Principal{TenantID: "alpha", Role: identity.RoleViewer}}, testAZ())
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, httptest.NewRequest("POST", "/agents/a1/sessions", nil))
	if rec.Code != 403 {
		t.Fatalf("viewer POST: code=%d want 403", rec.Code)
	}
}

func TestIdentityMW_OperatorInvokeOK(t *testing.T) {
	mw := IdentityMiddleware(okPrincipalHandler(),
		stubAuthndr{p: identity.Principal{TenantID: "alpha", Role: identity.RoleOperator}}, testAZ())
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, httptest.NewRequest("POST", "/agents/a1/sessions", nil))
	if rec.Code != 200 || rec.Header().Get("X-Tenant") != "alpha" {
		t.Fatalf("operator POST: code=%d tenant=%q", rec.Code, rec.Header().Get("X-Tenant"))
	}
}

func TestIdentityMW_HealthzExempt(t *testing.T) {
	mw := IdentityMiddleware(okPrincipalHandler(), stubAuthndr{err: identity.ErrUnauthenticated}, testAZ())
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Fatalf("healthz must be exempt: code=%d", rec.Code)
	}
}

func TestIdentityMW_UIRedirectsWhenUnauthenticated(t *testing.T) {
	mw := IdentityMiddleware(okPrincipalHandler(), stubAuthndr{err: identity.ErrUnauthenticated}, testAZ())
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, httptest.NewRequest("GET", "/ui/agents/x", nil))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/ui/login" {
		t.Fatalf("ui redirect: code=%d loc=%q", rec.Code, rec.Header().Get("Location"))
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
