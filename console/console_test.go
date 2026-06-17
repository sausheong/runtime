package console

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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
		Handler(testRegMulti(t), nil, OIDCConfig{}, nil),
		stubAuth{p: identity.Principal{TenantID: "beta", Role: identity.RoleViewer}},
		identity.NewAuthorizer(map[string]string{"a": "alpha", "b": "beta"}),
		nil,
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
		Handler(testRegMulti(t), nil, OIDCConfig{}, nil),
		stubAuth{p: identity.Principal{TenantID: "beta", Role: identity.RoleViewer}},
		identity.NewAuthorizer(map[string]string{"a": "alpha", "b": "beta"}),
		nil,
	)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/ui/agents/a", nil)) // a is alpha's
	if rec.Code != http.StatusNotFound {
		t.Fatalf("beta user GET /ui/agents/a (alpha): code=%d want 404", rec.Code)
	}
}

func TestConsole_OpenModeShowsAll(t *testing.T) {
	// No principal in context (open mode) → all agents visible.
	h := Handler(testRegMulti(t), nil, OIDCConfig{}, nil)
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
	srv := httptest.NewServer(Handler(testReg(t), nil, OIDCConfig{}, nil))
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
	srv := httptest.NewServer(Handler(testReg(t), nil, OIDCConfig{}, nil))
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
	srv := httptest.NewServer(Handler(testReg(t), nil, OIDCConfig{}, nil))
	defer srv.Close()
	resp, _ := http.Get(srv.URL + "/ui/agents/nope")
	if resp.StatusCode != 404 {
		t.Fatalf("unknown agent code=%d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestConsole_PostLoginForbiddenWhenOIDCEnabled(t *testing.T) {
	h := Handler(testReg(t), nil, OIDCConfig{
		Enabled:     true,
		AuthCodeURL: func(state string) string { return "https://idp.example/authorize?state=" + state },
	}, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/ui/login", strings.NewReader("token=svk-abc.def"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST /ui/login with OIDC on: code=%d want 403", rec.Code)
	}
	if c := rec.Result().Cookies(); len(c) != 0 {
		t.Fatalf("no session cookie should be set; got %d", len(c))
	}
}

func TestConsole_PostLoginWorksWhenOIDCDisabled(t *testing.T) {
	h := Handler(testReg(t), nil, OIDCConfig{Enabled: false}, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/ui/login", strings.NewReader("token=svk-abc.def"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /ui/login with OIDC off: code=%d want 303", rec.Code)
	}
}

func TestConsole_LoginShowsPasteWhenOIDCDisabled(t *testing.T) {
	h := Handler(testReg(t), nil, OIDCConfig{Enabled: false}, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/ui/login", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "token") {
		t.Fatalf("paste login expected; code=%d", rec.Code)
	}
}

