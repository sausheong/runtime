package controlplane

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sausheong/runtime/internal/config"
	"github.com/sausheong/runtime/internal/store"
)

// newTestPM builds a PoolManager with an injected spawn that never starts a real
// process: grow's readiness wait returns immediately. spawned records indices.
func newTestPM(t *testing.T, min, max, target int) (*PoolManager, *[]int) {
	t.Helper()
	var mu sync.Mutex
	var spawned []int
	base := AgentProcess{AgentID: "ag", BinPath: "/bin/true", PGDSN: "dsn", Tenant: "default"}
	acfg := config.AutoscaleConfig{Min: min, Max: max, TargetSessionsPerReplica: target}
	addrOf := func(i int) (string, error) { return fmt.Sprintf("127.0.0.1:%d", 9100+i), nil }
	pm := newPoolManager("ag", base, acfg, addrOf, nil, nil)
	pm.startReplica = func(ctx context.Context, ap AgentProcess) (context.CancelFunc, error) {
		mu.Lock()
		spawned = append(spawned, ap.ReplicaIndex)
		mu.Unlock()
		_, cancel := context.WithCancel(ctx)
		return cancel, nil
	}
	return pm, &spawned
}

func TestPoolManagerGrowAppendsSuffix(t *testing.T) {
	pm, _ := newTestPM(t, 1, 3, 2)
	ctx := context.Background()
	if err := pm.grow(ctx); err != nil {
		t.Fatal(err)
	}
	if err := pm.grow(ctx); err != nil {
		t.Fatal(err)
	}
	reps := pm.Replicas()
	if len(reps) != 2 {
		t.Fatalf("len=%d want 2", len(reps))
	}
	if reps[1].ReplicaIndex != 1 || reps[1].Addr != "127.0.0.1:9101" || reps[1].DBOSVMID != "ag#1" {
		t.Fatalf("replica 1 wrong: %+v", reps[1])
	}
}

func TestPoolManagerGrowRespectsMax(t *testing.T) {
	pm, _ := newTestPM(t, 1, 2, 2)
	ctx := context.Background()
	_ = pm.grow(ctx)
	_ = pm.grow(ctx)
	if err := pm.grow(ctx); err == nil {
		t.Fatalf("grow past max should error")
	}
}

func TestPoolManagerDrainAndReap(t *testing.T) {
	pm, _ := newTestPM(t, 1, 3, 2)
	ctx := context.Background()
	_ = pm.grow(ctx)
	_ = pm.grow(ctx)
	pm.drainTop()
	if !pm.topDraining() {
		t.Fatal("top not marked draining")
	}
	for i := 0; i < 10; i++ {
		if pm.NextReplica() == 2 {
			t.Fatal("NextReplica returned draining replica")
		}
	}
	pm.reapDrained(map[int]int{0: 1, 1: 1})
	if len(pm.Replicas()) != 2 {
		t.Fatalf("reap did not truncate: k=%d", len(pm.Replicas()))
	}
}

func TestPoolManagerReapOnlyContiguousZero(t *testing.T) {
	pm, _ := newTestPM(t, 1, 3, 2)
	ctx := context.Background()
	_ = pm.grow(ctx)
	_ = pm.grow(ctx)
	_ = pm.grow(ctx)
	pm.drainTop()
	pm.reapDrained(map[int]int{2: 1})
	if len(pm.Replicas()) != 3 {
		t.Fatalf("reaped a non-zero replica: k=%d", len(pm.Replicas()))
	}
}

func TestPoolManagerUndrainTop(t *testing.T) {
	pm, _ := newTestPM(t, 1, 3, 2)
	ctx := context.Background()
	_ = pm.grow(ctx)
	pm.drainTop()
	pm.undrainTop()
	if pm.topDraining() {
		t.Fatal("undrainTop did not clear draining")
	}
}

