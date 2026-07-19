package agentruntime

import (
	"net/http/httptest"
	"testing"

	"github.com/sausheong/runtime/internal/rheader"
)

// TestReadForwardedIdentity asserts the gated reader: when forwarding is on it
// returns the three X-Runtime-* header values verbatim; when off it returns
// empty strings regardless of any headers present (isolation depends on ON).
func TestReadForwardedIdentity(t *testing.T) {
	r := httptest.NewRequest("POST", "/sessions", nil)
	r.Header.Set(rheader.User, "alice")
	r.Header.Set(rheader.Tenant, "acme")
	r.Header.Set(rheader.Role, "operator")

	s, tn, rl := readForwardedIdentity(r, true)
	if s != "alice" || tn != "acme" || rl != "operator" {
		t.Fatalf("on: got %q/%q/%q, want alice/acme/operator", s, tn, rl)
	}

	s, tn, rl = readForwardedIdentity(r, false)
	if s != "" || tn != "" || rl != "" {
		t.Fatalf("off: got %q/%q/%q, want empty", s, tn, rl)
	}
}
