package eval

import (
	"context"
	"testing"
	"time"
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
	now := time.Now().UTC()
	_, _ = m.ClaimRun(ctx, "r1", "worker", now, now.Add(time.Minute))
	_, _ = m.PutResultClaimed(ctx, "r1", "worker", Result{CaseIndex: 0, Input: "hi", Output: "hello", Scorer: "exact", Passed: true})
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

func TestMemStoreRejectsStaleLeaseWritesAndFinalization(t *testing.T) {
	ctx := context.Background()
	m := NewMemStore()
	if err := m.CreateRun(ctx, Run{
		RunID: "lease-fence", Tenant: "t", SetName: "s", AgentID: "a", Status: StatusPending,
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if ok, err := m.ClaimRun(ctx, "lease-fence", "old", now, now.Add(time.Minute)); err != nil || !ok {
		t.Fatalf("old claim: ok=%v err=%v", ok, err)
	}
	// The same owner may renew; use that path to make its lease expired, then
	// let a replacement worker claim it.
	if ok, err := m.ClaimRun(ctx, "lease-fence", "old", now, now.Add(-time.Second)); err != nil || !ok {
		t.Fatalf("expire old claim: ok=%v err=%v", ok, err)
	}
	if ok, err := m.ClaimRun(ctx, "lease-fence", "new", now, now.Add(time.Minute)); err != nil || !ok {
		t.Fatalf("new claim: ok=%v err=%v", ok, err)
	}
	if ok, err := m.PutResultClaimed(ctx, "lease-fence", "old", Result{CaseIndex: 0}); err != nil || ok {
		t.Fatalf("stale result write: ok=%v err=%v", ok, err)
	}
	if ok, err := m.FinishRunClaimed(ctx, "lease-fence", "old", StatusCompleted, 0, 0, 0, 0, ""); err != nil || ok {
		t.Fatalf("stale finish: ok=%v err=%v", ok, err)
	}
	if ok, err := m.PutResultClaimed(ctx, "lease-fence", "new", Result{CaseIndex: 0, Passed: true}); err != nil || !ok {
		t.Fatalf("live result write: ok=%v err=%v", ok, err)
	}
	if ok, err := m.ClaimRun(ctx, "lease-fence", "new", now, now.Add(-time.Second)); err != nil || !ok {
		t.Fatalf("expire new claim: ok=%v err=%v", ok, err)
	}
	if ok, err := m.PutResultClaimed(ctx, "lease-fence", "new", Result{CaseIndex: 1}); err != nil || ok {
		t.Fatalf("expired owner wrote a result: ok=%v err=%v", ok, err)
	}
	if ok, err := m.FinishRunClaimed(ctx, "lease-fence", "new", StatusCompleted, 0, 0, 0, 0, ""); err != nil || ok {
		t.Fatalf("expired owner finalized run: ok=%v err=%v", ok, err)
	}
}

func TestEvalRetentionDeletesOnlyOneBoundedBatch(t *testing.T) {
	ctx := context.Background()
	m := NewMemStore()
	old := time.Now().UTC().Add(-2 * time.Hour)
	for _, id := range []string{"old-1", "old-2", "old-3"} {
		if err := m.CreateRun(ctx, Run{
			RunID: id, Status: StatusCompleted, CreatedAt: old,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := m.ReapBefore(ctx, time.Now().UTC().Add(-time.Hour), 2); err != nil || n != 2 {
		t.Fatalf("first bounded batch n=%d err=%v", n, err)
	}
	if runs, err := m.ListRuns(ctx, ""); err != nil || len(runs) != 1 {
		t.Fatalf("remaining runs=%d err=%v, want one", len(runs), err)
	}
}
