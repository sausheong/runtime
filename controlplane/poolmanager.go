package controlplane

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/sausheong/runtime/internal/config"
	"github.com/sausheong/runtime/internal/obs"
	"github.com/sausheong/runtime/internal/store"
)

var (
	errGrowAtMax = errors.New("poolmanager: at max replicas")
	errGrowRaced = errors.New("poolmanager: grow raced another grow")
)

// PoolManager owns one autoscaled agent's mutable replica set, the Supervisor
// goroutines behind each replica, and its scale decisions — all serialized by
// one mutex. The Registry delegates Replicas/Replica/NextReplica to it for
// autoscaled agents; static agents never construct one (lock-free slice path).
//
// Invariants (from the executor-id crux, see the A2 design spec):
//   - Suffix-only: only ever append at index k, or remove index k-1.
//   - Drain-before-stop: the top replica is stopped only at 0 active sessions.
type PoolManager struct {
	mu       sync.RWMutex
	agentID  string
	base     AgentProcess
	acfg     config.AutoscaleConfig
	addrOf   func(i int) (string, error)
	replicas []replicaSlot
	rr       uint64

	st      store.Store
	metrics *obs.ControlMetrics

	// clock yields monotonic-ish nanos (default timeNowNanos); injected in tests.
	clock func() int64
	// lastUp/lastDown: last up/down actuation (nanos). Owned by the policy
	// goroutine (single-writer); not mutex-guarded.
	lastUp    int64
	lastDown  int64
	upCD      int64 // scale-up cooldown (nanos)
	downCD    int64 // scale-down cooldown (nanos)
	pollEvery int64 // poll interval (nanos)

	// startReplica spawns replica ap's Supervisor and returns a cancel that stops
	// it. Production impl starts a Supervisor + waits for /healthz; tests inject a
	// fake. Set by newPoolManager; overridable in tests.
	startReplica func(ctx context.Context, ap AgentProcess) (context.CancelFunc, error)

	// readyWait, when set (by Start in Task 8), blocks until addr answers /healthz.
	readyWait func(ctx context.Context, addr string) error
}

type replicaSlot struct {
	ap       AgentProcess
	cancel   context.CancelFunc
	draining bool
}

// newPoolManager builds a PoolManager with zero live replicas. metrics/st may be
// nil in unit tests.
func newPoolManager(agentID string, base AgentProcess, acfg config.AutoscaleConfig,
	addrOf func(i int) (string, error), st store.Store, metrics *obs.ControlMetrics) *PoolManager {
	pm := &PoolManager{
		agentID: agentID, base: base, acfg: acfg, addrOf: addrOf,
		st: st, metrics: metrics,
	}
	pm.startReplica = pm.startReplicaProc
	pm.clock = func() int64 { return timeNowNanos() }
	// Asymmetric anti-flap hysteresis (spec Section 4): scale-down is deliberately
	// slower than scale-up so the pool sheds capacity reluctantly but adds it
	// eagerly. This up/down asymmetry is a separate gate layered ON TOP of the
	// pollEvery pacing — a single global poll interval cannot express it, since it
	// debounces up- and down-actuations by different amounts independent of how
	// often the policy loop ticks.
	pm.upCD = int64(10 * 1e9)   // scale-up cooldown: at most one grow per 10s
	pm.downCD = int64(30 * 1e9) // scale-down cooldown: slower than up (anti-flap)
	pm.pollEvery = int64(5 * 1e9)
	return pm
}

func timeNowNanos() int64 { return time.Now().UnixNano() }

// replicaProcess builds the AgentProcess for index i from the base template,
// mirroring NewRegistry's per-replica construction.
func (p *PoolManager) replicaProcess(i int) (AgentProcess, error) {
	addr, err := p.addrOf(i)
	if err != nil {
		return AgentProcess{}, err
	}
	ap := p.base
	ap.ReplicaIndex = i
	ap.Addr = addr
	ap.BaseURL = "http://" + addr
	ap.DBOSVMID = p.agentID + "#" + strconv.Itoa(i)
	return ap, nil
}

