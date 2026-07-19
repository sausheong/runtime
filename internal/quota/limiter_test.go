package quota

import (
	"context"
	"testing"
	"time"
)

func TestResolutionMostSpecificWins(t *testing.T) {
	ms := NewMemStore()
	ctx := context.Background()
	_ = ms.Insert(ctx, Rule{Tenant: "acme", Upstream: "orders", RatePerMin: 1})
	_ = ms.Insert(ctx, Rule{Tenant: "acme", Upstream: "*", RatePerMin: 100})
	_ = ms.Insert(ctx, Rule{Tenant: "*", Upstream: "orders", RatePerMin: 100})
	l := NewLimiter(ms, 0, nil)

	// (acme, orders) resolves to the most specific rule (rate 1): first call ok,
	// second denied within the same minute.
	if ok, _ := l.Allow(ctx, "acme", "orders"); !ok {
		t.Fatal("first call must pass")
	}
	if ok, ra := l.Allow(ctx, "acme", "orders"); ok || ra <= 0 {
		t.Fatalf("second call must be denied with a retry-after (ok=%v ra=%v)", ok, ra)
	}
	// (acme, other) falls back to (acme,*) rate 100 — passes.
	if ok, _ := l.Allow(ctx, "acme", "other"); !ok {
		t.Fatal("(acme,*) fallback must pass")
	}
	// No rule for (globex, x) and no env default ⇒ unlimited.
	if ok, _ := l.Allow(ctx, "globex", "x"); !ok {
		t.Fatal("no matching rule ⇒ unlimited")
	}
}

func TestBucketRefill(t *testing.T) {
	ms := NewMemStore()
	_ = ms.Insert(context.Background(), Rule{Tenant: "acme", Upstream: "u", RatePerMin: 60}) // 1 token/sec
	now := time.Unix(0, 0)
	l := NewLimiter(ms, 0, func() time.Time { return now })
	ctx := context.Background()
	// Burst = capacity = 60; drain them.
	for i := 0; i < 60; i++ {
		if ok, _ := l.Allow(ctx, "acme", "u"); !ok {
			t.Fatalf("burst token %d must pass", i)
		}
	}
	if ok, _ := l.Allow(ctx, "acme", "u"); ok {
		t.Fatal("bucket must be empty after draining capacity")
	}
	// Advance 1s ⇒ 1 token refilled.
	now = now.Add(time.Second)
	if ok, _ := l.Allow(ctx, "acme", "u"); !ok {
		t.Fatal("one token must refill after 1s")
	}
}

func TestRefreshThrottledWithinWindow(t *testing.T) {
	ms := NewMemStore()
	ctx := context.Background()
	// Start with a generous limit so the first Allow loads a rule (l.loaded=true).
	_ = ms.Insert(ctx, Rule{Tenant: "acme", Upstream: "u", RatePerMin: 100})
	now := time.Unix(0, 0)
	l := NewLimiter(ms, 0, func() time.Time { return now })

	// First call loads the rules (loaded==false ⇒ throttle skipped) and records
	// lastRefresh at t=0.
	if ok, _ := l.Allow(ctx, "acme", "u"); !ok {
		t.Fatal("first call must pass")
	}

	// Mutate the store within the refresh window: replace with a strict rate 1.
	if _, err := ms.Delete(ctx, "acme", "u"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_ = ms.Insert(ctx, Rule{Tenant: "acme", Upstream: "u", RatePerMin: 1})

	// Still within the 2s window: the mutation is NOT observed — the limiter must
	// keep using the cached rate-100 bucket, so calls keep passing.
	now = now.Add(time.Second) // < refreshWindow (2s)
	for i := 0; i < 5; i++ {
		if ok, _ := l.Allow(ctx, "acme", "u"); !ok {
			t.Fatalf("call %d within window must still use cached rate-100 rule", i)
		}
	}

	// Advance past the window: the re-query now observes the rate-1 rule. The
	// changed rate rebuilds the bucket full (capacity 1), so one call passes...
	now = now.Add(3 * time.Second) // total 4s > window
	if ok, _ := l.Allow(ctx, "acme", "u"); !ok {
		t.Fatal("first call after window must pass (fresh rate-1 bucket)")
	}
	// ...and the next is denied under the now-observed rate-1 limit.
	if ok, ra := l.Allow(ctx, "acme", "u"); ok || ra <= 0 {
		t.Fatalf("second call after window must be denied by rate-1 rule (ok=%v ra=%v)", ok, ra)
	}
}

func TestSeedForcesRefresh(t *testing.T) {
	ms := NewMemStore()
	ctx := context.Background()
	_ = ms.Insert(ctx, Rule{Tenant: "acme", Upstream: "u", RatePerMin: 100})
	now := time.Unix(0, 0)
	l := NewLimiter(ms, 0, func() time.Time { return now })

	if ok, _ := l.Allow(ctx, "acme", "u"); !ok {
		t.Fatal("first call must pass")
	}
	// Within the window a store mutation would be throttled, but Seed sets
	// loaded=false which forces the next Allow to re-read the store immediately.
	if _, err := ms.Delete(ctx, "acme", "u"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_ = ms.Insert(ctx, Rule{Tenant: "acme", Upstream: "u", RatePerMin: 1})
	l.Seed(nil)

	if ok, _ := l.Allow(ctx, "acme", "u"); !ok {
		t.Fatal("first call after Seed must pass (fresh rate-1 bucket)")
	}
	if ok, ra := l.Allow(ctx, "acme", "u"); ok || ra <= 0 {
		t.Fatalf("Seed must force refresh; rate-1 rule must deny (ok=%v ra=%v)", ok, ra)
	}
}

func TestEnvDefaultIsStarStarFloor(t *testing.T) {
	ms := NewMemStore()
	l := NewLimiter(ms, 1, nil) // env default rate 1 = (*,*)
	ctx := context.Background()
	if ok, _ := l.Allow(ctx, "acme", "u"); !ok {
		t.Fatal("first call under env default must pass")
	}
	if ok, _ := l.Allow(ctx, "acme", "u"); ok {
		t.Fatal("env default (*,*) rate 1 must deny the second call")
	}
}
