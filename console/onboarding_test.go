package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sausheong/runtime/controlplane"
	"github.com/sausheong/runtime/internal/config"
	"github.com/sausheong/runtime/internal/gateway"
	"github.com/sausheong/runtime/internal/identity"
)

// fakeUpstreamStore2 implements controlplane.UpstreamStore.
type fakeUpstreamStore2 struct {
	rows []gateway.UpstreamRow
}

func (f *fakeUpstreamStore2) InsertUpstream(ctx context.Context, r gateway.UpstreamRow) error {
	f.rows = append(f.rows, r)
	return nil
}
func (f *fakeUpstreamStore2) ListUpstreams(ctx context.Context, tenant string) ([]gateway.UpstreamRow, error) {
	return f.rows, nil
}
func (f *fakeUpstreamStore2) GetUpstream(ctx context.Context, id string) (gateway.UpstreamRow, bool, error) {
	for _, r := range f.rows {
		if r.ID == id {
			return r, true, nil
		}
	}
	return gateway.UpstreamRow{}, false, nil
}
func (f *fakeUpstreamStore2) DeleteUpstream(ctx context.Context, tenant, id string) error {
	return nil
}

// fakeMut2 implements controlplane.GatewayMutator.
type fakeMut2 struct{}

func (f *fakeMut2) Add(cfg config.GatewayServer) error            { return nil }
func (f *fakeMut2) Remove(name string)                            {}
func (f *fakeMut2) Status(tenant string) []gateway.UpstreamStatus { return nil }

// fakeAdmin2 implements controlplane.AdminStore. User methods are stateful so
// tests can observe add/update/remove; the rest are zero-value stubs.
type fakeAdmin2 struct {
	users     map[string]identity.UserRow // subject -> row
	userOrder []string                    // subjects in insertion order
}

func (f *fakeAdmin2) CreateTenant(ctx context.Context, id, name string) error { return nil }
func (f *fakeAdmin2) TenantExists(ctx context.Context, id string) (bool, error) {
	return false, nil
}
func (f *fakeAdmin2) UpsertUser(ctx context.Context, tenantID, subject string, role identity.Role) error {
	if f.users == nil {
		f.users = map[string]identity.UserRow{}
	}
	if _, exists := f.users[subject]; !exists {
		f.userOrder = append(f.userOrder, subject)
	}
	f.users[subject] = identity.UserRow{TenantID: tenantID, Subject: subject, Role: role}
	return nil
}
func (f *fakeAdmin2) DeleteUser(ctx context.Context, tenantID, subject string) error {
	delete(f.users, subject)
	for i, s := range f.userOrder {
		if s == subject {
			f.userOrder = append(f.userOrder[:i], f.userOrder[i+1:]...)
			break
		}
	}
	return nil
}
func (f *fakeAdmin2) ListUsers(ctx context.Context, tenantID string) ([]identity.UserRow, error) {
	var out []identity.UserRow
	for _, s := range f.userOrder {
		if f.users[s].TenantID == tenantID {
			out = append(out, f.users[s])
		}
	}
	return out, nil
}
func (f *fakeAdmin2) InsertServiceKey(ctx context.Context, id, tenantID, hash string, role identity.Role, label string) error {
	return nil
}
func (f *fakeAdmin2) RevokeKey(ctx context.Context, tenantID, id string) error { return nil }
func (f *fakeAdmin2) ListKeys(ctx context.Context, tenantID string) ([]identity.KeyRow, error) {
	return nil, nil
}
func (f *fakeAdmin2) ListTenants(ctx context.Context) ([]identity.TenantRow, error) {
	return nil, nil
}
func (f *fakeAdmin2) InsertRegistrationToken(ctx context.Context, tokenID, agentID, hash string) error {
	return nil
}
func (f *fakeAdmin2) ListRegistrationTokens(ctx context.Context) ([]identity.RegTokenRow, error) {
	return nil, nil
}
func (f *fakeAdmin2) RevokeRegistrationToken(ctx context.Context, tokenID string) error { return nil }

// fakeSec2 implements controlplane.SecretAdmin with zero-value stubs.
type fakeSec2 struct{}

func (f *fakeSec2) SetSecret(ctx context.Context, tenant, name, plaintext string) error { return nil }
func (f *fakeSec2) ListSecretNames(ctx context.Context, tenant string) ([]identity.SecretMeta, error) {
	return nil, nil
}
func (f *fakeSec2) DeleteSecret(ctx context.Context, tenant, name string) error { return nil }
func (f *fakeSec2) RotateSecrets(ctx context.Context, tenant string) (identity.RotateStats, error) {
	return identity.RotateStats{}, nil
}

