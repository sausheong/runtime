package policy

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sausheong/runtime/internal/identity"
)

// testStoreConformance exercises the PolicyStore contract; run against both
// MemStore (hermetic) and the PG Store (integration).
func testStoreConformance(t *testing.T, s PolicyStore) {
	ctx := context.Background()
	valid := `forbid (principal, action, resource) when { resource.server == "sandbox" };`

	if err := s.Insert(ctx, Row{Tenant: "acme", Name: "no-sandbox", CedarText: valid}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.List(ctx, "acme")
	if err != nil || len(rows) != 1 || rows[0].Name != "no-sandbox" {
		t.Fatalf("List = %v, %v", rows, err)
	}
	named, gen1, err := s.PoliciesFor(ctx, "acme")
	if err != nil || len(named) != 1 || named[0].ID != "tenant/no-sandbox" {
		t.Fatalf("PoliciesFor = %v gen=%d err=%v", named, gen1, err)
	}

	// Tenant isolation.
	if other, _, _ := s.PoliciesFor(ctx, "globex"); len(other) != 0 {
		t.Error("cross-tenant leak")
	}

	// Validation-on-write.
	if err := s.Insert(ctx, Row{Tenant: "acme", Name: "bad", CedarText: "not cedar"}); err == nil {
		t.Error("unparseable text must be rejected")
	}
	two := valid + "\n" + valid
	if err := s.Insert(ctx, Row{Tenant: "acme", Name: "two", CedarText: two}); err == nil {
		t.Error("multi-policy text must be rejected")
	}
	if err := s.Insert(ctx, Row{Tenant: "acme", Name: "no-sandbox", CedarText: valid}); err == nil {
		t.Error("duplicate name must be rejected")
	}

	// Delete bumps generation; PoliciesFor sees removal.
	okDel, err := s.Delete(ctx, "acme", "no-sandbox")
	if err != nil || !okDel {
		t.Fatalf("Delete = %v, %v", okDel, err)
	}
	named, gen2, _ := s.PoliciesFor(ctx, "acme")
	if len(named) != 0 || gen2 == gen1 {
		t.Errorf("after delete: %v gen1=%d gen2=%d", named, gen1, gen2)
	}
	if okDel, _ := s.Delete(ctx, "acme", "ghost"); okDel {
		t.Error("deleting a missing policy must report false")
	}
}

func TestMemStoreConformance(t *testing.T) { testStoreConformance(t, NewMemStore()) }

// TestEngineTenantLayer proves the engine composes the tenant layer, denies on
// a tenant forbid, invalidates its cache on generation change, and keeps
// platform forbids independent of the tenant layer.
func TestEngineTenantLayer(t *testing.T) {
	ctx := context.Background()
	ms := NewMemStore()
	platform := []byte(`forbid (principal, action, resource) when { resource.server == "browser" };`)
	e, err := NewEngine(platform, ms)
	if err != nil {
		t.Fatal(err)
	}
	p := identity.Principal{TenantID: "acme", Subject: "svc-a", Role: identity.RoleOperator}
	sandboxCall := Request{Principal: p, OK: true, ToolName: "sandbox__run_code",
		Args: json.RawMessage(`{}`), Mode: "full"}

	// No tenant policy yet ⇒ sandbox allowed (platform only forbids browser).
	if d := e.Evaluate(ctx, sandboxCall); !d.Allow {
		t.Fatalf("baseline sandbox call must pass: %+v", d)
	}
	// Add a tenant forbid ⇒ next eval denies with tenant/<name>.
	if err := ms.Insert(ctx, Row{Tenant: "acme", Name: "no-sbx",
		CedarText: `forbid (principal, action, resource) when { resource.server == "sandbox" };`}); err != nil {
		t.Fatal(err)
	}
	d := e.Evaluate(ctx, sandboxCall)
	if d.Allow || d.PolicyID != "tenant/no-sbx" {
		t.Fatalf("tenant forbid must deny with tenant/no-sbx: %+v", d)
	}
	// Another tenant is unaffected.
	pg := Request{Principal: identity.Principal{TenantID: "globex", Subject: "s", Role: identity.RoleOperator},
		OK: true, ToolName: "sandbox__run_code", Args: json.RawMessage(`{}`), Mode: "full"}
	if d := e.Evaluate(ctx, pg); !d.Allow {
		t.Fatalf("other tenant must be unaffected: %+v", d)
	}
	// Delete ⇒ generation bump invalidates cache ⇒ allowed again.
	if _, err := ms.Delete(ctx, "acme", "no-sbx"); err != nil {
		t.Fatal(err)
	}
	if d := e.Evaluate(ctx, sandboxCall); !d.Allow {
		t.Fatalf("after tenant delete, sandbox must pass: %+v", d)
	}
	// Platform forbid still applies regardless of tenant layer.
	browserCall := Request{Principal: p, OK: true, ToolName: "browser__navigate",
		Args: json.RawMessage(`{}`), Mode: "full"}
	if d := e.Evaluate(ctx, browserCall); d.Allow {
		t.Fatalf("platform browser forbid must still deny: %+v", d)
	}
}
