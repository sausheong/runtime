package quota

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"time"
)

// bucket is a token bucket: capacity tokens, refilled rate/60 tokens per second.
type bucket struct {
	capacity float64
	perSec   float64
	tokens   float64
	last     time.Time
}

func (b *bucket) take(now time.Time) (bool, time.Duration) {
	// Refill for elapsed time, capped at capacity.
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens = math.Min(b.capacity, b.tokens+elapsed*b.perSec)
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	// Seconds until the next whole token.
	need := 1 - b.tokens
	secs := need / b.perSec
	return false, time.Duration(math.Ceil(secs)) * time.Second
}

// Limiter enforces per-(tenant,upstream) rate limits with an in-process token
// bucket per resolved rule. The rule set refreshes when the store's generation
// changes; buckets persist across refreshes keyed by resolved (tenant,upstream)
// and reset when their rate changes.
type Limiter struct {
	st            QuotaStore
	envDefault    int
	clock         func() time.Time
	refreshWindow time.Duration

	mu          sync.Mutex
	gen         uint64
	loaded      bool
	lastRefresh time.Time
	seed        []Rule          // file-config rules (lowest precedence under DB)
	rules       map[string]Rule // "tenant\x00upstream" -> rule (resolved set)
	buckets     map[string]*bucket
	rates       map[string]int // bucketKey -> rate that built the bucket (reset on change)
	lastUse     map[string]time.Time
}

func NewLimiter(st QuotaStore, envDefault int, clock func() time.Time) *Limiter {
	if clock == nil {
		clock = time.Now
	}
	return &Limiter{
		st: st, envDefault: envDefault, clock: clock,
		refreshWindow: 2 * time.Second,
		rules:         map[string]Rule{}, buckets: map[string]*bucket{},
		rates: map[string]int{}, lastUse: map[string]time.Time{},
	}
}

// Seed installs file-config rules. DB rules (loaded via the store) override a
// seed rule with the same key.
func (l *Limiter) Seed(rules []Rule) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seed = rules
	l.loaded = false // force a rebuild on next Allow
}

// refresh rebuilds the resolved rule map from seed + store when the generation
// moved. Caller holds l.mu. Fail-open on store error.
func (l *Limiter) refresh(ctx context.Context) {
	// Throttle store reads off the hot path: Allow runs under l.mu, and for the
	// Postgres store l.st.Rules is a full SELECT, so an unthrottled refresh runs
	// a DB round-trip on every tool call, serialized behind the mutex. Re-query
	// at most once per refreshWindow. We can't just trust l.gen: Store.gen is
	// process-local, so a rule edited on replica A never bumps replica B's gen —
	// the periodic re-query is what makes cross-replica edits eventually visible.
	// The window bounds staleness while keeping that freshness.
	if l.loaded && l.clock().Sub(l.lastRefresh) < l.refreshWindow {
		return
	}
	dbRules, gen, err := l.st.Rules(ctx)
	if err != nil {
		// Fail-open only when no rules have ever loaded; a transient read error
		// after load keeps enforcing last-good rules.
		slog.Warn("quota: store read failed; treating as no limits", "err", err)
		if !l.loaded { // never loaded ⇒ at least apply the seed
			l.rebuild(nil)
			l.loaded = true
		}
		return
	}
	l.lastRefresh = l.clock() // re-queried; reset the throttle regardless of gen
	if l.loaded && gen == l.gen {
		return
	}
	l.rebuild(dbRules)
	l.gen = gen
	l.loaded = true
}

// rebuild sets l.rules = seed overlaid by dbRules. Caller holds l.mu.
func (l *Limiter) rebuild(dbRules []Rule) {
	m := map[string]Rule{}
	for _, r := range l.seed {
		m[key(r.Tenant, r.Upstream)] = r
	}
	for _, r := range dbRules {
		m[key(r.Tenant, r.Upstream)] = r
	}
	l.rules = m
}

// resolveRule walks the (tenant,upstream) precedence list once, returning the
// matched rate together with its bucket key (a matched rule's key is a shared
// ceiling: a (acme,*) rule caps acme across every upstream). Falling through to
// the env default yields the (*,*) floor keyed to the (*,*) bucket. Caller holds
// l.mu. ok=false ⇒ no applicable rule (unlimited).
func (l *Limiter) resolveRule(tenant, upstream string) (rate int, bucketKey string, ok bool) {
	for _, k := range []string{
		key(tenant, upstream), key(tenant, "*"), key("*", upstream), key("*", "*"),
	} {
		if r, ok := l.rules[k]; ok {
			return r.RatePerMin, k, true
		}
	}
	if l.envDefault > 0 {
		return l.envDefault, key("*", "*"), true
	}
	return 0, "", false
}

// Allow consumes one token for (tenant,upstream). No applicable rule ⇒ allowed.
func (l *Limiter) Allow(ctx context.Context, tenant, upstream string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refresh(ctx)
	rate, bk, ok := l.resolveRule(tenant, upstream)
	if !ok {
		return true, 0
	}
	now := l.clock()
	if l.rates[bk] != rate { // new or changed rate ⇒ fresh full bucket
		l.buckets[bk] = &bucket{capacity: float64(rate), perSec: float64(rate) / 60, tokens: float64(rate), last: now}
		l.rates[bk] = rate
	}
	l.lastUse[bk] = now
	return l.buckets[bk].take(now)
}

// Reap drops buckets untouched for older than ttl (call periodically). Bounds
// the map across tenant/upstream churn.
func (l *Limiter) Reap(ttl time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cut := l.clock().Add(-ttl)
	for k, t := range l.lastUse {
		if t.Before(cut) {
			delete(l.buckets, k)
			delete(l.rates, k)
			delete(l.lastUse, k)
		}
	}
}
