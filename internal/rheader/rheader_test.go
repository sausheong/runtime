package rheader

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestConsts(t *testing.T) {
	if Prefix != "X-Runtime-" {
		t.Fatalf("Prefix = %q", Prefix)
	}
	// Every claim header must carry the reserved prefix under canonical casing,
	// so the anti-spoof strip (which deletes by prefix) also removes them.
	for _, h := range []string{User, Tenant, Role} {
		if http.CanonicalHeaderKey(h)[:len(Prefix)] != Prefix {
			t.Fatalf("header %q does not start with %q", h, Prefix)
		}
	}
}

func TestSignedIdentityRoundTripAndTamper(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, "http://agent/sessions?mode=one", nil)
	req.Header.Set(User, "alice")
	req.Header.Set(Tenant, "acme")
	req.Header.Set(Role, "operator")
	req.Header.Set(Assertion, "jwt.secret.value")
	if err := Sign(req, EncodePrivateKey(privateKey), now); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(req, EncodePublicKey(publicKey), now.Add(5*time.Second), 30*time.Second); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	for _, tc := range []struct {
		name, header, value string
	}{
		{"subject", User, "mallory"},
		{"tenant", Tenant, "other"},
		{"role", Role, "admin"},
		{"assertion", Assertion, "different.jwt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tampered := req.Clone(req.Context())
			tampered.Header = req.Header.Clone()
			tampered.Header.Set(tc.header, tc.value)
			if _, err := Verify(tampered, EncodePublicKey(publicKey), now, 30*time.Second); !errors.Is(err, ErrSignatureInvalid) {
				t.Fatalf("tampered %s error=%v", tc.name, err)
			}
		})
	}
	for _, tc := range []struct {
		name   string
		mutate func(*http.Request)
	}{
		{"method", func(r *http.Request) { r.Method = http.MethodDelete }},
		{"target", func(r *http.Request) { r.URL.Path = "/sessions/other" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tampered := req.Clone(req.Context())
			tampered.Header = req.Header.Clone()
			tc.mutate(tampered)
			if _, err := Verify(tampered, EncodePublicKey(publicKey), now, 30*time.Second); !errors.Is(err, ErrSignatureInvalid) {
				t.Fatalf("tampered %s error=%v", tc.name, err)
			}
		})
	}
}

func TestSignedIdentityExpires(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, "http://agent/sessions", nil)
	if err := Sign(req, EncodePrivateKey(privateKey), now); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(req, EncodePublicKey(publicKey), now.Add(time.Minute), 30*time.Second); !errors.Is(err, ErrSignatureExpired) {
		t.Fatalf("expired error=%v", err)
	}
}
