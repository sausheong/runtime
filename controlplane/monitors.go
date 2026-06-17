package controlplane

import (
	"context"
	"sync"
)

// MonitorSet owns the lifecycle of per-agent HealthMonitors so they can be
// started and stopped at runtime (dynamic agent management), not just at boot.
// Each monitor runs under a child of the parent context, with its own cancel
// func, so Stop/Restart can tear exactly one down. Both the startup loop and the
// dynamic add/remove path go through here, so there is a single code path.
type MonitorSet struct {
	parent  context.Context
	reg     *Registry
	onState func(agentID string, replica int, reachable bool) // optional metrics hook

	mu      sync.Mutex
	cancels map[string]context.CancelFunc // agentID -> cancel its monitor
}

// NewMonitorSet builds a MonitorSet bound to parent (cancelled at shutdown) and
// reg (its SetReachable is called on every reachability transition). onState may
// be nil (no metrics).
func NewMonitorSet(parent context.Context, reg *Registry, onState func(string, int, bool)) *MonitorSet {
	return &MonitorSet{
		parent:  parent,
		reg:     reg,
		onState: onState,
		cancels: map[string]context.CancelFunc{},
	}
}

// Start launches (or replaces) the health monitor for a remote agent dialing
// ap.DialBase(). Replaces any existing monitor for the same id. The monitor
// reports reachability into the registry (and the optional metrics hook) as
// replica 0 — dynamically-managed agents are single-replica attach targets.
func (s *MonitorSet) Start(ap AgentProcess) {
	id := ap.AgentID
	s.mu.Lock()
	defer s.mu.Unlock()
	if cancel, ok := s.cancels[id]; ok {
		cancel() // replace: tear the old one down first
	}
	ctx, cancel := context.WithCancel(s.parent)
	s.cancels[id] = cancel
	hm := &HealthMonitor{
		BaseURL: ap.DialBase(),
		Token:   ap.AuthToken,
		OnChange: func(ok bool) {
			if s.onState != nil {
				s.onState(id, 0, ok)
			}
			s.reg.SetReachable(id, 0, ok)
		},
	}
	go hm.Run(ctx)
}

// Stop cancels the monitor for id (idempotent).
func (s *MonitorSet) Stop(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cancel, ok := s.cancels[id]; ok {
		cancel()
		delete(s.cancels, id)
	}
}

// Restart re-attaches: it tears down and relaunches the monitor and resets the
// agent's reachability to "unknown" so the next probe re-evaluates from scratch
// (used after an operator bounces the agent's container, to re-check now instead
// of waiting out the poll interval). ap carries the current dial base/token.
func (s *MonitorSet) Restart(ap AgentProcess) {
	s.reg.ResetReachable(ap.AgentID)
	s.Start(ap) // Start already replaces any existing monitor
}
