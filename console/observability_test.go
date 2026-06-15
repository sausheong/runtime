package console

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sausheong/runtime/controlplane"
	"github.com/sausheong/runtime/internal/config"
	"github.com/sausheong/runtime/internal/store"
)

// fakeAgentClient serves canned sessions/events per agent for hermetic tests.
type fakeAgentClient struct {
	sessions  map[string][]sessionRow // keyed by ap.AgentID
	events    map[string][]eventRow   // keyed by sessionID
	errAgents map[string]bool         // ap.AgentID -> ListSessions errors
}

func (f *fakeAgentClient) ListSessions(_ context.Context, ap controlplane.AgentProcess) ([]sessionRow, error) {
	if f.errAgents[ap.AgentID] {
		return nil, fmt.Errorf("boom")
	}
	return f.sessions[ap.AgentID], nil
}
func (f *fakeAgentClient) ListEvents(_ context.Context, _ controlplane.AgentProcess, sid string, limit int) ([]eventRow, error) {
	evs := f.events[sid]
	if len(evs) > limit {
		evs = evs[len(evs)-limit:]
	}
	return evs, nil
}

func obsTestReg(t *testing.T) *controlplane.Registry {
	t.Helper()
	cfg := &config.Config{Agents: []config.AgentConfig{
		{ID: "a", Name: "AlphaAgent", Model: "m", ListenAddr: "127.0.0.1:9001", Tenant: "acme"},
	}}
	return controlplane.NewRegistry(cfg, "./agentd", "dsn")
}

func TestBuildAgentObs_TalliesAndHealth(t *testing.T) {
	reg := obsTestReg(t)
	ctx := context.Background()
	client := &fakeAgentClient{sessions: map[string][]sessionRow{
		"a": {{ID: "s1", Status: "created"}, {ID: "s2", Status: "running"},
			{ID: "s3", Status: "completed"}, {ID: "s4", Status: "error"}},
	}}
	info := controlplane.AgentInfo{ID: "a", Name: "AlphaAgent", Model: "m", Tenant: "acme"}
	probe := func(ap controlplane.AgentProcess) bool { return true }

	obs := buildAgentObs(ctx, reg, client, probe, info)
	if obs.Sessions.Created != 1 || obs.Sessions.Running != 1 || obs.Sessions.Completed != 1 || obs.Sessions.Error != 1 {
		t.Fatalf("tally wrong: %+v", obs.Sessions)
	}
	if obs.Sessions.Total != 4 {
		t.Fatalf("total = %d, want 4", obs.Sessions.Total)
	}
	if obs.Replicas != 1 || obs.Healthy != 1 {
		t.Fatalf("health: replicas=%d healthy=%d, want 1/1", obs.Replicas, obs.Healthy)
	}
}

func TestBuildAgentObs_ClientErrorZeroTally(t *testing.T) {
	reg := obsTestReg(t)
	client := &fakeAgentClient{errAgents: map[string]bool{"a": true}}
	info := controlplane.AgentInfo{ID: "a", Name: "AlphaAgent", Tenant: "acme"}
	obs := buildAgentObs(context.Background(), reg, client, func(controlplane.AgentProcess) bool { return false }, info)
	if obs.Sessions.Total != 0 || obs.Healthy != 0 {
		t.Fatalf("client error should give zero tally and unhealthy: %+v", obs)
	}
}

func TestBuildFleetObs_AggregatesActiveAndHealthy(t *testing.T) {
	reg := obsTestReg(t)
	ctx := context.Background()
	client := &fakeAgentClient{sessions: map[string][]sessionRow{
		"a": {{ID: "s1", Status: "running"}},
	}}
	infos := []controlplane.AgentInfo{{ID: "a", Name: "AlphaAgent", Tenant: "acme"}}
	fleet := buildFleetObs(ctx, reg, client, func(controlplane.AgentProcess) bool { return true }, infos)
	if fleet.TotalAgents != 1 || fleet.HealthyAgents != 1 {
		t.Fatalf("fleet agents: total=%d healthy=%d", fleet.TotalAgents, fleet.HealthyAgents)
	}
	if fleet.ActiveSessions != 1 {
		t.Fatalf("active sessions = %d, want 1", fleet.ActiveSessions)
	}
	if len(fleet.Agents) != 1 || fleet.Agents[0].ID != "a" {
		t.Fatalf("fleet.Agents wrong: %+v", fleet.Agents)
	}
}

func TestObservabilityPageRenders(t *testing.T) {
	st := store.NewMemStore()
	id, _ := st.CreateSession(context.Background(), "a", 0)
	_ = st.SetSessionStatus(context.Background(), id, "running")
	h := Handler(obsTestReg(t), st, OIDCConfig{}, nil) // open mode → all agents visible
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/ui/observability", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /ui/observability: code=%d want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Fleet summary") {
		t.Fatal("missing Fleet summary section")
	}
	if !strings.Contains(body, "AlphaAgent") {
		t.Fatal("missing agent row")
	}
}

func TestObservabilityNavLinkPresent(t *testing.T) {
	h := Handler(obsTestReg(t), store.NewMemStore(), OIDCConfig{}, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/ui", nil))
	if !strings.Contains(rec.Body.String(), `href="/ui/observability"`) {
		t.Fatal("overview topbar missing Observability nav link")
	}
}

func TestAgentPageHasMetricsPanel(t *testing.T) {
	st := store.NewMemStore()
	id, _ := st.CreateSession(context.Background(), "a", 0)
	_ = st.SetSessionStatus(context.Background(), id, "running")
	h := Handler(obsTestReg(t), st, OIDCConfig{}, nil) // open mode
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/ui/agents/a", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /ui/agents/a: code=%d want 200", rec.Code)
	}
	body := rec.Body.String()
	// Anchor on the tile markup (not the bare word "Health") so this asserts the
	// metrics panel specifically, and on the Health value tile reflecting the
	// single replica (0/1, since the loopback probe fails in tests).
	if !strings.Contains(body, `<div class="stat-label">Health</div>`) {
		t.Fatal("agent page missing metrics panel Health tile")
	}
	if !strings.Contains(body, `<div class="stat-num">0/1</div>`) {
		t.Fatal("metrics panel missing Health value tile (want 0/1)")
	}
}
