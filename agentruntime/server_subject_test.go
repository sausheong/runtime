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

// TestReadAssertion asserts the gated reader for the caller's forwarded JWT
// (X-Runtime-Assertion): on ⇒ the header value verbatim; off ⇒ "" regardless of
// any inbound header (the JWT never reaches the turn loop when forwarding is off).
func TestReadAssertion(t *testing.T) {
	r := httptest.NewRequest("POST", "/sessions", nil)
	r.Header.Set(rheader.Assertion, "jwt.abc.def")

	if got := readAssertion(r, true); got != "jwt.abc.def" {
		t.Fatalf("on: got %q, want jwt.abc.def", got)
	}
	if got := readAssertion(r, false); got != "" {
		t.Fatalf("off: got %q, want empty", got)
	}
}

// TestAssertionsBridgeRoundTrip exercises the ephemeral per-session bridge in
// isolation (no DBOS): Store → Load → Delete, mirroring startSession's store and
// sessionWorkflow's load+defer-delete. After Delete the JWT is gone, so a
// replay (empty map) yields "" and downstream OBO fails closed.
func TestAssertionsBridgeRoundTrip(t *testing.T) {
	var m Manager
	const sid, jwt = "sess-1", "jwt.abc.def"

	m.assertions.Store(sid, jwt)
	v, ok := m.assertions.Load(sid)
	if !ok || v.(string) != jwt {
		t.Fatalf("load: ok=%v v=%v, want true/%q", ok, v, jwt)
	}
	m.assertions.Delete(sid)
	if _, ok := m.assertions.Load(sid); ok {
		t.Fatalf("assertion survived Delete; must be gone (fail-closed on replay)")
	}
}
