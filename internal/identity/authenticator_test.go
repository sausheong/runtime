package identity

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// memSource is a hermetic stand-in for the store methods Authenticator needs.
type memSource struct {
	users map[string]UserRow   // subject -> user
	keys  map[string]activeKey // id -> active key
}

func (m memSource) UserBySubject(_ context.Context, sub string) (UserRow, error) {
	u, ok := m.users[sub]
	if !ok {
		return UserRow{}, ErrNoUser
	}
	return u, nil
}
func (m memSource) ActiveKeyByID(_ context.Context, id string) (activeKey, error) {
	k, ok := m.keys[id]
	if !ok {
		return activeKey{}, ErrNoKey
	}
	return k, nil
}

func req(auth string) *http.Request {
	r := httptest.NewRequest("GET", "/agents", nil)
	if auth != "" {
		r.Header.Set("Authorization", "Bearer "+auth)
	}
	return r
}

func TestAuthenticate_ServiceKey(t *testing.T) {
	mk, _ := MintServiceKey()
	src := memSource{
		keys: map[string]activeKey{mk.ID: {TenantID: "alpha", Hash: mk.Hash, Role: RoleOperator}},
	}
	a := NewAuthenticator(src, fakeVerifier{}, "", nil)
	p, err := a.Authenticate(context.Background(), req(mk.Plaintext))
	if err != nil {
		t.Fatal(err)
	}
	if p.TenantID != "alpha" || p.Role != RoleOperator || p.Subject != mk.ID {
		t.Fatalf("principal = %+v", p)
	}
}

func TestAuthenticate_ServiceKeyWrongSecret(t *testing.T) {
	mk, _ := MintServiceKey()
	src := memSource{keys: map[string]activeKey{mk.ID: {TenantID: "alpha", Hash: mk.Hash, Role: RoleOperator}}}
	a := NewAuthenticator(src, fakeVerifier{}, "", nil)
	_, err := a.Authenticate(context.Background(), req(mk.ID+".wrongsecret"))
	if err != ErrUnauthenticated {
		t.Fatalf("err=%v want ErrUnauthenticated", err)
	}
}

func TestAuthenticate_OIDCUser(t *testing.T) {
	src := memSource{users: map[string]UserRow{"alice@corp": {TenantID: "beta", Subject: "alice@corp", Role: RoleViewer}}}
	a := NewAuthenticator(src, fakeVerifier{good: "jwt.aaa.bbb", sub: "alice@corp"}, "", nil)
	p, err := a.Authenticate(context.Background(), req("jwt.aaa.bbb"))
	if err != nil {
		t.Fatal(err)
	}
	if p.TenantID != "beta" || p.Role != RoleViewer || p.Subject != "alice@corp" {
		t.Fatalf("principal = %+v", p)
	}
}

func TestAuthenticate_OIDCValidButNotProvisioned(t *testing.T) {
	src := memSource{} // no users
	a := NewAuthenticator(src, fakeVerifier{good: "jwt.aaa.bbb", sub: "stranger"}, "", nil)
	_, err := a.Authenticate(context.Background(), req("jwt.aaa.bbb"))
	if err != ErrNotProvisioned {
		t.Fatalf("err=%v want ErrNotProvisioned", err)
	}
}

func TestAuthenticate_BadOIDCToken(t *testing.T) {
	a := NewAuthenticator(memSource{}, fakeVerifier{good: "good.tok.en"}, "", nil)
	_, err := a.Authenticate(context.Background(), req("bad.to.ken"))
	if err != ErrUnauthenticated {
		t.Fatalf("err=%v want ErrUnauthenticated", err)
	}
}

func TestAuthenticate_NoCredential(t *testing.T) {
	a := NewAuthenticator(memSource{}, fakeVerifier{}, "", nil)
	_, err := a.Authenticate(context.Background(), req(""))
	if err != ErrUnauthenticated {
		t.Fatalf("err=%v want ErrUnauthenticated", err)
	}
}

func TestAuthenticate_BootstrapSuperuser(t *testing.T) {
	a := NewAuthenticator(memSource{}, fakeVerifier{}, "boot-secret-123", nil)
	p, err := a.Authenticate(context.Background(), req("boot-secret-123"))
	if err != nil {
		t.Fatal(err)
	}
	if !p.Superuser || p.Role != RoleAdmin {
		t.Fatalf("bootstrap principal = %+v, want superuser admin", p)
	}
}

func TestAuthenticate_LegacyToken(t *testing.T) {
	a := NewAuthenticator(memSource{}, fakeVerifier{}, "", map[string]string{"legacy-tok": "ci"})
	p, err := a.Authenticate(context.Background(), req("legacy-tok"))
	if err != nil {
		t.Fatal(err)
	}
	if !p.Superuser || p.Role != RoleAdmin || p.Subject != "legacy:ci" {
		t.Fatalf("legacy principal = %+v, want superuser admin legacy:ci", p)
	}
}

func TestAuthenticate_OIDCDisabledRejectsJWT(t *testing.T) {
	// With a nil verifier (OIDC disabled), a JWT-shaped credential must not panic
	// and must be rejected as unauthenticated.
	a := NewAuthenticator(memSource{}, nil, "", nil)
	_, err := a.Authenticate(context.Background(), req("aaa.bbb.ccc"))
	if err != ErrUnauthenticated {
		t.Fatalf("oidc disabled: err=%v want ErrUnauthenticated", err)
	}
}

func TestAuthenticate_CookieFallback(t *testing.T) {
	src := memSource{users: map[string]UserRow{"alice@corp": {TenantID: "beta", Subject: "alice@corp", Role: RoleViewer}}}
	a := NewAuthenticator(src, fakeVerifier{good: "jwt.aaa.bbb", sub: "alice@corp"}, "", nil)
	r := httptest.NewRequest("GET", "/ui", nil)
	r.AddCookie(&http.Cookie{Name: "runtime_token", Value: "jwt.aaa.bbb"})
	p, err := a.Authenticate(context.Background(), r)
	if err != nil || p.Subject != "alice@corp" {
		t.Fatalf("cookie auth: p=%+v err=%v", p, err)
	}
}
