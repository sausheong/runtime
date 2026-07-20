package eval

import (
	"context"
	"testing"
)

func TestMemStoreSetsAndRuns(t *testing.T) {
	ctx := context.Background()
	m := NewMemStore()
	// bad set rejected
	if err := m.PutSet(ctx, Set{Tenant: "t1", Name: "", Cases: nil}); err == nil {
		t.Fatal("expected validation rejection")
	}
	s := Set{Tenant: "t1", Name: "greet", Cases: []Case{{Input: "hi", Scorer: ScorerExact, Expected: "hello"}}}
	if err := m.PutSet(ctx, s); err != nil {
		t.Fatal(err)
	}
	got, ok, _ := m.GetSet(ctx, "t1", "greet")
	if !ok || len(got.Cases) != 1 {
		t.Fatalf("get set: ok=%v cases=%d", ok, len(got.Cases))
	}
	// tenant isolation
	if _, ok, _ := m.GetSet(ctx, "t2", "greet"); ok {
		t.Fatal("cross-tenant set leaked")
	}
	if rows, _ := m.ListSets(ctx, "t2"); len(rows) != 0 {
		t.Fatal("cross-tenant list leaked")
	}
	// run + results
	r := Run{RunID: "r1", Tenant: "t1", SetName: "greet", AgentID: "a1", Status: StatusPending}
	if err := m.CreateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	_ = m.PutResult(ctx, "r1", Result{CaseIndex: 0, Input: "hi", Output: "hello", Scorer: "exact", Passed: true})
	_ = m.FinishRun(ctx, "r1", StatusCompleted, 1, 1, 0, 1.0, "")
	gr, ok, _ := m.GetRun(ctx, "r1")
	if !ok || gr.Status != StatusCompleted || gr.Score != 1.0 || gr.FinishedAt == nil {
		t.Fatalf("finish run: %+v", gr)
	}
	res, _ := m.ListResults(ctx, "r1")
	if len(res) != 1 || !res[0].Passed {
		t.Fatalf("results: %+v", res)
	}
	// run tenant isolation via ListRuns
	if rows, _ := m.ListRuns(ctx, "t2"); len(rows) != 0 {
		t.Fatal("cross-tenant run list leaked")
	}
}
