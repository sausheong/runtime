package controlplane

import (
	"context"
	"sync"
	"time"
)

// MonitorSet owns the lifecycle of per-agent HealthMonitors so they can be
// started and stopped at runtime (dynamic agent management), not just at boot.
// Each monitor runs under a child of the parent context, with its own cancel
// func, so Stop/Restart can tear exactly one down. Both the startup loop and the
// dynamic add/remove path go through here, so there is a single code path.
type MonitorSet struct {
	parent   context.Context
	reg      *Registry
	onState  func(agentID string, replica int, reachable bool) // optional metrics hook
	interval time.Duration                                     // test override; zero uses HealthMonitor default

	mu          sync.Mutex
	cancels     map[monitorKey]context.CancelFunc
	generations map[monitorKey]uint64
}

type monitorKey struct {
	agentID string
	replica int
}

// NewMonitorSet builds a MonitorSet bound to parent (cancelled at shutdown) and
// reg (its SetReachable is called on every reachability transition). onState may
// be nil (no metrics).
func NewMonitorSet(parent context.Context, reg *Registry, onState func(string, int, bool)) *MonitorSet {
	return &MonitorSet{
		parent:      parent,
		reg:         reg,
		onState:     onState,
		cancels:     map[monitorKey]context.CancelFunc{},
		generations: map[monitorKey]uint64{},
	}
}

// Start launches (or replaces) the health monitor for a remote agent dialing
// ap.DialBase(). Replaces any existing monitor for the same agent ordinal.
func (s *MonitorSet) Start(ap AgentProcess) {
	id := ap.AgentID
	replica := ap.ReplicaIndex
	key := monitorKey{agentID: id, replica: replica}
	s.mu.Lock()
	defer s.mu.Unlock()
	if cancel, ok := s.cancels[key]; ok {
		cancel() // replace: tear the old one down first
	}
	s.generations[key]++
	generation := s.generations[key]
	ctx, cancel := context.WithCancel(s.parent)
	s.cancels[key] = cancel
	hm := &HealthMonitor{
		BaseURL:   ap.DialBase(),
		Token:     ap.AuthToken,
		Transport: agentOutboundTransport(ap),
		Interval:  s.interval,
		OnChange: func(ok bool) {
			// A cancelled HTTP request can finish after its replacement's first
			// successful probe. Only the currently installed monitor may mutate
			// reachability or metrics. Keep the generation lock through the
			// mutation so Start cannot install a successor between validation
			// and SetReachable.
			s.mu.Lock()
			defer s.mu.Unlock()
			current := s.generations[key] == generation
			_, installed := s.cancels[key]
			if !current || !installed {
				return
			}
			if s.onState != nil {
				s.onState(id, replica, ok)
			}
			s.reg.SetReachable(id, replica, ok)
		},
	}
	go hm.Run(ctx)
}

// Stop cancels every monitor for id (idempotent).
func (s *MonitorSet) Stop(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, cancel := range s.cancels {
		if key.agentID == id {
			cancel()
			delete(s.cancels, key)
			s.generations[key]++
		}
	}
}

// StopReplica cancels one agent ordinal's monitor (idempotent).
func (s *MonitorSet) StopReplica(id string, replica int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := monitorKey{agentID: id, replica: replica}
	if cancel, ok := s.cancels[key]; ok {
		cancel()
		delete(s.cancels, key)
		s.generations[key]++
	}
}

// Restart re-attaches: it tears down and relaunches the monitor and resets the
// agent's reachability to "unknown" so the next probe re-evaluates from scratch
// (used after an operator bounces the agent's container, to re-check now instead
// of waiting out the poll interval). ap carries the current dial base/token.
func (s *MonitorSet) Restart(ap AgentProcess) {
	s.reg.ResetReplicaReachable(ap.AgentID, ap.ReplicaIndex)
	s.Start(ap) // Start already replaces any existing monitor
}
