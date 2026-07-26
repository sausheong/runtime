package eval

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeInvoker struct {
	out   map[string]string // input -> output
	err   map[string]error  // input -> error
	calls map[string]int
}

func (f fakeInvoker) Invoke(_ context.Context, _, input string) (string, error) {
	if f.calls != nil {
		f.calls[input]++
	}
	if e := f.err[input]; e != nil {
		return "", e
	}
	return f.out[input], nil
}

func TestExecuteResumesFromPersistedCaseIndexes(t *testing.T) {
	ctx := context.Background()
	m := NewMemStore()
	_ = m.PutSet(ctx, Set{Tenant: "t1", Name: "s", Cases: []Case{
		{Input: "already", Scorer: ScorerExact, Expected: "A"},
		{Input: "remaining", Scorer: ScorerExact, Expected: "B"},
	}})
	_ = m.CreateRun(ctx, Run{RunID: "r", Tenant: "t1", SetName: "s", AgentID: "a1", Status: StatusPending})
	now := time.Now().UTC()
	_, _ = m.ClaimRun(ctx, "r", "seed", now, now.Add(time.Minute))
	_, _ = m.PutResultClaimed(ctx, "r", "seed", Result{CaseIndex: 0, Input: "already", Output: "A", Scorer: string(ScorerExact), Passed: true})
	expireMemLease(t, m, "r")
	calls := map[string]int{}
	inv := fakeInvoker{out: map[string]string{"remaining": "B"}, calls: calls}

	Execute(ctx, m, inv, nil, "r", nil)

	if calls["already"] != 0 || calls["remaining"] != 1 {
		t.Fatalf("invocation calls=%v, want only remaining case once", calls)
	}
	run, _, _ := m.GetRun(ctx, "r")
	if run.Status != StatusCompleted || run.Total != 2 || run.Passed != 2 {
		t.Fatalf("resumed run=%+v", run)
	}
	results, _ := m.ListResults(ctx, "r")
	if len(results) != 2 {
		t.Fatalf("results=%d want 2", len(results))
	}
}

type nopMetric struct{ runs, cases int }

func (n *nopMetric) EvalRun(_, _ string)  { n.runs++ }
func (n *nopMetric) EvalCase(_, _ string) { n.cases++ }

func TestRunScoresAllCasesAndCompletes(t *testing.T) {
	ctx := context.Background()
	m := NewMemStore()
	set := Set{Tenant: "t1", Name: "s", Cases: []Case{
		{Input: "a", Scorer: ScorerExact, Expected: "A"}, // pass
		{Input: "b", Scorer: ScorerExact, Expected: "B"}, // fail (output "x")
		{Input: "c", Scorer: ScorerJudge, Rubric: "ok"},  // judge error → fail-the-case
		{Input: "d", Scorer: ScorerExact, Expected: "D"}, // invoke error → fail-the-case
	}}
	_ = m.PutSet(ctx, set)
	_ = m.CreateRun(ctx, Run{RunID: "r", Tenant: "t1", SetName: "s", AgentID: "a1", Status: StatusPending})
	inv := fakeInvoker{
		out: map[string]string{"a": "A", "b": "x", "c": "whatever"},
		err: map[string]error{"d": errors.New("boom")},
	}
	j := fakeJudge{err: errors.New("judge down")}
	met := &nopMetric{}
	Execute(ctx, m, inv, j, "r", met)

	gr, _, _ := m.GetRun(ctx, "r")
	if gr.Status != StatusCompleted {
		t.Fatalf("status=%s want completed (a judge/invoke error must NOT abort the run)", gr.Status)
	}
	if gr.Total != 4 || gr.Passed != 1 || gr.Failed != 3 {
		t.Fatalf("counts total=%d passed=%d failed=%d want 4/1/3", gr.Total, gr.Passed, gr.Failed)
	}
	res, _ := m.ListResults(ctx, "r")
	if len(res) != 4 {
		t.Fatalf("results=%d want 4", len(res))
	}
	if met.cases != 4 || met.runs != 1 {
		t.Fatalf("metrics cases=%d runs=%d want 4/1", met.cases, met.runs)
	}
}

