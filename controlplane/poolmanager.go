package controlplane

import (
	"context"
	"errors"
	"strconv"
	"sync"

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
	return pm
}

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

// drainTop marks the highest replica draining (no-op if k==0 or already draining).
func (p *PoolManager) drainTop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := len(p.replicas)
	if k == 0 || p.replicas[k-1].draining {
		return
	}
	p.replicas[k-1].draining = true
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

// topDraining reports whether the highest replica is marked draining (test helper).
func (p *PoolManager) topDraining() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	k := len(p.replicas)
	return k > 0 && p.replicas[k-1].draining
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
