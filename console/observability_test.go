package console

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sausheong/runtime/controlplane"
	"github.com/sausheong/runtime/internal/config"
	"github.com/sausheong/runtime/internal/store"
)

// fakeAgentClient serves canned sessions/events per agent for hermetic tests.
type fakeAgentClient struct {
	sessions  map[string][]sessionRow // keyed by ap.AgentID
	events    map[string][]eventRow   // keyed by sessionID
	errAgents map[string]bool         // ap.AgentID -> ListSessions errors
	metrics   map[string]AgentMetrics // ap.AgentID -> canned metrics
	metricErr map[string]bool         // ap.AgentID -> Metrics errors
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
func (f *fakeAgentClient) Metrics(_ context.Context, ap controlplane.AgentProcess) (AgentMetrics, error) {
	if f.metricErr[ap.AgentID] {
		return AgentMetrics{}, fmt.Errorf("boom")
	}
	return f.metrics[ap.AgentID], nil
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

func TestBuildAgentFeed_OrderingAndTruncation(t *testing.T) {
	reg := obsTestReg(t)
	// /sessions returns newest-first: S1 then S2.
	client := &fakeAgentClient{
		sessions: map[string][]sessionRow{
			"a": {{ID: "s1", Status: "running"}, {ID: "s2", Status: "completed"}},
		},
		events: map[string][]eventRow{
			"s1": {{Seq: 1, Type: "text", Text: "one"}, {Seq: 2, Type: "text", Text: "two"}},
			"s2": {{Seq: 1, Type: "done"}},
		},
	}
	feed := buildAgentFeed(context.Background(), reg, client, "a", 10, 50)
	if len(feed) != 3 {
		t.Fatalf("len=%d want 3: %+v", len(feed), feed)
	}
	// Newest session block first (s1: seq 1,2), then s2: seq 1.
	if feed[0].SessionID != "s1" || feed[0].Seq != 1 ||
		feed[1].SessionID != "s1" || feed[1].Seq != 2 ||
		feed[2].SessionID != "s2" || feed[2].Seq != 1 {
		t.Fatalf("ordering wrong: %+v", feed)
	}

	// Truncation: cap at 2 events keeps the newest session block first.
	feed2 := buildAgentFeed(context.Background(), reg, client, "a", 10, 2)
	if len(feed2) != 2 || feed2[0].SessionID != "s1" || feed2[1].Seq != 2 {
		t.Fatalf("truncation wrong: %+v", feed2)
	}
}

func TestBuildAgentFeed_SnippetForError(t *testing.T) {
	reg := obsTestReg(t)
	client := &fakeAgentClient{
		sessions: map[string][]sessionRow{"a": {{ID: "s1"}}},
		events:   map[string][]eventRow{"s1": {{Seq: 1, Type: "error", Err: "kaboom"}}},
	}
	feed := buildAgentFeed(context.Background(), reg, client, "a", 10, 50)
	if len(feed) != 1 || feed[0].Snippet != "error: kaboom" {
		t.Fatalf("error snippet wrong: %+v", feed)
	}
}

func TestBuildAgentMetrics_MapsAndDegrades(t *testing.T) {
	reg := obsTestReg(t)
	want := AgentMetrics{
		TokensIn: 100, TokensOut: 20,
		Tools:          []ToolCount{{Name: "search", Count: 3}},
		TurnsByOutcome: map[string]int64{"completed": 1},
		TurnCount:      1, TurnSumSeconds: 2.0,
	}
	client := &fakeAgentClient{metrics: map[string]AgentMetrics{"a": want}}
	got := buildAgentMetrics(context.Background(), reg, client, "a")
	if got.TokensIn != 100 || got.TokensOut != 20 || got.TurnsCompleted() != 1 || got.AvgTurnSeconds() != 2.0 {
		t.Fatalf("mapping wrong: %+v", got)
	}

	// Metrics error ⇒ zero snapshot (page still renders).
	errClient := &fakeAgentClient{metricErr: map[string]bool{"a": true}}
	z := buildAgentMetrics(context.Background(), reg, errClient, "a")
	if z.TokensIn != 0 || len(z.Tools) != 0 || z.TurnsByOutcome == nil {
		t.Fatalf("error path should yield zero snapshot with non-nil map: %+v", z)
	}

	// Unknown agent (no replica) ⇒ zero snapshot.
	u := buildAgentMetrics(context.Background(), reg, client, "nope")
	if u.TokensIn != 0 {
		t.Fatalf("unknown agent should be zero: %+v", u)
	}
}

func TestSnippetOf_MultibyteTruncationStaysValidUTF8(t *testing.T) {
	long := strings.Repeat("世", 200) // 200 runes, 600 bytes
	s := snippetOf(eventRow{Type: "text", Text: long})
	if !utf8.ValidString(s) {
		t.Fatalf("snippet is not valid UTF-8: %q", s)
	}
	// 140 runes kept + the ellipsis rune = 141 runes.
	if n := utf8.RuneCountInString(s); n != 141 {
		t.Fatalf("rune count = %d, want 141 (140 + ellipsis)", n)
	}
}

func TestBuildAgentFeed_NoSessionsEmpty(t *testing.T) {
	reg := obsTestReg(t)
	client := &fakeAgentClient{} // no sessions for "a"
	feed := buildAgentFeed(context.Background(), reg, client, "a", 10, 50)
	if len(feed) != 0 {
		t.Fatalf("want empty feed, got %+v", feed)
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
