package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/sausheong/runtime/internal/identity"
)

// fakeOBOSource implements OBOSource over an in-memory config with a bumpable gen
// and a configurable cred type (to exercise the non-OBO branch).
type fakeOBOSource struct {
	cfg identity.OBOConfig
	ct  string
	gen atomic.Uint64
}

func (f *fakeOBOSource) OBOConfigFor(_ context.Context, _, _ string) (identity.OBOConfig, error) {
	return f.cfg, nil
}
func (f *fakeOBOSource) CredType(_ context.Context, _, _ string) (string, error) {
	if f.ct != "" {
		return f.ct, nil
	}
	return identity.CredTypeOBO, nil
}
func (f *fakeOBOSource) Generation() uint64 { return f.gen.Load() }

// oboTokenServer is a fake RFC 8693 exchange endpoint. It asserts the grant type
// and subject_token, and returns an access token derived from the subject_token
// so distinct callers observe distinct tokens.
func oboTokenServer(t *testing.T, hits *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
		}
		if got := r.Form.Get("grant_type"); got != oboGrantType {
			t.Errorf("grant_type = %q, want %q", got, oboGrantType)
		}
		subj := r.Form.Get("subject_token")
		if subj == "" {
			t.Errorf("subject_token is empty")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":      "obo-" + subj,
			"token_type":        "Bearer",
			"expires_in":        3600,
			"issued_token_type": "urn:ietf:params:oauth:token-type:access_token",
		})
	}))
}

func TestOBOManagerMintCarriesSubjectToken(t *testing.T) {
	var hits int32
	ts := oboTokenServer(t, &hits)
	defer ts.Close()
	src := &fakeOBOSource{cfg: identity.OBOConfig{TokenURL: ts.URL, ClientID: "cid", ClientSecret: "sec"}}
	m := NewOBOManager(context.Background(), src)

	v, applies, err := m.Bearer(context.Background(), "acme", "orders_obo", "alice", "alice.jwt")
	if err != nil || !applies {
		t.Fatalf("Bearer applies=%v err=%v", applies, err)
	}
	if v != "Bearer obo-alice.jwt" {
		t.Fatalf("Bearer value = %q, want %q (exchange must carry the caller jwt as subject_token)", v, "Bearer obo-alice.jwt")
	}
}

func TestOBOManagerCacheReuse(t *testing.T) {
	var hits int32
	ts := oboTokenServer(t, &hits)
	defer ts.Close()
	src := &fakeOBOSource{cfg: identity.OBOConfig{TokenURL: ts.URL, ClientID: "cid", ClientSecret: "sec"}}
	m := NewOBOManager(context.Background(), src)

	// Two Bearer for the SAME (tenant, name, subject) → single mint (cache hit).
	if _, _, err := m.Bearer(context.Background(), "acme", "orders_obo", "alice", "alice.jwt"); err != nil {
		t.Fatalf("first Bearer: %v", err)
	}
	if _, _, err := m.Bearer(context.Background(), "acme", "orders_obo", "alice", "alice.jwt"); err != nil {
		t.Fatalf("second Bearer: %v", err)
	}
	if hits != 1 {
		t.Fatalf("token endpoint hit %d times, want 1 (cache reuse)", hits)
	}
}

func TestOBOManagerPerCallerIsolation(t *testing.T) {
	var hits int32
	ts := oboTokenServer(t, &hits)
	defer ts.Close()
	src := &fakeOBOSource{cfg: identity.OBOConfig{TokenURL: ts.URL, ClientID: "cid", ClientSecret: "sec"}}
	m := NewOBOManager(context.Background(), src)

	// Same (tenant, name), DIFFERENT subjects → two mints, distinct tokens.
	va, _, err := m.Bearer(context.Background(), "acme", "orders_obo", "alice", "alice.jwt")
	if err != nil {
		t.Fatalf("alice Bearer: %v", err)
	}
	vb, _, err := m.Bearer(context.Background(), "acme", "orders_obo", "bob", "bob.jwt")
	if err != nil {
		t.Fatalf("bob Bearer: %v", err)
	}
	if hits != 2 {
		t.Fatalf("token endpoint hit %d times, want 2 (per-caller isolation)", hits)
	}
	if va == vb {
		t.Fatalf("per-caller tokens must differ: alice=%q bob=%q", va, vb)
	}
	if va != "Bearer obo-alice.jwt" || vb != "Bearer obo-bob.jwt" {
		t.Fatalf("unexpected tokens: alice=%q bob=%q", va, vb)
	}
}