type blockingInvoker struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingInvoker) Invoke(ctx context.Context, _, _ string) (string, error) {
	b.once.Do(func() { close(b.started) })
	select {
	case <-b.release:
		return "A", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func TestExecuteLeasePreventsDuplicateWorker(t *testing.T) {
	ctx := context.Background()
	st := NewMemStore()
	if err := st.PutSet(ctx, Set{Tenant: "t", Name: "s", Cases: []Case{
		{Input: "a", Scorer: ScorerExact, Expected: "A"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateRun(ctx, Run{RunID: "r", Tenant: "t", SetName: "s", AgentID: "a", Status: StatusPending}); err != nil {
		t.Fatal(err)
	}
	inv := &blockingInvoker{started: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- ExecuteChecked(ctx, st, inv, nil, "r", nil) }()
	select {
	case <-inv.started:
	case <-time.After(time.Second):
		t.Fatal("first worker did not start")
	}

	err := ExecuteChecked(ctx, st, inv, nil, "r", nil)
	if !errors.Is(err, ErrRunNotClaimed) {
		t.Fatalf("second worker error=%v, want ErrRunNotClaimed", err)
	}
	close(inv.release)
	if err := <-done; err != nil {
		t.Fatalf("first worker: %v", err)
	}
}

type finishFailStore struct {
	EvalStore
	err error
}

func (f finishFailStore) FinishRunClaimed(context.Context, string, string, string, int, int, int, float64, string) (bool, error) {
	return false, f.err
}

func TestExecuteReportsFinalizationFailureAndDoesNotEmitRunMetric(t *testing.T) {
	ctx := context.Background()
	base := NewMemStore()
	if err := base.PutSet(ctx, Set{Tenant: "t", Name: "s", Cases: []Case{
		{Input: "a", Scorer: ScorerExact, Expected: "A"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := base.CreateRun(ctx, Run{RunID: "r", Tenant: "t", SetName: "s", AgentID: "a", Status: StatusPending}); err != nil {
		t.Fatal(err)
	}
	want := errors.New("finish unavailable")
	met := &nopMetric{}
	err := ExecuteChecked(ctx, finishFailStore{EvalStore: base, err: want}, fakeInvoker{
		out: map[string]string{"a": "A"},
	}, nil, "r", met)
	if !errors.Is(err, want) {
		t.Fatalf("error=%v, want %v", err, want)
	}
	if met.runs != 0 {
		t.Fatalf("run metrics=%d, want 0 before durable finalization", met.runs)
	}
}

type claimFailStore struct {
	EvalStore
	err error
}

func (f claimFailStore) ClaimRun(context.Context, string, string, time.Time, time.Time) (bool, error) {
	return false, f.err
}

type resultFailStore struct {
	EvalStore
	resultErr error
	finishErr error
}

func (f resultFailStore) PutResultClaimed(context.Context, string, string, Result) (bool, error) {
	return false, f.resultErr
}

func (f resultFailStore) FinishRunClaimed(context.Context, string, string, string, int, int, int, float64, string) (bool, error) {
	if f.finishErr != nil {
		return false, f.finishErr
	}
	return true, nil
}

func TestExecuteSurfacesClaimAndResultTransitionFailures(t *testing.T) {
	newRun := func(t *testing.T) *MemStore {
		t.Helper()
		st := NewMemStore()
		if err := st.PutSet(context.Background(), Set{Tenant: "t", Name: "s", Cases: []Case{
			{Input: "a", Scorer: ScorerExact, Expected: "A"},
		}}); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateRun(context.Background(), Run{
			RunID: "r", Tenant: "t", SetName: "s", AgentID: "a", Status: StatusPending,
		}); err != nil {
			t.Fatal(err)
		}
		return st
	}
	t.Run("claim failure", func(t *testing.T) {
		want := errors.New("claim unavailable")
		err := ExecuteChecked(context.Background(),
			claimFailStore{EvalStore: newRun(t), err: want},
			fakeInvoker{}, nil, "r", nil)
		if !errors.Is(err, want) {
			t.Fatalf("error=%v, want %v", err, want)
		}
	})
	t.Run("result failure is durably finalized", func(t *testing.T) {
		want := errors.New("result unavailable")
		met := &nopMetric{}
		err := ExecuteChecked(context.Background(),
			resultFailStore{EvalStore: newRun(t), resultErr: want},
			fakeInvoker{out: map[string]string{"a": "A"}}, nil, "r", met)
		if !errors.Is(err, want) {
			t.Fatalf("error=%v, want %v", err, want)
		}
		if met.cases != 0 || met.runs != 0 {
			t.Fatalf("metrics cases=%d runs=%d, want 0/0 before persisted result/final state", met.cases, met.runs)
		}
	})
	t.Run("error finalization failure is joined", func(t *testing.T) {
		resultErr := errors.New("result unavailable")
		finishErr := errors.New("error finalization unavailable")
		err := ExecuteChecked(context.Background(),
			resultFailStore{EvalStore: newRun(t), resultErr: resultErr, finishErr: finishErr},
			fakeInvoker{out: map[string]string{"a": "A"}}, nil, "r", nil)
		if !errors.Is(err, resultErr) || !errors.Is(err, finishErr) {
			t.Fatalf("error=%v, want both result and finalization failures", err)
		}
	})
}

func TestExpiredLeaseCanBeReclaimed(t *testing.T) {
	ctx := context.Background()
	st := NewMemStore()
	if err := st.CreateRun(ctx, Run{RunID: "r", Status: StatusPending}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if ok, err := st.ClaimRun(ctx, "r", "old", now, now.Add(time.Minute)); err != nil || !ok {
		t.Fatalf("initial claim ok=%v err=%v", ok, err)
	}
	expireMemLease(t, st, "r")
	if ok, err := st.ClaimRun(ctx, "r", "new", now, now.Add(time.Minute)); err != nil || !ok {
		t.Fatalf("expired lease reclaim ok=%v err=%v", ok, err)
	}
}

func TestRecoveryRetriesRunAfterForeignLeaseExpires(t *testing.T) {
	oldInterval := recoveryScanInterval
	recoveryScanInterval = 10 * time.Millisecond
	t.Cleanup(func() { recoveryScanInterval = oldInterval })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := NewMemStore()
	if err := st.PutSet(ctx, Set{Tenant: "t", Name: "s", Cases: []Case{
		{Input: "a", Scorer: ScorerExact, Expected: "A"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateRun(ctx, Run{RunID: "leased", Tenant: "t", SetName: "s", AgentID: "a", Status: StatusPending}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if ok, err := st.ClaimRun(ctx, "leased", "dead-worker", now, now.Add(40*time.Millisecond)); err != nil || !ok {
		t.Fatalf("foreign claim ok=%v err=%v", ok, err)
	}
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	if err := recoverIncomplete(ctx, st, &concurrentInvoker{started: started, release: release}, nil, nil, 1); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
		t.Fatal("recovery stole an unexpired lease")
	case <-time.After(20 * time.Millisecond):
	}
	select {
	case <-started:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("recovery did not retry after the foreign lease expired")
	}
	close(release)
}

func TestExecuteRenewsLeaseDuringLongProviderCall(t *testing.T) {
	oldLease := runLeaseDuration
	runLeaseDuration = 30 * time.Millisecond
	t.Cleanup(func() { runLeaseDuration = oldLease })

	ctx := context.Background()
	st := NewMemStore()
	if err := st.PutSet(ctx, Set{Tenant: "t", Name: "s", Cases: []Case{
		{Input: "a", Scorer: ScorerExact, Expected: "A"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateRun(ctx, Run{RunID: "long", Tenant: "t", SetName: "s", AgentID: "a", Status: StatusPending}); err != nil {
		t.Fatal(err)
	}
	inv := &blockingInvoker{started: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- ExecuteChecked(ctx, st, inv, nil, "long", nil) }()
	<-inv.started
	time.Sleep(60 * time.Millisecond)
	now := time.Now().UTC()
	if ok, err := st.ClaimRun(ctx, "long", "competitor", now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatal("competitor stole a lease during a long provider call")
	}
	close(inv.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestFailPendingRunIsCompareAndSwap(t *testing.T) {
	ctx := context.Background()
	st := NewMemStore()
	if err := st.CreateRun(ctx, Run{RunID: "pending", Status: StatusPending}); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.FailPendingRun(ctx, "pending", "queue full"); err != nil || !ok {
		t.Fatalf("first transition ok=%v err=%v", ok, err)
	}
	if ok, err := st.FailPendingRun(ctx, "pending", "again"); err != nil || ok {
		t.Fatalf("second transition ok=%v err=%v, want rejected", ok, err)
	}
}

func TestNormalSubmitterBoundsWorkersAndQueue(t *testing.T) {
	ctx := context.Background()
	st := NewMemStore()
	if err := st.PutSet(ctx, Set{Tenant: "t", Name: "s", Cases: []Case{
		{Input: "a", Scorer: ScorerExact, Expected: "A"},
	}}); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	inv := &concurrentInvoker{started: started, release: release}
	sub := newSubmitter(1, 1)
	for _, id := range []string{"one", "two", "three"} {
		if err := st.CreateRun(ctx, Run{RunID: id, Tenant: "t", SetName: "s", AgentID: "a", Status: StatusPending}); err != nil {
			t.Fatal(err)
		}
	}
	if !sub.submit(executionJob{ctx: ctx, st: st, inv: inv, runID: "one"}) {
		t.Fatal("first submit rejected")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}
	if !sub.submit(executionJob{ctx: ctx, st: st, inv: inv, runID: "two"}) {
		t.Fatal("queued submit rejected")
	}
	if sub.submit(executionJob{ctx: ctx, st: st, inv: inv, runID: "three"}) {
		t.Fatal("saturated submitter accepted excess work")
	}
	close(release)
}

func TestRecoveryConcurrencyStaysWithinWorkerBound(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := NewMemStore()
	if err := st.PutSet(ctx, Set{Tenant: "t", Name: "s", Cases: []Case{
		{Input: "a", Scorer: ScorerExact, Expected: "A"},
	}}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"one", "two", "three", "four"} {
		if err := st.CreateRun(ctx, Run{RunID: id, Tenant: "t", SetName: "s", AgentID: "a", Status: StatusPending}); err != nil {
			t.Fatal(err)
		}
	}
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	if err := recoverIncomplete(ctx, st, &concurrentInvoker{started: started, release: release}, nil, nil, 2); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("recovery worker did not start")
		}
	}
	select {
	case <-started:
		t.Fatal("recovery exceeded the two-worker bound")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("queued recovery job did not start after capacity became available")
		}
	}
}

type recordingIncompleteStore struct {
	EvalStore
	limits chan int
}

func (s recordingIncompleteStore) ListIncompleteRuns(ctx context.Context, limit int) ([]Run, error) {
	s.limits <- limit
	return s.EvalStore.ListIncompleteRuns(ctx, limit)
}

func TestRecoveryScanLoadsOnlyQueueCapacity(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	limits := make(chan int, 1)
	st := recordingIncompleteStore{EvalStore: NewMemStore(), limits: limits}
	if err := recoverIncomplete(ctx, st, fakeInvoker{}, nil, nil, 3); err != nil {
		t.Fatal(err)
	}
	select {
	case limit := <-limits:
		if limit != 12 {
			t.Fatalf("recovery scan limit = %d, want queue capacity 12", limit)
		}
	case <-time.After(time.Second):
		t.Fatal("recovery did not issue its initial bounded scan")
	}
}

type concurrentInvoker struct {
	started chan<- struct{}
	release <-chan struct{}
}

func (i *concurrentInvoker) Invoke(ctx context.Context, _, _ string) (string, error) {
	i.started <- struct{}{}
	select {
	case <-i.release:
		return "A", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
