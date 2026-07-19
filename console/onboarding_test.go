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
	keys      []identity.KeyRow           // returned by ListKeys
	revoked   []string                    // ids passed to RevokeKey
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
func (f *fakeAdmin2) UsersBySubject(ctx context.Context, subject string) ([]identity.UserRow, error) {
	if u, ok := f.users[subject]; ok {
		return []identity.UserRow{u}, nil
	}
	return nil, nil
}
func (f *fakeAdmin2) InsertServiceKey(ctx context.Context, id, tenantID, hash string, role identity.Role, label string) error {
	return nil
}
func (f *fakeAdmin2) RevokeKey(ctx context.Context, tenantID, id string) error {
	f.revoked = append(f.revoked, id)
	return nil
}
func (f *fakeAdmin2) ListKeys(ctx context.Context, tenantID string) ([]identity.KeyRow, error) {
	return f.keys, nil
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

// fakeSec2 implements controlplane.SecretAdmin with zero-value stubs;
// SetSecret records the names it was called with so tests can assert a
// rejected creation never reached the broker.
type fakeSec2 struct {
	setNames []string
}

func (f *fakeSec2) SetSecret(ctx context.Context, tenant, name, plaintext string) error {
	f.setNames = append(f.setNames, name)
	return nil
}
func (f *fakeSec2) SetOAuth2(ctx context.Context, tenant, name string, cfg identity.OAuth2Config) error {
	f.setNames = append(f.setNames, name)
	return nil
}
func (f *fakeSec2) ListSecretNames(ctx context.Context, tenant string) ([]identity.SecretMeta, error) {
	return nil, nil
}
func (f *fakeSec2) ListSecrets(ctx context.Context, tenant string) ([]identity.SecretMeta, error) {
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
	return Handler(nil, nil, OIDCConfig{}, deps), us, admin
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
	h := Handler(nil, nil, OIDCConfig{}, deps)

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

// TestOnboardingSecretReservedPrefixRejected proves the console creation path
// enforces the same reserved-prefix guard as the /admin/secrets API: a tenant
// secret named RUNTIME_* / DBOS__* would shadow platform control vars at
// spawn (e.g. RUNTIME_AGENT_LIMITS={} ⇒ unlimited), so it is rejected with
// 400 before reaching the broker.
func TestOnboardingSecretReservedPrefixRejected(t *testing.T) {
	sec := &fakeSec2{}
	deps := &Onboarding{Upstreams: &fakeUpstreamStore2{}, Mutator: &fakeMut2{}, Admin: &fakeAdmin2{}, Secrets: sec}
	h := Handler(nil, nil, OIDCConfig{}, deps)
	token := issuedCSRF(t, h)

	for _, name := range []string{"RUNTIME_AGENT_LIMITS", "DBOS__VMID"} {
		r := adminReq("POST", "/ui/onboarding/secrets",
			url.Values{"csrf_token": {token}, "name": {name}, "value": {"{}"}})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("name %s: want 400 got %d", name, w.Code)
		}
		if !strings.Contains(w.Body.String(), "reserved prefix") {
			t.Fatalf("name %s: body %q missing reserved-prefix message", name, w.Body.String())
		}
	}
	if len(sec.setNames) != 0 {
		t.Fatalf("reserved-prefix secret reached the broker: %v", sec.setNames)
	}

	// A non-reserved name still saves (the guard must not over-match).
	r := adminReq("POST", "/ui/onboarding/secrets",
		url.Values{"csrf_token": {token}, "name": {"OPENAI_API_KEY"}, "value": {"sk"}})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("legit secret: want 303 got %d", w.Code)
	}
	if len(sec.setNames) != 1 || sec.setNames[0] != "OPENAI_API_KEY" {
		t.Fatalf("legit secret not stored: %v", sec.setNames)
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

func TestOnboardingAddUser(t *testing.T) {
	h, _, admin := newTestConsoleWithAdmin()
	token := issuedCSRF(t, h)
	form := url.Values{"csrf_token": {token}, "subject": {"bob@example.com"}, "role": {"operator"}}
	r := adminReq("POST", "/ui/onboarding/users", form)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("add user: want 303 got %d", w.Code)
	}
	got, _ := admin.ListUsers(context.Background(), "t1")
	if len(got) != 1 || got[0].Subject != "bob@example.com" || got[0].Role != identity.RoleOperator {
		t.Fatalf("user not upserted: %+v", got)
	}
}

func TestOnboardingAddUserInvalidRole(t *testing.T) {
	h, _, _ := newTestConsoleWithAdmin()
	token := issuedCSRF(t, h)
	form := url.Values{"csrf_token": {token}, "subject": {"x@y.com"}, "role": {"superhero"}}
	r := adminReq("POST", "/ui/onboarding/users", form)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid role: want 400 got %d", w.Code)
	}
}

