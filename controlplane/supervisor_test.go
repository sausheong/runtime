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