func adminReq(method, path string, body url.Values) *http.Request {
	var r *http.Request
	if body != nil {
		r = httptest.NewRequest(method, path, strings.NewReader(body.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	r.AddCookie(&http.Cookie{Name: "runtime_token", Value: "sess-1"})
	p := identity.Principal{Role: identity.RoleAdmin, TenantID: "t1"}
	return r.WithContext(controlplane.WithPrincipal(r.Context(), p))
}

func newTestConsole() (http.Handler, *fakeUpstreamStore2) {
	h, us, _ := newTestConsoleWithAdmin()
	return h, us
}

func newTestConsoleWithAdmin() (http.Handler, *fakeUpstreamStore2, *fakeAdmin2) {
	us := &fakeUpstreamStore2{}
	admin := &fakeAdmin2{}
	deps := &Onboarding{
		Upstreams: us,
		Mutator:   &fakeMut2{},
		Admin:     admin,
		Secrets:   &fakeSec2{},
	}
	return Handler(nil, OIDCConfig{}, deps), us, admin
}

func TestOnboardingGETRendersForAdmin(t *testing.T) {
	h, _ := newTestConsole()
	r := adminReq("GET", "/ui/onboarding", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("GET onboarding: want 200 got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "csrf_token") {
		t.Fatal("form missing csrf token field")
	}
}

// TestOnboardingNilSecretsNoPanic reproduces I1: identity ON + file-configured
// gateway upstreams + NO secrets keyring builds an Onboarding with Secrets == nil.
// The GET handler must not panic on the secrets listing, and POST secrets must
// fail closed with 503 rather than nil-deref.
func TestOnboardingNilSecretsNoPanic(t *testing.T) {
	deps := &Onboarding{Upstreams: &fakeUpstreamStore2{}, Mutator: &fakeMut2{}, Admin: &fakeAdmin2{}, Secrets: nil}
	h := Handler(nil, OIDCConfig{}, deps)

	// GET must not panic and returns 200 (page is still useful: mint keys,
	// register credential-less upstreams).
	r := adminReq("GET", "/ui/onboarding", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("GET with nil Secrets: want 200 got %d", w.Code)
	}
	// The credentials section is hidden when no broker is configured.
	if strings.Contains(w.Body.String(), "/ui/onboarding/secrets") {
		t.Fatal("credentials form should be hidden when Secrets is nil")
	}

	// POST secrets WITHOUT a csrf token must 403 (proves the route exists and the
	// guard runs before any nil deref — i.e. no panic).
	rp := adminReq("POST", "/ui/onboarding/secrets", url.Values{"name": {"X"}, "value": {"y"}})
	wp := httptest.NewRecorder()
	h.ServeHTTP(wp, rp)
	if wp.Code != http.StatusForbidden {
		t.Fatalf("POST secrets without csrf (nil Secrets): want 403 got %d", wp.Code)
	}

	// POST secrets WITH a valid csrf token must 503 (broker absent), not panic.
	token := issuedCSRF(t, h)
	rp2 := adminReq("POST", "/ui/onboarding/secrets", url.Values{"csrf_token": {token}, "name": {"X"}, "value": {"y"}})
	wp2 := httptest.NewRecorder()
	h.ServeHTTP(wp2, rp2)
	if wp2.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST secrets with csrf (nil Secrets): want 503 got %d", wp2.Code)
	}
}

// issuedCSRF fetches the onboarding page as the test admin (session cookie
// "sess-1", matching adminReq) and extracts the CSRF token from a hidden input.
// The token is HMAC of the session value, so it round-trips with adminReq's
// runtime_token cookie.
func issuedCSRF(t *testing.T, h http.Handler) string {
	t.Helper()
	r := adminReq("GET", "/ui/onboarding", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	body := w.Body.String()
	const marker = `name="csrf_token" value="`
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatal("no csrf_token field found in onboarding page")
	}
	rest := body[i+len(marker):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		t.Fatal("malformed csrf_token field")
	}
	return rest[:j]
}

func TestOnboardingGETRendersUsers(t *testing.T) {
	h, _, admin := newTestConsoleWithAdmin()
	_ = admin.UpsertUser(context.Background(), "t1", "alice@example.com", identity.RoleAdmin)

	r := adminReq("GET", "/ui/onboarding", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("GET onboarding: want 200 got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "alice@example.com") {
		t.Fatal("users table missing the seeded user")
	}
	if !strings.Contains(body, `action="/ui/onboarding/users"`) {
		t.Fatal("add-user form missing")
	}
}

func TestOnboardingPOSTRequiresCSRF(t *testing.T) {
	h, _ := newTestConsole()
	form := url.Values{"name": {"orders"}, "url": {"http://x"}, "transport": {"http"}}
	r := adminReq("POST", "/ui/onboarding/upstreams", form)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("POST without csrf: want 403 got %d", w.Code)
	}
}
