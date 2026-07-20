package memory

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestStartGCLoop verifies the ticker calls the sweep repeatedly and stops on
// ctx cancel, and that onReap receives each pass's count. It drives the loop via
// startGCLoop (the DB-free seam StartGC delegates to).
func TestStartGCLoop(t *testing.T) {
	var sweeps int32
	var reaped int32
	sweep := func(ctx context.Context) (int, error) {
		atomic.AddInt32(&sweeps, 1)
		return 7, nil
	}
	onReap := func(n int) { atomic.AddInt32(&reaped, int32(n)) }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		startGCLoop(ctx, 5*time.Millisecond, sweep, onReap)
		close(done)
	}()
	time.Sleep(40 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("startGCLoop did not return after cancel")
	}
	if atomic.LoadInt32(&sweeps) < 2 {
		t.Fatalf("sweeps = %d, want >= 2", sweeps)
	}
	if got := atomic.LoadInt32(&reaped); got != atomic.LoadInt32(&sweeps)*7 {
		t.Fatalf("reaped = %d, want sweeps*7 = %d", got, sweeps*7)
	}
}

// TestStartGCLoopNilOnReap ensures a nil onReap is safe.
func TestStartGCLoopNilOnReap(t *testing.T) {
	sweep := func(ctx context.Context) (int, error) { return 3, nil }
	ctx, cancel := context.WithCancel(context.Background())
	go startGCLoop(ctx, 5*time.Millisecond, sweep, nil)
	time.Sleep(20 * time.Millisecond)
	cancel() // must not panic
}

// TestStartGCLoopErrorContinues ensures a sweep error does not stop the loop.
func TestStartGCLoopErrorContinues(t *testing.T) {
	var sweeps int32
	sweep := func(ctx context.Context) (int, error) {
		atomic.AddInt32(&sweeps, 1)
		return 0, context.DeadlineExceeded // arbitrary non-nil error
	}
	ctx, cancel := context.WithCancel(context.Background())
	go startGCLoop(ctx, 5*time.Millisecond, sweep, nil)
	time.Sleep(30 * time.Millisecond)
	cancel()
	if atomic.LoadInt32(&sweeps) < 2 {
		t.Fatalf("sweeps = %d, want >= 2 (loop must survive errors)", sweeps)
	}
}
