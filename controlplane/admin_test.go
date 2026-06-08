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
	tenants map[string]string
	users   map[string]identity.UserRow
	keys    map[string]identity.KeyRow
}

func newFakeAdminStore() *fakeAdminStore {
	return &fakeAdminStore{tenants: map[string]string{}, users: map[string]identity.UserRow{}, keys: map[string]identity.KeyRow{}}
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

func withPrincipal(r *http.Request, p identity.Principal) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), principalKey, p))
}

func adminMux(s AdminStore) http.Handler {
	mux := http.NewServeMux()
	RegisterAdmin(mux, s)
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