func TestConsole_LandingAtRoot(t *testing.T) {
	h := Handler(testReg(t), nil, OIDCConfig{
		Enabled:     true,
		AuthCodeURL: func(state string) string { return "https://idp.example/authorize?state=" + state },
	}, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 200 {
		t.Fatalf("landing at /: code=%d want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Sign in with Google") || !strings.Contains(body, "six pillars") {
		t.Fatalf("expected landing hero + Google button at /")
	}
}

func TestConsole_LoginRendersGoogleButtonWhenOIDCEnabled(t *testing.T) {
	h := Handler(testReg(t), nil, OIDCConfig{
		Enabled:     true,
		AuthCodeURL: func(state string) string { return "https://idp.example/authorize?state=" + state },
	}, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/ui/login", nil))
	// OIDC on: render the landing page with a Google sign-in link to the IdP,
	// NOT an instant redirect and NOT a paste-token form.
	if rec.Code != 200 {
		t.Fatalf("oidc login: code=%d want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Sign in with Google") {
		t.Fatalf("expected Google sign-in button in body")
	}
	if !strings.Contains(body, "https://idp.example/authorize") {
		t.Fatalf("expected IdP authorize URL in body")
	}
	if strings.Contains(body, `name="token"`) {
		t.Fatalf("token form must not show when OIDC is enabled")
	}
}

// validCallbackReq builds a /ui/callback request whose ?state= matches the
// rt_oauth_state cookie — i.e. a request that passes the login-CSRF check.
func validCallbackReq(code string) *http.Request {
	const st = "valid-state-token"
	r := httptest.NewRequest("GET", "/ui/callback?code="+code+"&state="+st, nil)
	r.AddCookie(&http.Cookie{Name: oauthStateCookie, Value: st})
	return r
}

func TestConsole_CallbackSetsCookie(t *testing.T) {
	h := Handler(testReg(t), nil, OIDCConfig{
		Enabled:  true,
		Exchange: func(_ context.Context, code string) (string, error) { return "id-token-" + code, nil },
	}, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, validCallbackReq("abc"))
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
	h := Handler(testReg(t), nil, OIDCConfig{
		Enabled:  true,
		Exchange: func(_ context.Context, code string) (string, error) { return "", errors.New("boom") },
	}, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, validCallbackReq("abc"))
	if rec.Code != 401 {
		t.Fatalf("exchange error: code=%d want 401", rec.Code)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == "runtime_token" {
			t.Fatal("must not set cookie on failed exchange")
		}
	}
}

func TestConsole_CallbackRejectsBadState(t *testing.T) {
	exchanged := false
	h := Handler(testReg(t), nil, OIDCConfig{
		Enabled:  true,
		Exchange: func(_ context.Context, code string) (string, error) { exchanged = true; return "id", nil },
	}, nil)

	// (a) state present in query but no matching cookie → 400, code never spent.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/ui/callback?code=abc&state=attacker", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("no-cookie callback: code=%d want 400", rec.Code)
	}

	// (b) cookie present but query state differs (forged callback) → 400.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/ui/callback?code=abc&state=attacker", nil)
	req.AddCookie(&http.Cookie{Name: oauthStateCookie, Value: "victim-real-state"})
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("mismatched-state callback: code=%d want 400", rec.Code)
	}

	if exchanged {
		t.Fatal("Exchange must not run when state is invalid (no code spent)")
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == "runtime_token" {
			t.Fatal("must not set session cookie on invalid state")
		}
	}
}

func TestConsole_LoginSetsStateCookieMatchingURL(t *testing.T) {
	// The state embedded in the authorize URL must equal the state cookie set on
	// the same response — otherwise the callback check can never pass.
	var urlState string
	h := Handler(testReg(t), nil, OIDCConfig{
		Enabled:     true,
		AuthCodeURL: func(state string) string { urlState = state; return "https://idp.example/auth?state=" + state },
	}, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/ui/login", nil))
	if urlState == "" {
		t.Fatal("AuthCodeURL received empty state")
	}
	var cookieState string
	for _, c := range rec.Result().Cookies() {
		if c.Name == oauthStateCookie {
			cookieState = c.Value
		}
	}
	if cookieState != urlState {
		t.Fatalf("state cookie %q != URL state %q", cookieState, urlState)
	}
}

func TestConsole_CallbackNoExchangeIs400(t *testing.T) {
	// OIDCConfig with no Exchange func (e.g. discovery failed) → 400, no cookie.
	h := Handler(testReg(t), nil, OIDCConfig{Enabled: true}, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/ui/callback?code=abc", nil))
	if rec.Code != 400 {
		t.Fatalf("no exchange configured: code=%d want 400", rec.Code)
	}
}

func TestAgentPageHasActivityCard(t *testing.T) {
	h := Handler(obsTestReg(t), nil, OIDCConfig{}, nil) // open mode, nil store
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/ui/agents/a", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /ui/agents/a: code=%d want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Activity") {
		t.Fatal("agent page missing Activity card heading")
	}
	if !strings.Contains(body, "No recent activity yet.") {
		t.Fatal("agent page missing empty-state for activity (registry addrs unreachable → empty feed)")
	}
}

// multiAdmin embeds fakeAdmin2 (a full AdminStore) but overrides UsersBySubject
// to return a fixed multi-membership set — fakeAdmin2 itself stores one row per
// subject, which can't model a subject belonging to multiple tenants.
type multiAdmin struct {
	*fakeAdmin2
	memberships []identity.UserRow
}

func (m *multiAdmin) UsersBySubject(_ context.Context, _ string) ([]identity.UserRow, error) {
	return m.memberships, nil
}

// consoleWithMemberships builds a console whose Onboarding.Admin reports the
// given memberships for any subject. Mirrors newTestConsoleWithAdmin's dep set.
func consoleWithMemberships(rows []identity.UserRow) http.Handler {
	admin := &multiAdmin{fakeAdmin2: &fakeAdmin2{}, memberships: rows}
	deps := &Onboarding{Upstreams: &fakeUpstreamStore2{}, Mutator: &fakeMut2{}, Admin: admin, Secrets: &fakeSec2{}}
	return Handler(nil, nil, OIDCConfig{}, deps)
}

// pickerReq builds a request carrying a session cookie and a principal WITH a
// Subject (adminReq's principal has no Subject), so the picker handlers and CSRF
// (bound to the runtime_token value) both work.
func pickerReq(method, path string, body url.Values) *http.Request {
	var r *http.Request
	if body != nil {
		r = httptest.NewRequest(method, path, strings.NewReader(body.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	r.AddCookie(&http.Cookie{Name: "runtime_token", Value: "sess-1"})
	p := identity.Principal{Subject: "alice@corp", Kind: identity.KindOIDC}
	return r.WithContext(controlplane.WithPrincipal(r.Context(), p))
}

// csrfFor fetches the picker page (multi-membership) via pickerReq and extracts a
// CSRF token bound to session "sess-1", valid for any pickerReq POST.
func csrfFor(t *testing.T, h http.Handler) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, pickerReq("GET", "/ui/select-tenant", nil))
	body := rec.Body.String()
	const marker = `name="csrf_token" value="`
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("no csrf_token field in picker page: %s", body)
	}
	rest := body[i+len(marker):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		t.Fatal("malformed csrf_token field")
	}
	return rest[:j]
}

// cookieSet reports whether the recorder set a cookie with the given name and value.
func cookieSet(rec *httptest.ResponseRecorder, name, val string) bool {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name && c.Value == val {
			return true
		}
	}
	return false
}

func TestSelectTenant_GET_ListsMemberships(t *testing.T) {
	h := consoleWithMemberships([]identity.UserRow{
		{TenantID: "alpha", Subject: "alice@corp", Role: identity.RoleAdmin},
		{TenantID: "beta", Subject: "alice@corp", Role: identity.RoleViewer},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, pickerReq("GET", "/ui/select-tenant", nil))
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, "alpha") || !strings.Contains(body, "beta") {
		t.Fatalf("picker missing tenants: code=%d body=%s", rec.Code, body)
	}
}

func TestSelectTenant_GET_SingleMembershipAutoRedirects(t *testing.T) {
	h := consoleWithMemberships([]identity.UserRow{
		{TenantID: "alpha", Subject: "alice@corp", Role: identity.RoleAdmin},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, pickerReq("GET", "/ui/select-tenant", nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("single membership: code=%d want 303", rec.Code)
	}
	if !cookieSet(rec, identity.TenantCookieName, "alpha") {
		t.Fatalf("single membership should set tenant cookie to alpha")
	}
}

func TestSelectTenant_POST_SetsCookieAndRedirects(t *testing.T) {
	rows := []identity.UserRow{
		{TenantID: "alpha", Subject: "alice@corp", Role: identity.RoleAdmin},
		{TenantID: "beta", Subject: "alice@corp", Role: identity.RoleViewer},
	}
	h := consoleWithMemberships(rows)
	tok := csrfFor(t, h)
	form := url.Values{"tenant": {"beta"}, "csrf_token": {tok}}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, pickerReq("POST", "/ui/select-tenant", form))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("code=%d want 303", rec.Code)
	}
	if !cookieSet(rec, identity.TenantCookieName, "beta") {
		t.Fatalf("runtime_tenant=beta not set: %v", rec.Result().Cookies())
	}
}

func TestSelectTenant_POST_RejectsNonMember(t *testing.T) {
	h := consoleWithMemberships([]identity.UserRow{
		{TenantID: "alpha", Subject: "alice@corp", Role: identity.RoleAdmin},
		{TenantID: "beta", Subject: "alice@corp", Role: identity.RoleViewer},
	})
	tok := csrfFor(t, h)
	form := url.Values{"tenant": {"gamma"}, "csrf_token": {tok}}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, pickerReq("POST", "/ui/select-tenant", form))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-member tenant: code=%d want 403", rec.Code)
	}
}
