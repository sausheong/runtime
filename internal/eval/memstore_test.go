package eval

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func expireMemLease(t *testing.T, m *MemStore, runID string) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	run, ok := m.runs[runID]
	if !ok {
		t.Fatalf("run %q not found", runID)
	}
	expired := time.Now().UTC().Add(-time.Second)
	run.LeaseUntil = &expired
	m.runs[runID] = run
}

func ageCompletedMemRun(t *testing.T, m *MemStore, runID string, createdAt time.Time) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	run, ok := m.runs[runID]
	if !ok {
		t.Fatalf("run %q not found", runID)
	}
	finished := createdAt.Add(time.Minute)
	run.Status = StatusCompleted
	run.CreatedAt = createdAt
	run.FinishedAt = &finished
	run.LeaseOwner = ""
	run.LeaseUntil = nil
	m.runs[runID] = run
}

func TestEvalStoreContractHasNoUnfencedRunTransitions(t *testing.T) {
	storeType := reflect.TypeOf((*EvalStore)(nil)).Elem()
	for _, name := range []string{"SetRunStatus", "FinishRun"} {
		if _, ok := storeType.MethodByName(name); ok {
			t.Fatalf("EvalStore exposes unfenced transition %s", name)
		}
	}
}

func TestMemStoreRejectsInvalidRunStateTransitions(t *testing.T) {
	ctx := context.Background()
	m := NewMemStore()

	if err := m.CreateRun(ctx, Run{RunID: "bad", Status: StatusRunning}); !errors.Is(err, ErrInvalidRunTransition) {
		t.Fatalf("non-pending create error=%v, want ErrInvalidRunTransition", err)
	}
	if err := m.CreateRun(ctx, Run{RunID: "r", Status: StatusPending}); err != nil {
		t.Fatal(err)
	}
	if err := m.CreateRun(ctx, Run{RunID: "r", Status: StatusPending}); !errors.Is(err, ErrRunExists) {
		t.Fatalf("duplicate create error=%v, want ErrRunExists", err)
	}
	now := time.Now().UTC()
	if _, err := m.ClaimRun(ctx, "r", "", now, now.Add(time.Minute)); !errors.Is(err, ErrInvalidRunTransition) {
		t.Fatalf("empty-owner claim error=%v", err)
	}
	if _, err := m.ClaimRun(ctx, "r", "worker", now, now); !errors.Is(err, ErrInvalidRunTransition) {
		t.Fatalf("non-positive claim error=%v", err)
	}
	if ok, err := m.ClaimRun(ctx, "r", "worker", now, now.Add(time.Minute)); err != nil || !ok {
		t.Fatalf("valid claim ok=%v err=%v", ok, err)
	}
	if _, err := m.PutResultClaimed(ctx, "r", "", Result{}); !errors.Is(err, ErrInvalidRunTransition) {
		t.Fatalf("empty-owner result error=%v", err)
	}
	for _, status := range []string{StatusPending, StatusRunning, "bogus"} {
		if _, err := m.FinishRunClaimed(ctx, "r", "worker", status, 0, 0, 0, 0, ""); !errors.Is(err, ErrInvalidRunTransition) {
			t.Fatalf("final status %q error=%v", status, err)
		}
	}
	if _, err := m.FinishRunClaimed(ctx, "r", "", StatusCompleted, 0, 0, 0, 0, ""); !errors.Is(err, ErrInvalidRunTransition) {
		t.Fatalf("empty-owner finish error=%v", err)
	}
	if run, _, _ := m.GetRun(ctx, "r"); run.Status != StatusRunning || run.LeaseOwner != "worker" {
		t.Fatalf("invalid operations mutated run: %+v", run)
	}
}

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
	if ok, err := m.FinishRunClaimed(ctx, "r1", "worker", StatusCompleted, 1, 1, 0, 1.0, ""); err != nil || !ok {
		t.Fatalf("finish claimed run: ok=%v err=%v", ok, err)
	}
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
	expireMemLease(t, m, "lease-fence")
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
	expireMemLease(t, m, "lease-fence")
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
			RunID: id, Status: StatusPending,
		}); err != nil {
			t.Fatal(err)
		}
		ageCompletedMemRun(t, m, id, old)
	}
	if n, err := m.ReapBefore(ctx, time.Now().UTC().Add(-time.Hour), 2); err != nil || n != 2 {
		t.Fatalf("first bounded batch n=%d err=%v", n, err)
	}
	if runs, err := m.ListRuns(ctx, ""); err != nil || len(runs) != 1 {
		t.Fatalf("remaining runs=%d err=%v, want one", len(runs), err)
	}
}

func TestEvalRetentionAgesTerminalRunsFromCompletion(t *testing.T) {
	ctx := context.Background()
	m := NewMemStore()
	created := time.Now().UTC().Add(-48 * time.Hour)
	if err := m.CreateRun(ctx, Run{
		RunID: "long-running", Status: StatusPending, CreatedAt: created,
	}); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	run := m.runs["long-running"]
	finished := time.Now().UTC().Add(-time.Minute)
	run.Status = StatusCompleted
	run.FinishedAt = &finished
	m.runs[run.RunID] = run
	m.mu.Unlock()

	if n, err := m.ReapBefore(ctx, time.Now().UTC().Add(-24*time.Hour), 10); err != nil || n != 0 {
		t.Fatalf("newly completed long-running run reaped: n=%d err=%v", n, err)
	}
	if _, ok, err := m.GetRun(ctx, run.RunID); err != nil || !ok {
		t.Fatalf("newly completed run missing: ok=%v err=%v", ok, err)
	}
}
