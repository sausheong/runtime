package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sausheong/runtime/controlplane"
	"github.com/sausheong/runtime/internal/config"
	"github.com/sausheong/runtime/internal/store"
)

func obsTestReg(t *testing.T) *controlplane.Registry {
	t.Helper()
	cfg := &config.Config{Agents: []config.AgentConfig{
		{ID: "a", Name: "AlphaAgent", Model: "m", ListenAddr: "127.0.0.1:9001", Tenant: "acme"},
	}}
	return controlplane.NewRegistry(cfg, "./agentd", "dsn")
}

func TestBuildAgentObs_TalliesAndHealth(t *testing.T) {
	reg := obsTestReg(t)
	st := store.NewMemStore()
	ctx := context.Background()
	for _, s := range []string{"created", "running", "completed", "error"} {
		id, _ := st.CreateSession(ctx, "a", 0)
		_ = st.SetSessionStatus(ctx, id, s)
	}
	info := controlplane.AgentInfo{ID: "a", Name: "AlphaAgent", Model: "m", Tenant: "acme"}
	probe := func(ap controlplane.AgentProcess) bool { return true }

	obs := buildAgentObs(ctx, reg, st, probe, info)
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

func TestBuildAgentObs_NilStoreNoPanic(t *testing.T) {
	reg := obsTestReg(t)
	info := controlplane.AgentInfo{ID: "a", Name: "AlphaAgent", Tenant: "acme"}
	obs := buildAgentObs(context.Background(), reg, nil, func(controlplane.AgentProcess) bool { return false }, info)
	if obs.Sessions.Total != 0 || obs.Healthy != 0 {
		t.Fatalf("nil store should give zero tally and unhealthy: %+v", obs)
	}
}

func TestBuildFleetObs_AggregatesActiveAndHealthy(t *testing.T) {
	reg := obsTestReg(t)
	st := store.NewMemStore()
	ctx := context.Background()
	id, _ := st.CreateSession(ctx, "a", 0)
	_ = st.SetSessionStatus(ctx, id, "running")
	infos := []controlplane.AgentInfo{{ID: "a", Name: "AlphaAgent", Tenant: "acme"}}
	fleet := buildFleetObs(ctx, reg, st, func(controlplane.AgentProcess) bool { return true }, infos)
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
