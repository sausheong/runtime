package console

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sausheong/runtime/controlplane"
	"github.com/sausheong/runtime/internal/config"
	"github.com/sausheong/runtime/internal/identity"
)

func testRegMulti(t *testing.T) *controlplane.Registry {
	t.Helper()
	cfg := &config.Config{Agents: []config.AgentConfig{
		{ID: "a", Name: "AlphaAgent", Model: "m", ListenAddr: "127.0.0.1:9001", Tenant: "alpha"},
		{ID: "b", Name: "BetaAgent", Model: "m", ListenAddr: "127.0.0.1:9002", Tenant: "beta"},
	}}
	return controlplane.NewRegistry(cfg, "./agentd", "dsn")
}

// stubAuth is a minimal authenticator that always returns a fixed principal,
// letting tests drive the console through the real IdentityMiddleware.
type stubAuth struct{ p identity.Principal }

func (s stubAuth) Authenticate(_ context.Context, _ *http.Request) (identity.Principal, error) {
	return s.p, nil
}

func TestConsole_OverviewFiltersByTenant(t *testing.T) {
	h := controlplane.IdentityMiddleware(
		Handler(testRegMulti(t), OIDCConfig{}),
		stubAuth{p: identity.Principal{TenantID: "beta", Role: identity.RoleViewer}},
		identity.NewAuthorizer(map[string]string{"a": "alpha", "b": "beta"}),
	)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/ui", nil))
	body := rec.Body.String()
	if strings.Contains(body, "AlphaAgent") {
		t.Error("beta user must NOT see AlphaAgent in overview")
	}
	if !strings.Contains(body, "BetaAgent") {
		t.Error("beta user should see BetaAgent")
	}
}

func TestConsole_AgentPageCrossTenant404(t *testing.T) {
	h := controlplane.IdentityMiddleware(
		Handler(testRegMulti(t), OIDCConfig{}),
		stubAuth{p: identity.Principal{TenantID: "beta", Role: identity.RoleViewer}},
		identity.NewAuthorizer(map[string]string{"a": "alpha", "b": "beta"}),
	)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/ui/agents/a", nil)) // a is alpha's
	if rec.Code != http.StatusNotFound {
		t.Fatalf("beta user GET /ui/agents/a (alpha): code=%d want 404", rec.Code)
	}
}

func TestConsole_OpenModeShowsAll(t *testing.T) {
	// No principal in context (open mode) → all agents visible.
	h := Handler(testRegMulti(t), OIDCConfig{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/ui", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "AlphaAgent") || !strings.Contains(body, "BetaAgent") {
		t.Error("open mode should show all agents")
	}
}

func testReg(t *testing.T) *controlplane.Registry {
	t.Helper()
	cfg := &config.Config{Agents: []config.AgentConfig{
		{ID: "support", Name: "Support", Model: "test/scripted", ListenAddr: "127.0.0.1:9001", Tenant: "alpha"},
	}}
	return controlplane.NewRegistry(cfg, "/bin/agentd", "dsn")
}

func TestConsole_Overview(t *testing.T) {
	srv := httptest.NewServer(Handler(testReg(t), OIDCConfig{}))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/ui")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(body), "Support") {
		t.Fatalf("overview missing agent; code=%d body=%q", resp.StatusCode, body)
	}
}

func TestConsole_LoginPage(t *testing.T) {
	srv := httptest.NewServer(Handler(testReg(t), OIDCConfig{}))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/ui/login")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("login page code=%d", resp.StatusCode)
	}
}

func TestConsole_UnknownAgent404(t *testing.T) {
	srv := httptest.NewServer(Handler(testReg(t), OIDCConfig{}))
	defer srv.Close()
	resp, _ := http.Get(srv.URL + "/ui/agents/nope")
	if resp.StatusCode != 404 {
		t.Fatalf("unknown agent code=%d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestConsole_LoginShowsPasteWhenOIDCDisabled(t *testing.T) {
	h := Handler(testReg(t), OIDCConfig{Enabled: false})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/ui/login", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "token") {
		t.Fatalf("paste login expected; code=%d", rec.Code)
	}
}

func TestConsole_LoginRedirectsToIdPWhenEnabled(t *testing.T) {
	h := Handler(testReg(t), OIDCConfig{
		Enabled:     true,
		AuthCodeURL: func(state string) string { return "https://idp.example/authorize?state=" + state },
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/ui/login", nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("oidc login: code=%d want 303", rec.Code)
	}
	if !strings.HasPrefix(rec.Header().Get("Location"), "https://idp.example/authorize") {
		t.Fatalf("redirect to %q", rec.Header().Get("Location"))
	}
}

func TestConsole_CallbackSetsCookie(t *testing.T) {
	h := Handler(testReg(t), OIDCConfig{
		Enabled:  true,
		Exchange: func(_ context.Context, code string) (string, error) { return "id-token-" + code, nil },
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/ui/callback?code=abc&state=s", nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("callback: code=%d want 303", rec.Code)
	}
	var found bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == "runtime_token" && c.Value == "id-token-abc" {
			found = true
		}
	}
	if !found {
		t.Fatal("callback did not set runtime_token cookie to the id token")
	}
}

func TestConsole_CallbackExchangeErrorIs401(t *testing.T) {
	h := Handler(testReg(t), OIDCConfig{
		Enabled:  true,
		Exchange: func(_ context.Context, code string) (string, error) { return "", errors.New("boom") },
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/ui/callback?code=abc", nil))
	if rec.Code != 401 {
		t.Fatalf("exchange error: code=%d want 401", rec.Code)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == "runtime_token" {
			t.Fatal("must not set cookie on failed exchange")
		}
	}
}

func TestConsole_CallbackNoExchangeIs400(t *testing.T) {
	// OIDCConfig with no Exchange func (e.g. discovery failed) → 400, no cookie.
	h := Handler(testReg(t), OIDCConfig{Enabled: true})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/ui/callback?code=abc", nil))
	if rec.Code != 400 {
		t.Fatalf("no exchange configured: code=%d want 400", rec.Code)
	}
}