func TestOBOManagerRebuildsOnGenerationBump(t *testing.T) {
	var hits int32
	ts := oboTokenServer(t, &hits)
	defer ts.Close()
	src := &fakeOBOSource{cfg: identity.OBOConfig{TokenURL: ts.URL, ClientID: "cid", ClientSecret: "sec"}}
	m := NewOBOManager(context.Background(), src)

	if _, _, err := m.Bearer(context.Background(), "acme", "c", "alice", "alice.jwt"); err != nil {
		t.Fatalf("first Bearer: %v", err)
	}
	// Generation bump (rotation) → next call for the same caller rebuilds + re-mints.
	src.gen.Add(1)
	if _, _, err := m.Bearer(context.Background(), "acme", "c", "alice", "alice.jwt"); err != nil {
		t.Fatalf("second Bearer: %v", err)
	}
	if hits != 2 {
		t.Fatalf("token endpoint hit %d times, want 2 (gen bump forces rebuild)", hits)
	}
}

func TestOBOManagerFailsClosedWithoutAssertion(t *testing.T) {
	var hits int32
	ts := oboTokenServer(t, &hits)
	defer ts.Close()
	src := &fakeOBOSource{cfg: identity.OBOConfig{TokenURL: ts.URL, ClientID: "cid", ClientSecret: "sec"}}
	m := NewOBOManager(context.Background(), src)

	v, applies, err := m.Bearer(context.Background(), "acme", "orders_obo", "", "")
	if err == nil {
		t.Fatal("empty jwt must fail closed (err != nil)")
	}
	if !applies {
		t.Fatal("applies must be true for an OBO cred even without an assertion")
	}
	if v != "" {
		t.Fatalf("value must be empty on error, got %q", v)
	}
	if hits != 0 {
		t.Fatalf("token endpoint hit %d times, want 0 (never dispatch without a caller token)", hits)
	}
}

func TestOBOManagerNonOBOCred(t *testing.T) {
	var hits int32
	ts := oboTokenServer(t, &hits)
	defer ts.Close()
	src := &fakeOBOSource{
		cfg: identity.OBOConfig{TokenURL: ts.URL, ClientID: "cid", ClientSecret: "sec"},
		ct:  identity.CredTypeStatic,
	}
	m := NewOBOManager(context.Background(), src)

	v, applies, err := m.Bearer(context.Background(), "acme", "static_cred", "alice", "alice.jwt")
	if err != nil {
		t.Fatalf("non-OBO cred must not error, got %v", err)
	}
	if applies {
		t.Fatal("applies must be false for a non-OBO cred")
	}
	if v != "" {
		t.Fatalf("value must be empty for a non-OBO cred, got %q", v)
	}
}

func TestOBOManagerFailsClosedOnMintError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer ts.Close()
	src := &fakeOBOSource{cfg: identity.OBOConfig{TokenURL: ts.URL, ClientID: "cid", ClientSecret: "sec"}}
	m := NewOBOManager(context.Background(), src)

	v, applies, err := m.Bearer(context.Background(), "acme", "orders_obo", "alice", "alice.jwt")
	if err == nil {
		t.Fatal("expected mint error (fail closed), got nil")
	}
	if !applies {
		t.Fatal("applies must be true for an OBO cred even on mint failure")
	}
	if v != "" {
		t.Fatalf("value must be empty on error, got %q", v)
	}
}
