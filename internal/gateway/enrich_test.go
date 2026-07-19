package gateway

import (
	"testing"

	"github.com/sausheong/runtime/internal/identity"
)

func TestResolveEnrichedHeaders(t *testing.T) {
	enrich := map[string]string{"tenant": "X-Runtime-Tenant", "subject": "X-Runtime-User", "role": "X-Runtime-Role"}
	p := identity.Principal{TenantID: "acme", Subject: "svc-ops", Role: identity.RoleOperator}
	got := ResolveEnrichedHeaders(enrich, p, true)
	if got["X-Runtime-Tenant"] != "acme" || got["X-Runtime-User"] != "svc-ops" || got["X-Runtime-Role"] != "operator" {
		t.Fatalf("wrong headers: %v", got)
	}
	// Missing claim value omits the header.
	p2 := identity.Principal{TenantID: "acme"} // empty subject
	got2 := ResolveEnrichedHeaders(enrich, p2, true)
	if _, present := got2["X-Runtime-User"]; present {
		t.Error("empty subject must omit its header")
	}
	// open mode (ok=false) ⇒ nil.
	if ResolveEnrichedHeaders(enrich, identity.Principal{}, false) != nil {
		t.Error("open mode must inject nothing")
	}
}
