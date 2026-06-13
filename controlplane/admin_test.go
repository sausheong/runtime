package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sausheong/runtime/internal/identity"
)

// fakeAdminStore implements AdminStore in-memory.
type fakeAdminStore struct {
	tenants   map[string]string
	users     map[string]identity.UserRow
	keys      map[string]identity.KeyRow
	regTokens map[string]identity.RegTokenRow
}

func newFakeAdminStore() *fakeAdminStore {
	return &fakeAdminStore{
		tenants:   map[string]string{},
		users:     map[string]identity.UserRow{},
		keys:      map[string]identity.KeyRow{},
		regTokens: map[string]identity.RegTokenRow{},
	}
}
func (f *fakeAdminStore) CreateTenant(_ context.Context, id, name string) error {
	f.tenants[id] = name
	return nil
}
func (f *fakeAdminStore) TenantExists(_ context.Context, id string) (bool, error) {
	_, ok := f.tenants[id]
	return ok, nil
}
func (f *fakeAdminStore) UpsertUser(_ context.Context, tid, sub string, role identity.Role) error {
	f.users[sub] = identity.UserRow{TenantID: tid, Subject: sub, Role: role}
	return nil
}
func (f *fakeAdminStore) DeleteUser(_ context.Context, tid, sub string) error {
	delete(f.users, sub)
	return nil
}
func (f *fakeAdminStore) ListUsers(_ context.Context, tid string) ([]identity.UserRow, error) {
	var out []identity.UserRow
	for _, u := range f.users {
		if u.TenantID == tid {
			out = append(out, u)
		}
	}
	return out, nil
}
func (f *fakeAdminStore) InsertServiceKey(_ context.Context, id, tid, hash string, role identity.Role, label string) error {
	f.keys[id] = identity.KeyRow{ID: id, TenantID: tid, Role: role, Label: label}
	return nil
}
func (f *fakeAdminStore) RevokeKey(_ context.Context, tid, id string) error {
	if k, ok := f.keys[id]; ok && k.TenantID == tid {
		k.Revoked = true
		f.keys[id] = k
	}
	return nil
}
func (f *fakeAdminStore) ListKeys(_ context.Context, tid string) ([]identity.KeyRow, error) {
	var out []identity.KeyRow
	for _, k := range f.keys {
		if k.TenantID == tid {
			out = append(out, k)
		}
	}
	return out, nil
}
func (f *fakeAdminStore) InsertRegistrationToken(_ context.Context, tokenID, agentID, hash string) error {
	f.regTokens[tokenID] = identity.RegTokenRow{TokenID: tokenID, AgentID: agentID}
	return nil
}
func (f *fakeAdminStore) ListRegistrationTokens(_ context.Context) ([]identity.RegTokenRow, error) {
	var out []identity.RegTokenRow
	for _, t := range f.regTokens {
		out = append(out, t)
	}
	return out, nil
}
func (f *fakeAdminStore) RevokeRegistrationToken(_ context.Context, tokenID string) error {
	if t, ok := f.regTokens[tokenID]; ok {
		t.Revoked = true
		f.regTokens[tokenID] = t
	}
	return nil
}
func (f *fakeAdminStore) ListTenants(_ context.Context) ([]identity.TenantRow, error) {
	var out []identity.TenantRow
	for id, name := range f.tenants {
		out = append(out, identity.TenantRow{ID: id, Name: name})
	}
	return out, nil
}

func withPrincipal(r *http.Request, p identity.Principal) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), principalKey, p))
}

func adminMux(s AdminStore) http.Handler {
	mux := http.NewServeMux()
	RegisterAdmin(mux, s, map[string]string{"support": "acme"})
	return mux
}

