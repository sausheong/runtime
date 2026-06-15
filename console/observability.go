package console

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/sausheong/runtime/controlplane"
	"github.com/sausheong/runtime/internal/gateway"
)

// SessionTally counts a single agent's sessions by status.
type SessionTally struct {
	Created, Running, Completed, Error, Total int
}

// AgentObs is the observability snapshot for one agent.
type AgentObs struct {
	ID, Name, Model, Tenant string
	Replicas, Healthy       int
	Sessions                SessionTally
}

// FleetObs is the fleet-wide observability snapshot.
type FleetObs struct {
	Agents         []AgentObs
	TotalAgents    int
	HealthyAgents  int
	ActiveSessions int
	Upstreams      []gateway.UpstreamStatus
}

// probeFunc reports whether a replica is healthy. Injected for testability.
type probeFunc func(controlplane.AgentProcess) bool

// httpProbe is the production probe: a replica is healthy if GET <base>/healthz
// returns 200 (bearer attached when set). Mirrors the /agents API health check.
func httpProbe(ap controlplane.AgentProcess) bool {
	client := &http.Client{Timeout: 1 * time.Second}
	req, err := http.NewRequest("GET", ap.DialBase()+"/healthz", nil)
	if err != nil {
		return false
	}
	if ap.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+ap.AuthToken)
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == 200
}

// tallyHTTP reads an agent's sessions from its own HTTP API and counts them by
// status. A client error yields a zero tally (the page still renders), matching
// the previous store-error behaviour.
func tallyHTTP(ctx context.Context, client agentClient, ap controlplane.AgentProcess) SessionTally {
	var t SessionTally
	rows, err := client.ListSessions(ctx, ap)
	if err != nil {
		return t
	}
	for _, s := range rows {
		switch s.Status {
		case "created":
			t.Created++
		case "running":
			t.Running++
		case "completed":
			t.Completed++
		case "error":
			t.Error++
		}
		t.Total++
	}
	return t
}

// buildAgentObs assembles the snapshot for one agent: replica health (via probe)
// and session tally (via the agent's own HTTP API).
func buildAgentObs(ctx context.Context, reg *controlplane.Registry, client agentClient, probe probeFunc, info controlplane.AgentInfo) AgentObs {
	o := AgentObs{ID: info.ID, Name: info.Name, Model: info.Model, Tenant: info.Tenant}
	replicas, _ := reg.Replicas(info.ID)
	o.Replicas = len(replicas)
	for _, ap := range replicas {
		if probe(ap) {
			o.Healthy++
		}
	}
	// Tally against replica 0 (the proxy's attach target; all replicas of an
	// agent share that agent's session store). Absent replica 0 → zero tally.
	if ap, ok := reg.Replica(info.ID, 0); ok {
		o.Sessions = tallyHTTP(ctx, client, ap)
	}
	return o
}

// buildFleetObs assembles the fleet snapshot across the given (already
// tenant-filtered) agents. Agents are probed concurrently; each goroutine writes
// its own pre-allocated slice slot and aggregation runs after Wait, so the only
// shared reads are reg + client, which must be safe for concurrent reads (the
// Registry and httpAgentClient both are).
func buildFleetObs(ctx context.Context, reg *controlplane.Registry, client agentClient, probe probeFunc, infos []controlplane.AgentInfo) FleetObs {
	f := FleetObs{Agents: make([]AgentObs, len(infos)), TotalAgents: len(infos)}
	var wg sync.WaitGroup
	for i := range infos {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			f.Agents[i] = buildAgentObs(ctx, reg, client, probe, infos[i])
		}(i)
	}
	wg.Wait()
	for _, a := range f.Agents {
		if a.Healthy > 0 {
			f.HealthyAgents++
		}
		f.ActiveSessions += a.Sessions.Created + a.Sessions.Running
	}
	return f
}
