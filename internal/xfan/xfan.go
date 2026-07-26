// Package xfan runs bounded, cancellable fan-out over an index range.
//
// Registry- and session-derived handlers previously started one goroutine per
// member per request, so a single inbound request could multiply into an
// unbounded number of outbound requests. Each caps that multiplication.
package xfan

import (
	"context"
	"sync"
)

// DefaultLimit is the concurrency ceiling callers use unless they have a
// reason to differ: high enough that fan-out latency stays flat for a normal
// fleet, low enough that one request cannot open hundreds of sockets.
const DefaultLimit = 16

// Each runs fn for each index in [0, n) with at most limit invocations running
// concurrently, and returns only after every started call has returned.
//
// ctx is passed to fn and also gates scheduling: once ctx is done Each stops
// starting new indices, but already-running calls are awaited, so Each never
// returns while it still owns a live goroutine. fn is responsible for honouring
// ctx. A limit below 1 becomes DefaultLimit.
//
// Cancellation therefore means SOME indices may not run at all. Callers that
// write into a pre-sized slice must treat unwritten entries as absent rather
// than as zero-valued results — an unrun index left as a zero struct would be
// serialized as a real, empty record.
func Each(ctx context.Context, n, limit int, fn func(ctx context.Context, i int)) {
	if n <= 0 {
		return
	}
	if limit < 1 {
		limit = DefaultLimit
	}
	if limit > n {
		limit = n
	}
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		// Checked before the select because a select with two ready cases picks
		// at random: relying on it alone starts work on an already-cancelled
		// context roughly half the time.
		if ctx.Err() != nil {
			wg.Wait() // stop scheduling, but never abandon running work
			return
		}
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(i int) {
			defer func() { <-sem; wg.Done() }()
			fn(ctx, i)
		}(i)
	}
	wg.Wait()
}