func TestOnboardingAddUserEmptySubject(t *testing.T) {
	h, _, _ := newTestConsoleWithAdmin()
	token := issuedCSRF(t, h)
	form := url.Values{"csrf_token": {token}, "subject": {""}, "role": {"viewer"}}
	r := adminReq("POST", "/ui/onboarding/users", form)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("empty subject: want 400 got %d", w.Code)
	}
}

func TestOnboardingAddUserRequiresCSRF(t *testing.T) {
	h, _, _ := newTestConsoleWithAdmin()
	form := url.Values{"subject": {"x@y.com"}, "role": {"viewer"}}
	r := adminReq("POST", "/ui/onboarding/users", form)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("no csrf: want 403 got %d", w.Code)
	}
}

func TestOnboardingSelfDemoteRejected(t *testing.T) {
	h, _, _ := newTestConsoleWithAdmin()
	token := issuedCSRF(t, h)
	// adminReq's principal has Subject == "" and Role == admin; demoting "" to
	// viewer is self-demotion. (Empty-subject validation also returns 400 here,
	// which is consistent — both assert 400.)
	form := url.Values{"csrf_token": {token}, "subject": {""}, "role": {"viewer"}}
	r := adminReq("POST", "/ui/onboarding/users", form)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("self-demote: want 400 got %d", w.Code)
	}
}

func TestOnboardingRemoveUser(t *testing.T) {
	h, _, admin := newTestConsoleWithAdmin()
	_ = admin.UpsertUser(context.Background(), "t1", "carol@example.com", identity.RoleViewer)
	token := issuedCSRF(t, h)
	form := url.Values{"csrf_token": {token}}
	r := adminReq("POST", "/ui/onboarding/users/carol@example.com/delete", form)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("remove user: want 303 got %d", w.Code)
	}
	got, _ := admin.ListUsers(context.Background(), "t1")
	if len(got) != 0 {
		t.Fatalf("user not removed: %+v", got)
	}
}

func TestOnboardingRemoveUserRequiresCSRF(t *testing.T) {
	h, _, admin := newTestConsoleWithAdmin()
	_ = admin.UpsertUser(context.Background(), "t1", "carol@example.com", identity.RoleViewer)
	r := adminReq("POST", "/ui/onboarding/users/carol@example.com/delete", url.Values{})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("remove without csrf: want 403 got %d", w.Code)
	}
}

func TestOnboardingRevokeKey(t *testing.T) {
	h, _, admin := newTestConsoleWithAdmin()
	token := issuedCSRF(t, h)
	form := url.Values{"csrf_token": {token}}
	r := adminReq("POST", "/ui/onboarding/keys/svk-abc123/delete", form)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("revoke key: want 303 got %d", w.Code)
	}
	if len(admin.revoked) != 1 || admin.revoked[0] != "svk-abc123" {
		t.Fatalf("RevokeKey not called with svk-abc123: %+v", admin.revoked)
	}
}

func TestOnboardingRevokeKeyRequiresCSRF(t *testing.T) {
	h, _, admin := newTestConsoleWithAdmin()
	r := adminReq("POST", "/ui/onboarding/keys/svk-abc123/delete", url.Values{})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("revoke without csrf: want 403 got %d", w.Code)
	}
	if len(admin.revoked) != 0 {
		t.Fatalf("RevokeKey must not run without csrf: %+v", admin.revoked)
	}
}