// grow appends a replica at index k=len(replicas), spawning THEN publishing so a
// half-started replica is never routable.
func (p *PoolManager) grow(ctx context.Context) error {
	p.mu.Lock()
	k := len(p.replicas)
	p.mu.Unlock()
	if k >= p.acfg.Max {
		return errGrowAtMax
	}
	ap, err := p.replicaProcess(k)
	if err != nil {
		return err
	}
	cancel, err := p.startReplica(ctx, ap)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.replicas) != k {
		cancel()
		return errGrowRaced
	}
	p.replicas = append(p.replicas, replicaSlot{ap: ap, cancel: cancel})
	return nil
}

// drainTop marks the highest replica draining and reports whether this call newly
// transitioned it to draining. Returns false (no transition) when k==0 or the top
// was already draining; true only when it newly marked the top draining.
func (p *PoolManager) drainTop() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := len(p.replicas)
	if k == 0 || p.replicas[k-1].draining {
		return false
	}
	p.replicas[k-1].draining = true
	return true
}

// undrainTop clears the draining flag on the highest replica (un-drain fast path).
func (p *PoolManager) undrainTop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := len(p.replicas)
	if k == 0 || !p.replicas[k-1].draining {
		return
	}
	p.replicas[k-1].draining = false
}

// reapDrained stops and removes the contiguous draining suffix whose active count
// is 0. Only contiguous-from-top reaping preserves suffix-only. Never below 1.
//
// active is a pre-lock snapshot of per-replica non-terminal session counts; using
// it across iterations is safe only because draining replicas are unroutable
// (NextReplica skips them), so a slot already at 0 cannot gain sessions before it
// is reaped.
func (p *PoolManager) reapDrained(active map[int]int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for len(p.replicas) > 1 {
		k := len(p.replicas)
		top := p.replicas[k-1]
		if !top.draining || active[k-1] > 0 {
			return
		}
		top.cancel()
		p.replicas = p.replicas[:k-1]
		if p.metrics != nil {
			p.metrics.AutoscaleEvent(p.agentID, obs.AutoscaleReap)
		}
	}
}

// Replicas returns a snapshot of the live replica set.
func (p *PoolManager) Replicas() []AgentProcess {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]AgentProcess, len(p.replicas))
	for i := range p.replicas {
		out[i] = p.replicas[i].ap
	}
	return out
}

// Replica returns one replica by index. false if i out of range.
func (p *PoolManager) Replica(i int) (AgentProcess, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if i < 0 || i >= len(p.replicas) {
		return AgentProcess{}, false
	}
	return p.replicas[i].ap, true
}

// NextReplica round-robins over the NON-draining replicas for a new session. If
// every replica is draining it falls back to index 0.
func (p *PoolManager) NextReplica() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := len(p.replicas)
	if k == 0 {
		return 0
	}
	for tries := 0; tries < k; tries++ {
		idx := int(p.rr % uint64(k))
		p.rr++
		if !p.replicas[idx].draining {
			return idx
		}
	}
	return 0
}

// replica0Info returns the AgentProcess for index 0, derived from the base
// template even when no replica is live yet (pre-Start). Used for agent-level
// info (tenant/broker) and a health dial target. ok=false only if addr derive fails.
func (p *PoolManager) replica0Info() (AgentProcess, bool) {
	if ap, ok := p.Replica(0); ok {
		return ap, true
	}
	ap, err := p.replicaProcess(0)
	if err != nil {
		return AgentProcess{}, false
	}
	return ap, true
}

// SetDeps injects the store, metrics, and readiness probe before Start. Must be
// called before runPolicy/grow start spawning real replicas.
func (p *PoolManager) SetDeps(st store.Store, m *obs.ControlMetrics, readyWait func(ctx context.Context, addr string) error) {
	p.st = st
	p.metrics = m
	p.readyWait = readyWait
}

