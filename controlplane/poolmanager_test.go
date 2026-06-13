package controlplane

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/sausheong/runtime/internal/config"
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
