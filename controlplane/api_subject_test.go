package controlplane

import (
	"net/http/httptest"
	"testing"

	"github.com/sausheong/runtime/internal/identity"
	"github.com/sausheong/runtime/internal/rheader"
)

// TestForwardSubject_SetsAssertion: forwarding on ⇒ a caller-supplied assertion
// spoof is stripped and the retained caller JWT (from ctx) is set, overwriting
// the forged value.
func TestForwardSubject_SetsAssertion(t *testing.T) {
	r := httptest.NewRequest("POST", "/agents/x/sessions", nil)
	r.Header.Set(rheader.Assertion, "forged") // caller-supplied spoof
	p := identity.Principal{Subject: "alice", TenantID: "acme", Role: identity.Role("operator"), Kind: identity.KindOIDC}
	r = withPrincipal(r, p)
	r = r.WithContext(identity.WithAssertion(r.Context(), "real.jwt"))

	forwardSubject(r, true)

	if got := r.Header.Get(rheader.Assertion); got != "real.jwt" {
		t.Fatalf("Assertion = %q, want real.jwt (forged spoof must be overwritten)", got)
	}
}

// TestForwardSubject_NoAssertionWhenAbsent: forwarding on, principal present, but
// no assertion on ctx ⇒ the header is stripped and nothing is set.
func TestForwardSubject_NoAssertionWhenAbsent(t *testing.T) {
	r := httptest.NewRequest("POST", "/agents/x/sessions", nil)
	r.Header.Set(rheader.Assertion, "forged") // caller-supplied spoof
	p := identity.Principal{Subject: "alice", TenantID: "acme", Role: identity.Role("operator"), Kind: identity.KindOIDC}
	r = withPrincipal(r, p)

	forwardSubject(r, true)

	if got := r.Header.Get(rheader.Assertion); got != "" {
		t.Fatalf("Assertion = %q, want empty (stripped, none on ctx)", got)
	}
}

// TestForwardSubject_StripsThenSets: forwarding on ⇒ inbound X-Runtime-* are
// stripped and the trio is set from the authenticated Principal, overwriting a
// caller-supplied spoof.
func TestForwardSubject_StripsThenSets(t *testing.T) {
	r := httptest.NewRequest("POST", "/agents/x/sessions", nil)
	r.Header.Set(rheader.User, "eve")    // caller-supplied spoof
	r.Header.Set("X-Runtime-Bogus", "x") // arbitrary spoof under the prefix
	p := identity.Principal{Subject: "alice", TenantID: "acme", Role: identity.Role("operator")}
	r = withPrincipal(r, p) // seed the authenticated principal (test helper in admin_test.go)

	forwardSubject(r, true)

	if got := r.Header.Get(rheader.User); got != "alice" {
		t.Fatalf("User = %q, want alice (spoof must be overwritten)", got)
	}
	if got := r.Header.Get(rheader.Tenant); got != "acme" {
		t.Fatalf("Tenant = %q, want acme", got)
	}
	if got := r.Header.Get(rheader.Role); got != "operator" {
		t.Fatalf("Role = %q, want operator", got)
	}
	if got := r.Header.Get("X-Runtime-Bogus"); got != "" {
		t.Fatalf("X-Runtime-Bogus = %q, want empty (stripped)", got)
	}
}

// TestForwardSubject_NoPrincipalStripsAll: forwarding on but no principal ⇒ all
// X-Runtime-* stripped, nothing set. A caller must never spoof an identity.
func TestForwardSubject_NoPrincipalStripsAll(t *testing.T) {
	r := httptest.NewRequest("POST", "/agents/x/sessions", nil)
	r.Header.Set(rheader.User, "eve")
	// forwarding on, but no principal in ctx
	forwardSubject(r, true)
	if got := r.Header.Get(rheader.User); got != "" {
		t.Fatalf("User = %q, want empty (stripped, nothing to set)", got)
	}
}

// TestForwardSubject_OffIsInert: forwarding off ⇒ no strip, no set (today's
// behavior). Documents that isolation depends on the flag being ON.
func TestForwardSubject_OffIsInert(t *testing.T) {
	r := httptest.NewRequest("POST", "/agents/x/sessions", nil)
	r.Header.Set(rheader.User, "eve") // caller header
	forwardSubject(r, false)
	if got := r.Header.Get(rheader.User); got != "eve" {
		t.Fatalf("User = %q, want eve unchanged (off is inert)", got)
	}
}