// topDraining reports whether the highest replica is marked draining (test helper).
func (p *PoolManager) topDraining() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	k := len(p.replicas)
	return k > 0 && p.replicas[k-1].draining
}

type scaleStep int

const (
	stepNone scaleStep = iota
	stepGrow
	stepDrain
	stepUndrain
	stepBlocked // wanted to act but a cooldown/clamp stopped it (counted, no-op)
)

// decideStep computes the single step to take this tick. Pure: no I/O, no locks.
//
// raw = ceil(active/target) is the unclamped replica count the load wants; k is
// the current replica count. The direction decision is driven by desired vs k,
// where desired is raw CLAMPED to [Min,Max] — so the Min floor is actively
// restored (an idle pool below Min still grows up to Min) and the Max ceiling is
// held. The one place raw (not the clamp) speaks directly is the ceiling-pressure
// signal: when load wants more than Max allows (raw>k && k>=Max) we report
// stepBlocked, a thwarted up-action worth observing.
//
//   - capped up-pressure (raw>k && k>=Max): stepBlocked.
//   - up   (desired > k): undrain the top if it is draining (rebound fast path)
//     or grow, gated by upReady (cooldown).
//   - down (desired < k): drain the top, gated by downReady (cooldown).
//   - otherwise stepNone.
//
// stepBlocked means "wanted to act but a cooldown or the Max clamp prevented it"
// — it is counted but performs no mutation.
//
// Asymmetry by design: at Max with excess load we report stepBlocked (capped
// up-pressure), but a pool resting at Min with no load is stepNone, not blocked —
// a floor with no downward pressure is the normal resting state, not an action
// we'd report every tick. (At Min, the clamped desired==Min==k ⇒ stepNone; at Max
// with excess load, raw>k && k>=Max ⇒ stepBlocked.)
func decideStep(acfg config.AutoscaleConfig, active, k int, topDraining, upReady, downReady bool) scaleStep {
	target := acfg.TargetSessionsPerReplica
	raw := (active + target - 1) / target // ceil
	// Load wants more than Max allows: report thwarted up-pressure (capped).
	if raw > k && k >= acfg.Max {
		return stepBlocked
	}
	// Clamp to [Min,Max] for the direction decision so the Min floor is actively
	// restored (an idle pool below Min still grows up to Min) and Max is held.
	desired := raw
	if desired < acfg.Min {
		desired = acfg.Min
	}
	if desired > acfg.Max {
		desired = acfg.Max
	}
	switch {
	case desired > k:
		if !upReady {
			return stepBlocked
		}
		if topDraining {
			return stepUndrain
		}
		return stepGrow
	case desired < k:
		if !downReady {
			return stepBlocked
		}
		return stepDrain
	default:
		return stepNone
	}
}