func TestAdmin_NonAdminForbidden(t *testing.T) {
	mux := adminMux(newFakeAdminStore())
	rec := httptest.NewRecorder()
	r := withPrincipal(httptest.NewRequest("GET", "/admin/users", nil),
		identity.Principal{TenantID: "alpha", Role: identity.RoleOperator})
	mux.ServeHTTP(rec, r)
	if rec.Code != 403 {
		t.Fatalf("operator on /admin: code=%d want 403", rec.Code)
	}
}

func TestAdmin_CreateUserScopedToTenant(t *testing.T) {
	s := newFakeAdminStore()
	s.CreateTenant(context.Background(), "alpha", "A")
	mux := adminMux(s)
	body := `{"subject":"alice@corp","role":"operator"}`
	r := withPrincipal(httptest.NewRequest("POST", "/admin/users", strings.NewReader(body)),
		identity.Principal{TenantID: "alpha", Role: identity.RoleAdmin})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != 200 && rec.Code != 201 {
		t.Fatalf("create user: code=%d", rec.Code)
	}
	if u, ok := s.users["alice@corp"]; !ok || u.TenantID != "alpha" || u.Role != identity.RoleOperator {
		t.Fatalf("user not stored in caller's tenant: %+v", s.users)
	}
}

func TestAdmin_CreateKeyReturnsPlaintextOnce(t *testing.T) {
	s := newFakeAdminStore()
	s.CreateTenant(context.Background(), "alpha", "A")
	mux := adminMux(s)
	body := `{"label":"ci","role":"viewer"}`
	r := withPrincipal(httptest.NewRequest("POST", "/admin/keys", strings.NewReader(body)),
		identity.Principal{TenantID: "alpha", Role: identity.RoleAdmin})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != 200 && rec.Code != 201 {
		t.Fatalf("create key: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		ID        string `json:"id"`
		Plaintext string `json:"plaintext"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if !strings.HasPrefix(resp.ID, "svk-") || !strings.HasPrefix(resp.Plaintext, resp.ID+".") {
		t.Fatalf("bad key response: %+v", resp)
	}
}

func TestAdmin_CreateTenantSuperuserOnly(t *testing.T) {
	s := newFakeAdminStore()
	mux := adminMux(s)
	body := `{"id":"beta","name":"Team Beta"}`
	r := withPrincipal(httptest.NewRequest("POST", "/admin/tenants", strings.NewReader(body)),
		identity.Principal{TenantID: "alpha", Role: identity.RoleAdmin})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != 403 {
		t.Fatalf("tenant-admin create tenant: code=%d want 403", rec.Code)
	}
	r2 := withPrincipal(httptest.NewRequest("POST", "/admin/tenants", strings.NewReader(body)),
		identity.Principal{Role: identity.RoleAdmin, Superuser: true})
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, r2)
	if rec2.Code != 200 && rec2.Code != 201 {
		t.Fatalf("superuser create tenant: code=%d", rec2.Code)
	}
	if _, ok := s.tenants["beta"]; !ok {
		t.Fatal("tenant beta not created")
	}
}

func TestAdmin_MissingPrincipalIs401(t *testing.T) {
	mux := adminMux(newFakeAdminStore())
	rec := httptest.NewRecorder()
	// No principal in context.
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/admin/users", nil))
	if rec.Code != 401 {
		t.Fatalf("no principal: code=%d want 401", rec.Code)
	}
}

func TestAdmin_BadRoleIs400(t *testing.T) {
	s := newFakeAdminStore()
	s.CreateTenant(context.Background(), "alpha", "A")
	mux := adminMux(s)
	r := withPrincipal(httptest.NewRequest("POST", "/admin/users", strings.NewReader(`{"subject":"x","role":"superuser"}`)),
		identity.Principal{TenantID: "alpha", Role: identity.RoleAdmin})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != 400 {
		t.Fatalf("bad role: code=%d want 400", rec.Code)
	}
}

func TestAdmin_SuperuserCreatesUserInTargetTenant(t *testing.T) {
	s := newFakeAdminStore()
	s.CreateTenant(context.Background(), "acme", "Acme")
	mux := adminMux(s)
	body := `{"subject":"root@acme","role":"admin","tenant":"acme"}`
	r := withPrincipal(httptest.NewRequest("POST", "/admin/users", strings.NewReader(body)),
		identity.Principal{Role: identity.RoleAdmin, Superuser: true}) // TenantID == ""
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != 200 {
		t.Fatalf("superuser create user in acme: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if u, ok := s.users["root@acme"]; !ok || u.TenantID != "acme" {
		t.Fatalf("user not created in acme: %+v", s.users)
	}
}

func TestAdmin_SuperuserMustSpecifyTenant(t *testing.T) {
	s := newFakeAdminStore()
	mux := adminMux(s)
	body := `{"subject":"x","role":"admin"}` // no tenant
	r := withPrincipal(httptest.NewRequest("POST", "/admin/users", strings.NewReader(body)),
		identity.Principal{Role: identity.RoleAdmin, Superuser: true})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != 400 {
		t.Fatalf("superuser without tenant: code=%d want 400", rec.Code)
	}
}

func TestAdmin_SuperuserUnknownTenant400(t *testing.T) {
	s := newFakeAdminStore()
	mux := adminMux(s)
	body := `{"label":"k","role":"viewer","tenant":"ghost"}`
	r := withPrincipal(httptest.NewRequest("POST", "/admin/keys", strings.NewReader(body)),
		identity.Principal{Role: identity.RoleAdmin, Superuser: true})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != 400 {
		t.Fatalf("superuser unknown tenant: code=%d want 400", rec.Code)
	}
}

func TestAdmin_NonSuperuserBodyTenantIgnored(t *testing.T) {
	s := newFakeAdminStore()
	s.CreateTenant(context.Background(), "alpha", "A")
	s.CreateTenant(context.Background(), "beta", "B")
	mux := adminMux(s)
	// A tenant-admin in alpha tries to target beta via body — must be ignored, user lands in alpha.
	body := `{"subject":"sneaky","role":"viewer","tenant":"beta"}`
	r := withPrincipal(httptest.NewRequest("POST", "/admin/users", strings.NewReader(body)),
		identity.Principal{TenantID: "alpha", Role: identity.RoleAdmin})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	if s.users["sneaky"].TenantID != "alpha" {
		t.Fatalf("body tenant must be ignored for non-superuser: got %q", s.users["sneaky"].TenantID)
	}
}

func TestAdmin_RevokeKeyTenantScoped(t *testing.T) {
	s := newFakeAdminStore()
	s.CreateTenant(context.Background(), "alpha", "A")
	mux := adminMux(s)
	// Create a key in alpha.
	cr := withPrincipal(httptest.NewRequest("POST", "/admin/keys", strings.NewReader(`{"role":"viewer","label":"k"}`)),
		identity.Principal{TenantID: "alpha", Role: identity.RoleAdmin})
	crrec := httptest.NewRecorder()
	mux.ServeHTTP(crrec, cr)
	var resp struct {
		ID string `json:"id"`
	}
	json.Unmarshal(crrec.Body.Bytes(), &resp)
	// A beta admin DELETE on alpha's key must be a no-op (fake RevokeKey checks tenant).
	dr := withPrincipal(httptest.NewRequest("DELETE", "/admin/keys/"+resp.ID, nil),
		identity.Principal{TenantID: "beta", Role: identity.RoleAdmin})
	drrec := httptest.NewRecorder()
	mux.ServeHTTP(drrec, dr)
	if drrec.Code != 204 {
		t.Fatalf("delete returns 204 regardless: code=%d", drrec.Code)
	}
	if s.keys[resp.ID].Revoked {
		t.Fatal("beta admin must NOT revoke alpha's key (tenant-scoped)")
	}
}

func TestAdminRegisterTokens(t *testing.T) {
	s := newFakeAdminStore()
	mux := adminMux(s) // agentTenants: support→acme

	// 1) Admin in acme mints a token for "support" → 201 + {id, plaintext}.
	cr := withPrincipal(httptest.NewRequest("POST", "/admin/register-tokens", strings.NewReader(`{"agent":"support"}`)),
		identity.Principal{TenantID: "acme", Role: identity.RoleAdmin})
	crrec := httptest.NewRecorder()
	mux.ServeHTTP(crrec, cr)
	if crrec.Code != 201 {
		t.Fatalf("mint: code=%d body=%s want 201", crrec.Code, crrec.Body.String())
	}
	var resp struct {
		ID        string `json:"id"`
		Plaintext string `json:"plaintext"`
	}
	json.Unmarshal(crrec.Body.Bytes(), &resp)
	if resp.ID == "" || resp.Plaintext == "" {
		t.Fatalf("mint response missing id/plaintext: %+v", resp)
	}

	// 2) Non-superuser admin in another tenant minting for "support" → 403.
	or := withPrincipal(httptest.NewRequest("POST", "/admin/register-tokens", strings.NewReader(`{"agent":"support"}`)),
		identity.Principal{TenantID: "other", Role: identity.RoleAdmin})
	orrec := httptest.NewRecorder()
	mux.ServeHTTP(orrec, or)
	if orrec.Code != 403 {
		t.Fatalf("cross-tenant mint: code=%d want 403", orrec.Code)
	}

	// 3) Unknown agent → 400.
	gr := withPrincipal(httptest.NewRequest("POST", "/admin/register-tokens", strings.NewReader(`{"agent":"ghost"}`)),
		identity.Principal{TenantID: "acme", Role: identity.RoleAdmin})
	grrec := httptest.NewRecorder()
	mux.ServeHTTP(grrec, gr)
	if grrec.Code != 400 {
		t.Fatalf("unknown agent: code=%d want 400", grrec.Code)
	}

	// 4) GET as acme admin → list includes the support token.
	lr := withPrincipal(httptest.NewRequest("GET", "/admin/register-tokens", nil),
		identity.Principal{TenantID: "acme", Role: identity.RoleAdmin})
	lrec := httptest.NewRecorder()
	mux.ServeHTTP(lrec, lr)
	if lrec.Code != 200 {
		t.Fatalf("list (acme): code=%d", lrec.Code)
	}
	var acmeRows []identity.RegTokenRow
	json.Unmarshal(lrec.Body.Bytes(), &acmeRows)
	if len(acmeRows) != 1 || acmeRows[0].TokenID != resp.ID || acmeRows[0].AgentID != "support" {
		t.Fatalf("acme list should include support token: %+v", acmeRows)
	}

	// GET as a different tenant → filtered out (empty).
	lr2 := withPrincipal(httptest.NewRequest("GET", "/admin/register-tokens", nil),
		identity.Principal{TenantID: "other", Role: identity.RoleAdmin})
	lrec2 := httptest.NewRecorder()
	mux.ServeHTTP(lrec2, lr2)
	if lrec2.Code != 200 {
		t.Fatalf("list (other): code=%d", lrec2.Code)
	}
	var otherRows []identity.RegTokenRow
	json.Unmarshal(lrec2.Body.Bytes(), &otherRows)
	if len(otherRows) != 0 {
		t.Fatalf("other tenant must not see acme's token: %+v", otherRows)
	}

	// 5) Non-superuser admin in another tenant DELETE'ing acme's token → 204
	// (no oracle) but the token must NOT be revoked.
	xr := withPrincipal(httptest.NewRequest("DELETE", "/admin/register-tokens/"+resp.ID, nil),
		identity.Principal{TenantID: "other", Role: identity.RoleAdmin})
	xrrec := httptest.NewRecorder()
	mux.ServeHTTP(xrrec, xr)
	if xrrec.Code != 204 {
		t.Fatalf("cross-tenant revoke: code=%d want 204", xrrec.Code)
	}
	if s.regTokens[resp.ID].Revoked {
		t.Fatal("cross-tenant revoke must be a no-op (token still active)")
	}

	// 6) Owning tenant (acme) admin revokes the support token → 204, revoked.
	dr := withPrincipal(httptest.NewRequest("DELETE", "/admin/register-tokens/"+resp.ID, nil),
		identity.Principal{TenantID: "acme", Role: identity.RoleAdmin})
	drrec := httptest.NewRecorder()
	mux.ServeHTTP(drrec, dr)
	if drrec.Code != 204 {
		t.Fatalf("revoke: code=%d want 204", drrec.Code)
	}
	if !s.regTokens[resp.ID].Revoked {
		t.Fatal("token not revoked")
	}

	// 7) Superuser revokes unconditionally → mint a fresh token, revoke as
	// superuser (TenantID == "", Superuser) → 204 + revoked.
	cr2 := withPrincipal(httptest.NewRequest("POST", "/admin/register-tokens", strings.NewReader(`{"agent":"support"}`)),
		identity.Principal{Superuser: true, Role: identity.RoleAdmin})
	cr2rec := httptest.NewRecorder()
	mux.ServeHTTP(cr2rec, cr2)
	if cr2rec.Code != 201 {
		t.Fatalf("superuser mint: code=%d body=%s want 201", cr2rec.Code, cr2rec.Body.String())
	}
	var resp2 struct {
		ID string `json:"id"`
	}
	json.Unmarshal(cr2rec.Body.Bytes(), &resp2)
	sdr := withPrincipal(httptest.NewRequest("DELETE", "/admin/register-tokens/"+resp2.ID, nil),
		identity.Principal{Superuser: true, Role: identity.RoleAdmin})
	sdrrec := httptest.NewRecorder()
	mux.ServeHTTP(sdrrec, sdr)
	if sdrrec.Code != 204 {
		t.Fatalf("superuser revoke: code=%d want 204", sdrrec.Code)
	}
	if !s.regTokens[resp2.ID].Revoked {
		t.Fatal("superuser revoke should revoke the token")
	}
}

// fakeSecretAdmin implements SecretAdmin in-memory.
type fakeSecretAdmin struct {
	set     map[string]map[string]string // tenant -> name -> plaintext
	names   map[string][]identity.SecretMeta
	rotated []string
}

func newFakeSecretAdmin() *fakeSecretAdmin {
	return &fakeSecretAdmin{set: map[string]map[string]string{}, names: map[string][]identity.SecretMeta{}}
}
func (f *fakeSecretAdmin) SetSecret(_ context.Context, tenant, name, plaintext string) error {
	if f.set[tenant] == nil {
		f.set[tenant] = map[string]string{}
	}
	f.set[tenant][name] = plaintext
	f.names[tenant] = append(f.names[tenant], identity.SecretMeta{Name: name})
	return nil
}
func (f *fakeSecretAdmin) ListSecretNames(_ context.Context, tenant string) ([]identity.SecretMeta, error) {
	return f.names[tenant], nil
}
func (f *fakeSecretAdmin) DeleteSecret(_ context.Context, tenant, name string) error {
	delete(f.set[tenant], name)
	return nil
}
func (f *fakeSecretAdmin) RotateSecrets(_ context.Context, tenant string) (identity.RotateStats, error) {
	f.rotated = append(f.rotated, tenant)
	return identity.RotateStats{Tenant: tenant, Total: 1, Rotated: 1}, nil
}

// adminMuxWithSecrets wires both the store and the secret admin.
func adminMuxWithSecrets(s AdminStore, sa SecretAdmin) http.Handler {
	mux := http.NewServeMux()
	RegisterAdmin(mux, s, map[string]string{"support": "acme"})
	RegisterSecretAdmin(mux, s, sa)
	return mux
}

func TestSecretAdmin_SetAndListNoValueLeak(t *testing.T) {
	s := newFakeAdminStore()
	s.CreateTenant(context.Background(), "alpha", "A")
	sa := newFakeSecretAdmin()
	mux := adminMuxWithSecrets(s, sa)

	body := `{"name":"OPENAI_API_KEY","value":"sk-secret"}`
	r := withPrincipal(httptest.NewRequest("POST", "/admin/secrets", strings.NewReader(body)),
		identity.Principal{TenantID: "alpha", Role: identity.RoleAdmin})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != 200 && rec.Code != 201 {
		t.Fatalf("set secret: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if sa.set["alpha"]["OPENAI_API_KEY"] != "sk-secret" {
		t.Fatalf("secret not stored: %+v", sa.set)
	}

	lr := withPrincipal(httptest.NewRequest("GET", "/admin/secrets", nil),
		identity.Principal{TenantID: "alpha", Role: identity.RoleAdmin})
	lrec := httptest.NewRecorder()
	mux.ServeHTTP(lrec, lr)
	if lrec.Code != 200 {
		t.Fatalf("list: code=%d", lrec.Code)
	}
	if strings.Contains(lrec.Body.String(), "sk-secret") {
		t.Fatalf("LIST LEAKED THE VALUE: %s", lrec.Body.String())
	}
	if !strings.Contains(lrec.Body.String(), "OPENAI_API_KEY") {
		t.Fatalf("list missing name: %s", lrec.Body.String())
	}
}

func TestSecretAdmin_NonAdminForbidden(t *testing.T) {
	mux := adminMuxWithSecrets(newFakeAdminStore(), newFakeSecretAdmin())
	r := withPrincipal(httptest.NewRequest("POST", "/admin/secrets", strings.NewReader(`{"name":"K","value":"v"}`)),
		identity.Principal{TenantID: "alpha", Role: identity.RoleOperator})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != 403 {
		t.Fatalf("operator set secret: code=%d want 403", rec.Code)
	}
}

func TestSecretAdmin_DisabledIs503(t *testing.T) {
	s := newFakeAdminStore()
	s.CreateTenant(context.Background(), "alpha", "A")
	mux := http.NewServeMux()
	RegisterAdmin(mux, s, map[string]string{"support": "acme"})
	RegisterSecretAdmin(mux, s, nil) // nil broker ⇒ feature disabled
	r := withPrincipal(httptest.NewRequest("POST", "/admin/secrets", strings.NewReader(`{"name":"K","value":"v"}`)),
		identity.Principal{TenantID: "alpha", Role: identity.RoleAdmin})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != 503 {
		t.Fatalf("no broker: code=%d want 503", rec.Code)
	}
}

func TestSecretAdmin_BadNameOrValue400(t *testing.T) {
	s := newFakeAdminStore()
	s.CreateTenant(context.Background(), "alpha", "A")
	mux := adminMuxWithSecrets(s, newFakeSecretAdmin())
	cases := []string{
		`{"name":"","value":"v"}`,
		`{"name":"OPENAI","value":""}`,
		`{"name":"bad name","value":"v"}`,
		`{"name":"1BAD","value":"v"}`,
		`{"name":"A=B","value":"v"}`,
	}
	for _, body := range cases {
		r := withPrincipal(httptest.NewRequest("POST", "/admin/secrets", strings.NewReader(body)),
			identity.Principal{TenantID: "alpha", Role: identity.RoleAdmin})
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, r)
		if rec.Code != 400 {
			t.Fatalf("body %s: code=%d want 400", body, rec.Code)
		}
	}
}

func TestSecretAdmin_Delete(t *testing.T) {
	s := newFakeAdminStore()
	s.CreateTenant(context.Background(), "alpha", "A")
	sa := newFakeSecretAdmin()
	mux := adminMuxWithSecrets(s, sa)
	sa.SetSecret(context.Background(), "alpha", "K", "v")
	r := withPrincipal(httptest.NewRequest("DELETE", "/admin/secrets/K", nil),
		identity.Principal{TenantID: "alpha", Role: identity.RoleAdmin})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != 204 {
		t.Fatalf("delete: code=%d want 204", rec.Code)
	}
	if _, ok := sa.set["alpha"]["K"]; ok {
		t.Fatal("secret not deleted")
	}
}

func TestSecretAdmin_RotateNonAdminForbidden(t *testing.T) {
	mux := adminMuxWithSecrets(newFakeAdminStore(), newFakeSecretAdmin())
	r := withPrincipal(httptest.NewRequest("POST", "/admin/secrets/rotate", strings.NewReader(`{}`)),
		identity.Principal{TenantID: "alpha", Role: identity.RoleOperator})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != 403 {
		t.Fatalf("operator rotate: code=%d want 403", rec.Code)
	}
}

func TestSecretAdmin_RotateDisabledIs503(t *testing.T) {
	s := newFakeAdminStore()
	mux := http.NewServeMux()
	RegisterAdmin(mux, s, map[string]string{"support": "acme"})
	RegisterSecretAdmin(mux, s, nil)
	r := withPrincipal(httptest.NewRequest("POST", "/admin/secrets/rotate", strings.NewReader(`{}`)),
		identity.Principal{TenantID: "alpha", Role: identity.RoleAdmin})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != 503 {
		t.Fatalf("nil broker rotate: code=%d want 503", rec.Code)
	}
}

func TestSecretAdmin_RotateNonSuperuserScopedToOwnTenant(t *testing.T) {
	s := newFakeAdminStore()
	s.CreateTenant(context.Background(), "alpha", "A")
	sa := newFakeSecretAdmin()
	mux := adminMuxWithSecrets(s, sa)
	// tenant-admin in alpha tries to target beta via body — must rotate alpha only.
	r := withPrincipal(httptest.NewRequest("POST", "/admin/secrets/rotate", strings.NewReader(`{"tenant":"beta"}`)),
		identity.Principal{TenantID: "alpha", Role: identity.RoleAdmin})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != 200 {
		t.Fatalf("admin rotate: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(sa.rotated) != 1 || sa.rotated[0] != "alpha" {
		t.Fatalf("non-superuser rotated %v, want [alpha]", sa.rotated)
	}
}

func TestSecretAdmin_RotateSuperuserAllTenants(t *testing.T) {
	s := newFakeAdminStore()
	s.CreateTenant(context.Background(), "alpha", "A")
	s.CreateTenant(context.Background(), "beta", "B")
	sa := newFakeSecretAdmin()
	mux := adminMuxWithSecrets(s, sa)
	r := withPrincipal(httptest.NewRequest("POST", "/admin/secrets/rotate", strings.NewReader(`{}`)),
		identity.Principal{Role: identity.RoleAdmin, Superuser: true})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != 200 {
		t.Fatalf("superuser rotate-all: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(sa.rotated) != 2 {
		t.Fatalf("superuser all-tenants rotated %v, want 2", sa.rotated)
	}
}

func TestSecretAdmin_RotateSuperuserSpecificTenant(t *testing.T) {
	s := newFakeAdminStore()
	s.CreateTenant(context.Background(), "acme", "Acme")
	sa := newFakeSecretAdmin()
	mux := adminMuxWithSecrets(s, sa)
	r := withPrincipal(httptest.NewRequest("POST", "/admin/secrets/rotate", strings.NewReader(`{"tenant":"acme"}`)),
		identity.Principal{Role: identity.RoleAdmin, Superuser: true})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != 200 {
		t.Fatalf("superuser rotate acme: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(sa.rotated) != 1 || sa.rotated[0] != "acme" {
		t.Fatalf("rotated %v, want [acme]", sa.rotated)
	}
}