func TestPoolManagerGrowRacedBranch(t *testing.T) {
	base := AgentProcess{AgentID: "ag", BinPath: "/bin/true", PGDSN: "dsn", Tenant: "default"}
	acfg := config.AutoscaleConfig{Min: 1, Max: 5, TargetSessionsPerReplica: 2}
	addrOf := func(i int) (string, error) { return fmt.Sprintf("127.0.0.1:%d", 9100+i), nil }
	pm := newPoolManager("ag", base, acfg, addrOf, nil, nil)

	release := make(chan struct{})
	entered := make(chan struct{}, 2)
	var canceled int32
	pm.startReplica = func(ctx context.Context, ap AgentProcess) (context.CancelFunc, error) {
		entered <- struct{}{} // signal we're spawning (outside pm lock)
		<-release             // block so both grows observe the same k
		return func() { atomic.AddInt32(&canceled, 1) }, nil
	}

	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { errs <- pm.grow(context.Background()) }()
	}
	// Wait until BOTH grows have entered startReplica (both saw k==0) before
	// releasing them — guarantees a real collision at the same index.
	<-entered
	<-entered
	close(release)

	e1, e2 := <-errs, <-errs
	// Exactly one grow wins (nil), the other returns errGrowRaced.
	raced := 0
	if e1 == errGrowRaced {
		raced++
	}
	if e2 == errGrowRaced {
		raced++
	}
	if raced != 1 {
		t.Fatalf("expected exactly one errGrowRaced, got e1=%v e2=%v", e1, e2)
	}
	if got := len(pm.Replicas()); got != 1 {
		t.Fatalf("expected exactly 1 published replica, got %d", got)
	}
	if atomic.LoadInt32(&canceled) != 1 {
		t.Fatalf("expected the raced loser's cancel to run exactly once, got %d", atomic.LoadInt32(&canceled))
	}
}

func TestPoolManagerReadsRaceClean(t *testing.T) {
	pm, _ := newTestPM(t, 1, 4, 2)
	ctx := context.Background()
	_ = pm.grow(ctx)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = pm.NextReplica()
				_ = pm.Replicas()
				_, _ = pm.Replica(0)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = pm.grow(ctx)
		pm.drainTop()
		pm.reapDrained(map[int]int{0: 1})
	}()
	wg.Wait()
}

func TestDecideStep(t *testing.T) {
	acfg := config.AutoscaleConfig{Min: 1, Max: 3, TargetSessionsPerReplica: 2}
	cases := []struct {
		name      string
		active, k int
		topDrain  bool
		upReady   bool
		downReady bool
		want      scaleStep
	}{
		{"need up, ready", 5, 1, false, true, true, stepGrow},
		{"need up, cooling", 5, 1, false, false, true, stepBlocked},
		{"at max", 99, 3, false, true, true, stepBlocked},
		{"need down, ready", 1, 3, false, true, true, stepDrain},
		{"need down, cooling", 1, 3, false, true, false, stepBlocked},
		{"at min", 0, 1, false, true, true, stepNone},
		{"rebound undrain", 5, 2, true, true, true, stepUndrain},
		{"hold steady", 3, 2, false, true, true, stepNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := decideStep(acfg, c.active, c.k, c.topDrain, c.upReady, c.downReady)
			if got != c.want {
				t.Fatalf("decideStep(active=%d,k=%d,drain=%v,up=%v,down=%v)=%v want %v",
					c.active, c.k, c.topDrain, c.upReady, c.downReady, got, c.want)
			}
		})
	}
}

// fakeLoad is a store.Store stub returning a scripted active-by-replica map.
type fakeLoad struct {
	store.Store
	mu  sync.Mutex
	ret map[int]int
	err error
}

func (f *fakeLoad) ActiveSessionsByReplica(_ context.Context, _ string) (map[int]int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	m := map[int]int{}
	for k, v := range f.ret {
		m[k] = v
	}
	return m, nil
}