// tick runs one policy iteration: read load, decide, actuate at most one step,
// always reap drained-to-zero top replicas, update gauges. Never scales on a
// failed load read (holds current size).
func (p *PoolManager) tick(ctx context.Context) {
	active, err := p.st.ActiveSessionsByReplica(ctx, p.agentID)
	if err != nil {
		return
	}
	total := 0
	for _, n := range active {
		total += n
	}
	p.mu.RLock()
	k := len(p.replicas)
	topDrain := k > 0 && p.replicas[k-1].draining
	p.mu.RUnlock()

	now := p.clock()
	upReady := now-p.lastUp >= p.upCD
	downReady := now-p.lastDown >= p.downCD

	newlyDrained := false
	switch decideStep(p.acfg, total, k, topDrain, upReady, downReady) {
	case stepGrow:
		if err := p.grow(ctx); err != nil {
			p.lastUp = now
			p.metrics.AutoscaleEvent(p.agentID, obs.AutoscaleBlocked)
		} else {
			p.lastUp = now
			p.metrics.AutoscaleEvent(p.agentID, obs.AutoscaleUp)
		}
	case stepUndrain:
		p.undrainTop()
		p.lastUp = now
		p.metrics.AutoscaleEvent(p.agentID, obs.AutoscaleUndrain)
	case stepDrain:
		newlyDrained = p.drainTop()
		p.lastDown = now
		p.metrics.AutoscaleEvent(p.agentID, obs.AutoscaleDown)
	case stepBlocked:
		p.metrics.AutoscaleEvent(p.agentID, obs.AutoscaleBlocked)
	}

	// Never reap a replica in the same tick it was newly drained: the load
	// snapshot above predates the drain mark, so a session routed to the top
	// between snapshot and drain would be invisible here and could be reaped out
	// from under a live durable workflow. Deferring the reap to a later tick
	// re-reads the snapshot AFTER the drain (and after NextReplica has been
	// skipping the draining replica under lock), closing that window. (A residual
	// micro-window remains only if a session is routed to the top, then its row
	// commit lags more than a full poll interval past the next snapshot — far
	// outside normal operation.)
	if !newlyDrained {
		p.reapDrained(active)
	}

	p.mu.RLock()
	cur := len(p.replicas)
	p.mu.RUnlock()
	target := p.acfg.TargetSessionsPerReplica
	desired := (total + target - 1) / target
	if desired < p.acfg.Min {
		desired = p.acfg.Min
	}
	if desired > p.acfg.Max {
		desired = p.acfg.Max
	}
	p.metrics.AutoscaleActive(p.agentID, total)
	p.metrics.AutoscaleDesired(p.agentID, desired)
	p.metrics.AutoscaleCurrent(p.agentID, cur)
}

// Start grows the pool toward min live replicas (sequential, readiness-gated so
// the first replica creates the DBOS schema before the rest), then ALWAYS
// launches the policy loop — which self-heals toward min if an initial grow
// failed. Returns the first-replica error (for the caller to log) but the pool
// is not abandoned: the policy loop keeps retrying. Deps must be set via SetDeps
// first.
func (p *PoolManager) Start(ctx context.Context) error {
	var firstErr error
	for i := 0; i < p.acfg.Min; i++ {
		if err := p.grow(ctx); err != nil {
			if i == 0 {
				firstErr = err
			}
			break
		}
	}
	go p.runPolicy(ctx)
	return firstErr
}

// ApplyTuning overrides poll interval and cooldowns from operator/test env (given
// in seconds; <=0 keeps the default). Call before Start.
func (p *PoolManager) ApplyTuning(pollSec, upCDSec, downCDSec float64) {
	if pollSec > 0 {
		p.pollEvery = int64(pollSec * 1e9)
	}
	if upCDSec > 0 {
		p.upCD = int64(upCDSec * 1e9)
	}
	if downCDSec > 0 {
		p.downCD = int64(downCDSec * 1e9)
	}
}

// runPolicy ticks every pollEvery until ctx is cancelled.
func (p *PoolManager) runPolicy(ctx context.Context) {
	t := time.NewTicker(time.Duration(p.pollEvery))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.tick(ctx)
		}
	}
}

// startReplicaProc is the production startReplica: it launches a Supervisor for ap
// under a child context and waits until ap answers /healthz (via readyWait).
func (p *PoolManager) startReplicaProc(ctx context.Context, ap AgentProcess) (context.CancelFunc, error) {
	rctx, cancel := context.WithCancel(ctx)
	idx := ap.ReplicaIndex
	sup := &Supervisor{
		Spawn:     ap.SpawnFunc(),
		OnRestart: func() { p.metrics.AgentRestart(p.agentID, idx) },
	}
	go sup.Run(rctx)
	if p.readyWait != nil {
		if err := p.readyWait(rctx, ap.Addr); err != nil {
			cancel()
			return nil, err
		}
	}
	return cancel, nil
}
