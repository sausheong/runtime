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