func TestTickGrowsOnLoad(t *testing.T) {
	pm, _ := newTestPM(t, 1, 3, 2)
	pm.st = &fakeLoad{ret: map[int]int{0: 5}}
	ctx := context.Background()
	if err := pm.grow(ctx); err != nil {
		t.Fatal(err)
	}
	// Advancing clock: +20s per call, so the 10s up-cooldown elapses between the
	// two ticks and both are allowed to grow (proving one-step-per-tick AND that
	// the cooldown gate permits spaced actuations).
	var now int64
	pm.clock = func() int64 { now += int64(20 * 1e9); return now }
	pm.tick(ctx) // active 5 over k=1 ⇒ grow ⇒ k=2
	if got := len(pm.Replicas()); got != 2 {
		t.Fatalf("k=%d want 2 (one step per tick)", got)
	}
	pm.tick(ctx) // active 5 over k=2 ⇒ grow ⇒ k=3
	if got := len(pm.Replicas()); got != 3 {
		t.Fatalf("k=%d want 3", got)
	}
}

func TestTickUpCooldownGates(t *testing.T) {
	pm, _ := newTestPM(t, 1, 3, 2)
	pm.st = &fakeLoad{ret: map[int]int{0: 5}} // wants to grow
	ctx := context.Background()
	if err := pm.grow(ctx); err != nil {
		t.Fatal(err)
	}
	// Constant clock: the first tick grows (lastUp starts at 0, now huge ⇒ ready),
	// the second tick is WITHIN the cooldown (now-lastUp == 0 < upCD) ⇒ no grow.
	pm.clock = func() int64 { return int64(1 << 60) }
	pm.tick(ctx)
	if got := len(pm.Replicas()); got != 2 {
		t.Fatalf("first tick k=%d want 2", got)
	}
	pm.tick(ctx) // same instant ⇒ cooldown not elapsed ⇒ held
	if got := len(pm.Replicas()); got != 2 {
		t.Fatalf("second tick k=%d want 2 (up-cooldown should gate)", got)
	}
}

func TestTickFailedReadIsNoOp(t *testing.T) {
	pm, _ := newTestPM(t, 1, 3, 2)
	pm.st = &fakeLoad{err: fmt.Errorf("db down")}
	ctx := context.Background()
	_ = pm.grow(ctx)
	pm.clock = func() int64 { return 1 << 60 }
	pm.tick(ctx)
	if got := len(pm.Replicas()); got != 1 {
		t.Fatalf("k=%d want 1 (no scaling on failed read)", got)
	}
}

func TestTickReapsDrainedTop(t *testing.T) {
	pm, _ := newTestPM(t, 1, 3, 2)
	ctx := context.Background()
	_ = pm.grow(ctx)
	pm.drainTop()
	pm.st = &fakeLoad{ret: map[int]int{0: 1}}
	pm.clock = func() int64 { return 1 << 60 }
	pm.tick(ctx)
	if got := len(pm.Replicas()); got != 1 {
		t.Fatalf("k=%d want 1 (drained top reaped)", got)
	}
}

func TestTickDrainAndReapSeparatedByTick(t *testing.T) {
	pm, _ := newTestPM(t, 1, 3, 2)
	ctx := context.Background()
	if err := pm.grow(ctx); err != nil { // k=1
		t.Fatal(err)
	}
	if err := pm.grow(ctx); err != nil { // k=2 (pool starts empty; need two grows)
		t.Fatal(err)
	}
	// Low load (total=1 over target=2 ⇒ desired=1 < k=2) so the top (index 1),
	// which has 0 active, should be drained. Constant clock ⇒ cooldowns elapsed.
	pm.st = &fakeLoad{ret: map[int]int{0: 1}} // index 1 absent ⇒ 0 active
	pm.clock = func() int64 { return 1 << 60 }

	pm.tick(ctx) // newly drains index 1 this tick ⇒ must NOT reap it yet
	if got := len(pm.Replicas()); got != 2 {
		t.Fatalf("after drain tick k=%d, want 2 (newly-drained top must not be reaped same tick)", got)
	}
	if !pm.topDraining() {
		t.Fatal("top should be draining after the drain tick")
	}

	pm.tick(ctx) // top already draining ⇒ drainTop no-op ⇒ reapDrained runs ⇒ reap
	if got := len(pm.Replicas()); got != 1 {
		t.Fatalf("after second tick k=%d, want 1 (prior-tick-drained top reaped)", got)
	}
}
