package xfan

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestEachBoundsPeakConcurrency proves the limit is a real ceiling, not a hint.
func TestEachBoundsPeakConcurrency(t *testing.T) {
	const n, limit = 200, 4
	var cur, peak atomic.Int64
	Each(context.Background(), n, limit, func(_ context.Context, _ int) {
		c := cur.Add(1)
		// CAS-raise the high-water mark: no lock, and no lost update from a
		// racing goroutine that observed a smaller peak.
		for {
			p := peak.Load()
			if c <= p || peak.CompareAndSwap(p, c) {
				break
			}
		}
		time.Sleep(time.Millisecond)
		cur.Add(-1)
	})
	if p := peak.Load(); p > limit {
		t.Fatalf("peak concurrency = %d, want <= %d", p, limit)
	}
	if p := peak.Load(); p < 2 {
		t.Fatalf("peak concurrency = %d, want real parallelism", p)
	}
}

// TestEachRunsEveryIndexExactlyOnce guards against a scheduling bug that
// silently drops or duplicates work.
func TestEachRunsEveryIndexExactlyOnce(t *testing.T) {
	const n = 500
	seen := make([]atomic.Int64, n)
	Each(context.Background(), n, 8, func(_ context.Context, i int) { seen[i].Add(1) })
	for i := range seen {
		if got := seen[i].Load(); got != 1 {
			t.Fatalf("index %d ran %d times, want 1", i, got)
		}
	}
}

// TestEachReturnsAndDrainsOnCancel proves cancellation stops scheduling and
// that Each never leaks a goroutine past its return.
func TestEachReturnsAndDrainsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var started, finished atomic.Int64
	done := make(chan struct{})
	go func() {
		Each(ctx, 1000, 4, func(c context.Context, _ int) {
			started.Add(1)
			<-c.Done()
			finished.Add(1)
		})
		close(done)
	}()
	for started.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Each did not return after cancellation")
	}
	if s, f := started.Load(), finished.Load(); s != f {
		t.Fatalf("started=%d finished=%d: Each returned with work still running", s, f)
	}
	if started.Load() >= 1000 {
		t.Fatal("cancellation did not stop scheduling new work")
	}
}

func TestEachHandlesDegenerateInput(t *testing.T) {
	Each(context.Background(), 0, 4, func(context.Context, int) { t.Fatal("ran for n=0") })
	var ran atomic.Int64
	Each(context.Background(), 3, 0, func(context.Context, int) { ran.Add(1) }) // limit<1 ⇒ default
	if ran.Load() != 3 {
		t.Fatalf("limit<1 ran %d, want 3 via default", ran.Load())
	}
}
