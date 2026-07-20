package eval

import (
	"context"
	"testing"
)

func TestPolicyMemStore(t *testing.T) {
	ctx := context.Background()
	m := NewPolicyMemStore()
	if err := m.PutPolicy(ctx, Policy{Tenant: "t1", AgentID: "", SampleRate: 10, Criteria: nil}); err == nil {
		t.Fatal("expected validation rejection")
	}
	p := Policy{Tenant: "t1", AgentID: "a1", SampleRate: 25, Criteria: []Criterion{{Name: "c", Scorer: ScorerContains, Pattern: "ok"}}}
	if err := m.PutPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	got, ok, _ := m.GetPolicy(ctx, "t1", "a1")
	if !ok || got.SampleRate != 25 || len(got.Criteria) != 1 {
		t.Fatalf("get: ok=%v %+v", ok, got)
	}
	if _, ok, _ := m.GetPolicy(ctx, "t2", "a1"); ok {
		t.Fatal("cross-tenant leak")
	}
	if rows, _ := m.ListPolicies(ctx, "t2"); len(rows) != 0 {
		t.Fatal("cross-tenant list leak")
	}
	// upsert
	p.SampleRate = 80
	_ = m.PutPolicy(ctx, p)
	got2, _, _ := m.GetPolicy(ctx, "t1", "a1")
	if got2.SampleRate != 80 {
		t.Fatalf("upsert failed: %d", got2.SampleRate)
	}
	ok2, _ := m.DeletePolicy(ctx, "t1", "a1")
	if !ok2 {
		t.Fatal("delete returned false")
	}
	if _, ok, _ := m.GetPolicy(ctx, "t1", "a1"); ok {
		t.Fatal("still present after delete")
	}
}