func TestOnboardingRevokeKeyRequiresAdmin(t *testing.T) {
	h, _, admin := newTestConsoleWithAdmin()
	token := issuedCSRF(t, h)
	form := url.Values{"csrf_token": {token}}
	r := nonAdminReq("POST", "/ui/onboarding/keys/svk-abc123/delete", form)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-admin revoke: want 403 got %d", w.Code)
	}
	if len(admin.revoked) != 0 {
		t.Fatalf("RevokeKey must not run for non-admin: %+v", admin.revoked)
	}
}

// The keys table renders a Remove button for active keys and a Revoked badge
// Active keys render with a Remove button; revoked keys are hidden entirely
// (they stay auditable via the GET /admin/keys API, just not in the console).
func TestOnboardingKeysTableHidesRevoked(t *testing.T) {
	h, _, admin := newTestConsoleWithAdmin()
	admin.keys = []identity.KeyRow{
		{ID: "svk-active1", TenantID: "t1", Role: identity.RoleAdmin, Label: "live", Revoked: false},
		{ID: "svk-dead1", TenantID: "t1", Role: identity.RoleViewer, Label: "oldlabel", Revoked: true},
	}
	r := adminReq("GET", "/ui/onboarding", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	body := w.Body.String()
	if !strings.Contains(body, "/ui/onboarding/keys/svk-active1/delete") {
		t.Fatal("active key must have a Remove form")
	}
	if strings.Contains(body, "svk-dead1") || strings.Contains(body, "oldlabel") {
		t.Fatal("revoked key must not appear in the table at all")
	}
}

// NOTE: self-removal via an EMPTY path segment (/ui/onboarding/users//delete) is
// not testable — Go's ServeMux collapses the "//" and 301-redirects before the
// handler runs, so the empty {subject} never reaches the guard. The self-removal
// guard is exercised with a non-empty subject in TestOnboardingSelfRemoveNonEmptySubject
// (Task 5), which can set the principal's subject to match the path.

// adminReqAs is adminReq with a chosen principal subject (and admin role), so
// tests can exercise the self-lockout guards with a non-empty subject.
func adminReqAs(method, path, subject string, body url.Values) *http.Request {
	r := adminReq(method, path, body)
	p := identity.Principal{Role: identity.RoleAdmin, TenantID: "t1", Subject: subject}
	return r.WithContext(controlplane.WithPrincipal(r.Context(), p))
}

// nonAdminReq builds a request whose principal is a viewer (not admin).
func nonAdminReq(method, path string, body url.Values) *http.Request {
	r := adminReq(method, path, body)
	p := identity.Principal{Role: identity.RoleViewer, TenantID: "t1", Subject: "viewer@example.com"}
	return r.WithContext(controlplane.WithPrincipal(r.Context(), p))
}

func TestOnboardingAddUserNonAdminForbidden(t *testing.T) {
	h, _, _ := newTestConsoleWithAdmin()
	token := issuedCSRF(t, h)
	form := url.Values{"csrf_token": {token}, "subject": {"x@y.com"}, "role": {"viewer"}}
	r := nonAdminReq("POST", "/ui/onboarding/users", form)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-admin add user: want 403 got %d", w.Code)
	}
}

func TestOnboardingSelfDemoteNonEmptySubject(t *testing.T) {
	h, _, _ := newTestConsoleWithAdmin()
	token := issuedCSRF(t, h)
	form := url.Values{"csrf_token": {token}, "subject": {"me@example.com"}, "role": {"viewer"}}
	r := adminReqAs("POST", "/ui/onboarding/users", "me@example.com", form)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("self-demote (non-empty subject): want 400 got %d", w.Code)
	}
}

func TestOnboardingSelfRemoveNonEmptySubject(t *testing.T) {
	h, _, admin := newTestConsoleWithAdmin()
	// Seed for realism only: the self-removal guard returns 400 before DeleteUser
	// is ever reached, so this test passes with or without the seed.
	_ = admin.UpsertUser(context.Background(), "t1", "me@example.com", identity.RoleAdmin)
	token := issuedCSRF(t, h)
	form := url.Values{"csrf_token": {token}}
	r := adminReqAs("POST", "/ui/onboarding/users/me@example.com/delete", "me@example.com", form)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("self-remove (non-empty subject): want 400 got %d", w.Code)
	}
}
