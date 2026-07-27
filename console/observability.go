package console

import (
	"context"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sausheong/runtime/controlplane"
	"github.com/sausheong/runtime/internal/gateway"
	"github.com/sausheong/runtime/internal/xfan"
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
	SubHealthy     int // agents with no reachable replica (TotalAgents - HealthyAgents)
	ActiveSessions int
	Upstreams      []gateway.UpstreamStatus
}

// probeFunc reports whether a replica is healthy. Injected for testability.
// It takes a context so a probe stops when the browser request that triggered
// it goes away: buildFleetObs bounds how many probes run at once, but without
// cancellation each survivor still runs to its client timeout after the caller
// has gone.
type probeFunc func(context.Context, controlplane.AgentProcess) bool

// httpProbe is the production probe: a replica is healthy if GET <base>/healthz
// returns 200 (bearer attached when set). Mirrors the /agents API health check.
func httpProbe(ctx context.Context, ap controlplane.AgentProcess) bool {
	client := controlplane.NewAgentHTTPClient(ap, time.Second)
	req, err := http.NewRequestWithContext(ctx, "GET", ap.DialBase()+"/healthz", nil)
	if err != nil {
		return false
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
		if probe(ctx, ap) {
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

// buildAgentMetrics fetches an agent's lifetime telemetry snapshot (tokens, tool
// calls, turn duration/outcome) from its own /metrics, via replica 0 — the same
// attach target buildAgentObs/buildAgentFeed use. Any error (no /metrics, parse
// failure, no replicas) yields a zero snapshot so the page still renders. For a
// multi-replica agent this is replica-0's process totals, not the fleet sum.
func buildAgentMetrics(ctx context.Context, reg *controlplane.Registry, client agentClient, agentID string) AgentMetrics {
	ap, ok := reg.Replica(agentID, 0)
	if !ok {
		return AgentMetrics{TurnsByOutcome: map[string]int64{}}
	}
	m, err := client.Metrics(ctx, ap)
	if err != nil {
		return AgentMetrics{TurnsByOutcome: map[string]int64{}}
	}
	return m
}

// FeedEntry is one row in an agent's activity feed.
type FeedEntry struct {
	SessionID string
	Seq       int64
	Type      string
	Snippet   string
}

// snippetOf renders an event's human-readable detail: the error (prefixed) when
// present, else the text, trimmed to ~140 chars.
func snippetOf(e eventRow) string {
	s := e.Text
	if e.Err != "" {
		s = "error: " + e.Err
	}
	if utf8.RuneCountInString(s) > 140 {
		s = strings.TrimSpace(string([]rune(s)[:140])) + "…"
	}
	return s
}

// buildAgentFeed assembles a newest-session-first, seq-ascending-within feed for
// one agent, capped at maxEvents across at most maxSessions sessions. Reads the
// agent's own HTTP API (works for local and remote agents). Per-session fetch
// errors are skipped (degrade, don't fail); no replicas / list error / no
// sessions yields an empty slice.
func buildAgentFeed(ctx context.Context, reg *controlplane.Registry, client agentClient, agentID string, maxSessions, maxEvents int) []FeedEntry {
	ap, ok := reg.Replica(agentID, 0)
	if !ok {
		return nil
	}
	// The feed's newest-first ordering relies on ListSessions returning sessions
	// newest-first, which the agent runtime's GET /sessions does (its store query
	// is ORDER BY created_at DESC).
	sessions, err := client.ListSessions(ctx, ap)
	if err != nil {
		return nil
	}
	if len(sessions) > maxSessions {
		sessions = sessions[:maxSessions] // newest-first; keep the newest maxSessions
	}
	// Fetch each session's events concurrently; preserve session order on merge.
	perSession := make([][]eventRow, len(sessions))
	xfan.Each(ctx, len(sessions), xfan.DefaultLimit, func(ctx context.Context, i int) {
		evs, err := client.ListEvents(ctx, ap, sessions[i].ID, maxEvents)
		if err != nil {
			return // skip this session
		}
		perSession[i] = evs
	})

	out := make([]FeedEntry, 0, maxEvents)
	for i, evs := range perSession {
		for _, e := range evs { // events are seq-ascending from the endpoint
			out = append(out, FeedEntry{
				SessionID: sessions[i].ID, Seq: e.Seq, Type: e.Type, Snippet: snippetOf(e),
			})
			if len(out) >= maxEvents {
				return out
			}
		}
	}
	return out
}

// buildFleetObs assembles the fleet snapshot across the given (already
// tenant-filtered) agents. Agents are probed concurrently; each goroutine writes
// its own pre-allocated slice slot and aggregation runs after Wait, so the only
// shared reads are reg + client, which must be safe for concurrent reads (the
// Registry and httpAgentClient both are).
func buildFleetObs(ctx context.Context, reg *controlplane.Registry, client agentClient, probe probeFunc, infos []controlplane.AgentInfo) FleetObs {
	f := FleetObs{Agents: make([]AgentObs, len(infos)), TotalAgents: len(infos)}
	xfan.Each(ctx, len(infos), xfan.DefaultLimit, func(ctx context.Context, i int) {
		f.Agents[i] = buildAgentObs(ctx, reg, client, probe, infos[i])
	})
	for _, a := range f.Agents {
		if a.Healthy > 0 {
			f.HealthyAgents++
		}
		f.ActiveSessions += a.Sessions.Created + a.Sessions.Running
	}
	// Precomputed rather than derived in the template: html/template has no
	// arithmetic, and the overview's status strip needs the count of agents with
	// no reachable replica to state the problem in its own words.
	f.SubHealthy = f.TotalAgents - f.HealthyAgents
	return f
}
