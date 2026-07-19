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
	st         QuotaStore
	envDefault int
	clock      func() time.Time

	mu      sync.Mutex
	gen     uint64
	loaded  bool
	seed    []Rule          // file-config rules (lowest precedence under DB)
	rules   map[string]Rule // "tenant\x00upstream" -> rule (resolved set)
	buckets map[string]*bucket
	rates   map[string]int // bucketKey -> rate that built the bucket (reset on change)
	lastUse map[string]time.Time
}

func NewLimiter(st QuotaStore, envDefault int, clock func() time.Time) *Limiter {
	if clock == nil {
		clock = time.Now
	}
	return &Limiter{
		st: st, envDefault: envDefault, clock: clock,
		rules: map[string]Rule{}, buckets: map[string]*bucket{},
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
	dbRules, gen, err := l.st.Rules(ctx)
	if err != nil {
		slog.Warn("quota: store read failed; treating as no limits", "err", err)
		if !l.loaded { // never loaded ⇒ at least apply the seed
			l.rebuild(nil)
			l.loaded = true
		}
		return
	}
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

// resolve returns the effective rule for (tenant,upstream): most-specific-wins,
// then the env default as the (*,*) floor. Caller holds l.mu.
func (l *Limiter) resolve(tenant, upstream string) (int, bool) {
	for _, k := range []string{
		key(tenant, upstream), key(tenant, "*"), key("*", upstream), key("*", "*"),
	} {
		if r, ok := l.rules[k]; ok {
			return r.RatePerMin, true
		}
	}
	if l.envDefault > 0 {
		return l.envDefault, true
	}
	return 0, false
}

// Allow consumes one token for (tenant,upstream). No applicable rule ⇒ allowed.
func (l *Limiter) Allow(ctx context.Context, tenant, upstream string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refresh(ctx)
	rate, ok := l.resolve(tenant, upstream)
	if !ok {
		return true, 0
	}
	// Bucket keyed by the RESOLVED call pair (not the rule pair) so different
	// calls sharing a wildcard rule each get independent buckets... EXCEPT that
	// would let a (acme,*) rate be exceeded across many upstreams. Key on the
	// matched rule instead so the wildcard rate is a shared ceiling.
	bk := l.matchedRuleKey(tenant, upstream)
	now := l.clock()
	if l.rates[bk] != rate { // new or changed rate ⇒ fresh full bucket
		l.buckets[bk] = &bucket{capacity: float64(rate), perSec: float64(rate) / 60, tokens: float64(rate), last: now}
		l.rates[bk] = rate
	}
	l.lastUse[bk] = now
	return l.buckets[bk].take(now)
}

// matchedRuleKey returns the key of the rule that resolve() matched, so the
// bucket is shared across all calls that resolve to the same rule (a (acme,*)
// rule is one shared ceiling for acme across every upstream). Caller holds l.mu.
func (l *Limiter) matchedRuleKey(tenant, upstream string) string {
	for _, k := range []string{
		key(tenant, upstream), key(tenant, "*"), key("*", upstream), key("*", "*"),
	} {
		if _, ok := l.rules[k]; ok {
			return k
		}
	}
	return key("*", "*") // env-default bucket
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
