package controlplane

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestSupervisor_RestartsOnExit(t *testing.T) {
	var spawns int32
	spawn := func(ctx context.Context) <-chan error {
		atomic.AddInt32(&spawns, 1)
		ch := make(chan error, 1)
		go func() {
			time.Sleep(10 * time.Millisecond)
			ch <- nil // process "exited"
		}()
		return ch
	}

	sup := &Supervisor{Spawn: spawn, Backoff: time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	go sup.Run(ctx)
	time.Sleep(60 * time.Millisecond)
	cancel()

	if got := atomic.LoadInt32(&spawns); got < 2 {
		t.Fatalf("expected >=2 spawns (restart), got %d", got)
	}
}

func TestSupervisor_StopsOnContextCancel(t *testing.T) {
	var spawns int32
	spawn := func(ctx context.Context) <-chan error {
		atomic.AddInt32(&spawns, 1)
		ch := make(chan error, 1)
		go func() { <-ctx.Done(); ch <- ctx.Err() }()
		return ch
	}
	sup := &Supervisor{Spawn: spawn, Backoff: time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { sup.Run(ctx); close(done) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
	if got := atomic.LoadInt32(&spawns); got != 1 {
		t.Fatalf("expected exactly 1 spawn before cancel, got %d", got)
	}
}

func TestNextBackoff_DoublesAndCaps(t *testing.T) {
	max := 30 * time.Second
	cur := time.Second
	want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}
	for i, w := range want {
		cur = nextBackoff(cur, max)
		if cur != w {
			t.Fatalf("step %d: nextBackoff = %v, want %v", i, cur, w)
		}
	}
}

func TestSupervisorOnRestartFires(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	restarts := 0
	spawns := 0
	s := &Supervisor{
		Backoff:   time.Millisecond,
		OnRestart: func() { restarts++ },
		Spawn: func(ctx context.Context) <-chan error {
			spawns++
			ch := make(chan error, 1)
			if spawns >= 3 {
				cancel()
			}
			ch <- nil
			return ch
		},
	}
	s.Run(ctx)
	if restarts != spawns-1 {
		t.Fatalf("restarts = %d, want spawns-1 = %d (OnRestart must fire before every respawn but never the first spawn)", restarts, spawns-1)
	}
	if restarts < 2 {
		t.Fatalf("restarts = %d, want >= 2 (spawns=%d)", restarts, spawns)
	}
}

func TestSupervisor_FastFailuresGrowBackoff(t *testing.T) {
	// A spawn that fails instantly (like buildEnv failing closed on a bad
	// secret) must NOT respawn in a tight loop: backoff grows, so the number of
	// spawns in a fixed window is bounded well below base-rate.
	var spawns int32
	spawn := func(ctx context.Context) <-chan error {
		atomic.AddInt32(&spawns, 1)
		ch := make(chan error, 1)
		ch <- context.DeadlineExceeded // instant failure, ~0 uptime
		return ch
	}
	// base 5ms, cap 40ms. With fixed 5ms backoff a 200ms window would allow ~40
	// spawns; exponential (5,10,20,40,40,...) allows far fewer.
	sup := &Supervisor{Spawn: spawn, Backoff: 5 * time.Millisecond, MaxBackoff: 40 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	go sup.Run(ctx)
	time.Sleep(200 * time.Millisecond)
	cancel()
	got := atomic.LoadInt32(&spawns)
	if got == 0 {
		t.Fatal("expected at least 1 spawn")
	}
	if got > 15 {
		t.Fatalf("backoff did not grow: %d spawns in 200ms (fixed-rate would be ~40; exponential should be ~8-12)", got)
	}
}
