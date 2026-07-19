package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/sausheong/runtime/internal/identity"
)

// fakeSource implements OAuth2Source over an in-memory config with a bumpable gen.
type fakeSource struct {
	cfg identity.OAuth2Config
	gen atomic.Uint64
}

func (f *fakeSource) OAuth2ConfigFor(_ context.Context, _, _ string) (identity.OAuth2Config, error) {
	return f.cfg, nil
}
func (f *fakeSource) CredType(_ context.Context, _, _ string) (string, error) {
	return identity.CredTypeOAuth2, nil
}
func (f *fakeSource) Generation() uint64 { return f.gen.Load() }

func tokenServer(t *testing.T, hits *int32, clientID *string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		_ = r.ParseForm()
		if clientID != nil {
			*clientID = r.Form.Get("client_id") // present when AuthStyleInParams; else basic-auth
			if *clientID == "" {
				u, _, _ := r.BasicAuth()
				*clientID = u
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "tok-" + *clientID, "token_type": "Bearer", "expires_in": 3600,
		})
	}))
}

func TestOAuth2ManagerMintAndReuse(t *testing.T) {
	var hits int32
	var seen string
	ts := tokenServer(t, &hits, &seen)
	defer ts.Close()
	src := &fakeSource{cfg: identity.OAuth2Config{TokenURL: ts.URL, ClientID: "cid", ClientSecret: "sec"}}
	m := NewOAuth2Manager(context.Background(), src)

	v, applies, err := m.Bearer(context.Background(), "acme", "orders_oauth")
	if err != nil || !applies || v != "Bearer tok-cid" {
		t.Fatalf("Bearer = %q applies=%v err=%v", v, applies, err)
	}
	// Second call within TTL must NOT hit the token endpoint again.
	if _, _, err := m.Bearer(context.Background(), "acme", "orders_oauth"); err != nil {
		t.Fatalf("second Bearer: %v", err)
	}
	if hits != 1 {
		t.Fatalf("token endpoint hit %d times, want 1 (cache miss)", hits)
	}
}

func TestOAuth2ManagerRebuildsOnGenerationBump(t *testing.T) {
	var hits int32
	var seen string
	ts := tokenServer(t, &hits, &seen)
	defer ts.Close()
	src := &fakeSource{cfg: identity.OAuth2Config{TokenURL: ts.URL, ClientID: "cid1", ClientSecret: "sec"}}
	m := NewOAuth2Manager(context.Background(), src)
	v1, _, _ := m.Bearer(context.Background(), "acme", "c")
	if v1 != "Bearer tok-cid1" {
		t.Fatalf("v1 = %q", v1)
	}
	// Rotate config + bump generation → next call rebuilds with new client_id.
	src.cfg.ClientID = "cid2"
	src.gen.Add(1)
	v2, _, _ := m.Bearer(context.Background(), "acme", "c")
	if v2 != "Bearer tok-cid2" {
		t.Fatalf("v2 = %q (did not rebuild on gen bump)", v2)
	}
}

// errCredTypeSource is a source whose CredType lookup fails (e.g. the cred was
// deleted mid-session or a transient DB error).
type errCredTypeSource struct{ err error }

func (s *errCredTypeSource) OAuth2ConfigFor(_ context.Context, _, _ string) (identity.OAuth2Config, error) {
	return identity.OAuth2Config{}, s.err
}
func (s *errCredTypeSource) CredType(_ context.Context, _, _ string) (string, error) {
	return "", s.err
}
func (s *errCredTypeSource) Generation() uint64 { return 0 }

// TestOAuth2ManagerBearerCredTypeError is the FIX-2 regression: a CredType
// lookup error must surface as err!=nil (applies=false) so gate #5 fails CLOSED,
// rather than the old fail-OPEN (nil err) that let an oauth2 upstream go out
// with no Authorization header.
func TestOAuth2ManagerBearerCredTypeError(t *testing.T) {
	src := &errCredTypeSource{err: errors.New("not found")}
	m := NewOAuth2Manager(context.Background(), src)
	v, applies, err := m.Bearer(context.Background(), "acme", "orders_oauth")
	if err == nil {
		t.Fatal("Bearer must surface CredType error (fail closed), got nil")
	}
	if applies {
		t.Fatal("applies must be false on a CredType lookup error")
	}
	if v != "" {
		t.Fatalf("value must be empty on error, got %q", v)
	}
}

func TestOAuth2ManagerFailClosed(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer ts.Close()
	src := &fakeSource{cfg: identity.OAuth2Config{TokenURL: ts.URL, ClientID: "cid", ClientSecret: "sec"}}
	m := NewOAuth2Manager(context.Background(), src)
	_, applies, err := m.Bearer(context.Background(), "acme", "c")
	if !applies {
		t.Fatal("applies should be true for an oauth2 cred even on mint failure")
	}
	if err == nil {
		t.Fatal("expected mint error (fail closed), got nil")
	}
}
