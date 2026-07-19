package quota

import (
	"context"
	"testing"
)

// testStoreConformance exercises the QuotaStore contract against both stores.
func testStoreConformance(t *testing.T, s QuotaStore) {
	ctx := context.Background()
	if err := s.Insert(ctx, Rule{Tenant: "acme", Upstream: "orders", RatePerMin: 60}); err != nil {
		t.Fatal(err)
	}
	if err := s.Insert(ctx, Rule{Tenant: "acme", Upstream: "*", RatePerMin: 120}); err != nil {
		t.Fatal(err)
	}
	if err := s.Insert(ctx, Rule{Tenant: "globex", Upstream: "orders", RatePerMin: 10}); err != nil {
		t.Fatal(err)
	}

	// List is tenant-scoped.
	rows, err := s.List(ctx, "acme")
	if err != nil || len(rows) != 2 {
		t.Fatalf("List(acme) = %v, %v (want 2)", rows, err)
	}
	// Rules returns all + a generation.
	all, gen1, err := s.Rules(ctx)
	if err != nil || len(all) != 3 {
		t.Fatalf("Rules = %v gen=%d err=%v (want 3)", all, gen1, err)
	}
	// Reject non-positive rate and empty keys.
	if err := s.Insert(ctx, Rule{Tenant: "acme", Upstream: "x", RatePerMin: 0}); err == nil {
		t.Error("rate 0 must be rejected")
	}
	if err := s.Insert(ctx, Rule{Tenant: "", Upstream: "x", RatePerMin: 5}); err == nil {
		t.Error("empty tenant must be rejected")
	}
	// Duplicate (tenant,upstream) rejected.
	if err := s.Insert(ctx, Rule{Tenant: "acme", Upstream: "orders", RatePerMin: 99}); err == nil {
		t.Error("duplicate key must be rejected")
	}
	// Delete bumps generation.
	ok, err := s.Delete(ctx, "acme", "orders")
	if err != nil || !ok {
		t.Fatalf("Delete = %v, %v", ok, err)
	}
	_, gen2, _ := s.Rules(ctx)
	if gen2 == gen1 {
		t.Error("generation must change after delete")
	}
	if ok, _ := s.Delete(ctx, "acme", "ghost"); ok {
		t.Error("deleting a missing rule must report false")
	}
}

func TestMemStoreConformance(t *testing.T) { testStoreConformance(t, NewMemStore()) }
