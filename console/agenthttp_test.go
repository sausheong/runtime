package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sausheong/runtime/controlplane"
)

func TestHTTPAgentClient_ListSessionsAndEvents(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/sessions":
			_, _ = w.Write([]byte(`[{"id":"s1","status":"running","turn_count":2}]`))
		case "/sessions/s1/events":
			if r.URL.Query().Get("limit") != "5" {
				t.Errorf("limit not forwarded: %q", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`[{"seq":1,"type":"text","text":"hi"}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := &httpAgentClient{}
	ap := controlplane.AgentProcess{Addr: srv.Listener.Addr().String(), AuthToken: "tok"}

	sess, err := c.ListSessions(context.Background(), ap)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sess) != 1 || sess[0].ID != "s1" || sess[0].Status != "running" {
		t.Fatalf("sessions wrong: %+v", sess)
	}
	if gotAuth != "Bearer tok" {
		t.Fatalf("bearer not attached: %q", gotAuth)
	}

	evs, err := c.ListEvents(context.Background(), ap, "s1", 5)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(evs) != 1 || evs[0].Type != "text" || evs[0].Text != "hi" {
		t.Fatalf("events wrong: %+v", evs)
	}
}

func TestHTTPAgentClient_Non200IsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := &httpAgentClient{}
	ap := controlplane.AgentProcess{Addr: srv.Listener.Addr().String()}
	if _, err := c.ListSessions(context.Background(), ap); err == nil {
		t.Fatal("expected error on non-200")
	}
}

// liveExposition mirrors the shape an agent's /metrics actually serves (see the
// live nutrition-openai output): tokens by direction, tool calls, turns by
// outcome, and the turn-duration histogram count/sum.
const liveExposition = `# HELP agent_tokens_total LLM tokens consumed, by direction.
# TYPE agent_tokens_total counter
agent_tokens_total{direction="input"} 6975
agent_tokens_total{direction="output"} 1362
# HELP agent_tool_calls_total Tool calls dispatched by the agent loop.
# TYPE agent_tool_calls_total counter
agent_tool_calls_total{tool="recall_product"} 1
agent_tool_calls_total{tool="check_sfa_additive"} 6
agent_tool_calls_total{tool="check_hcs"} 1
# HELP agent_turns_total Agent turns by outcome.
# TYPE agent_turns_total counter
agent_turns_total{outcome="completed"} 2
agent_turns_total{outcome="error"} 1
# HELP agent_turn_duration_seconds Agent turn wall time.
# TYPE agent_turn_duration_seconds histogram
agent_turn_duration_seconds_bucket{le="10"} 1
agent_turn_duration_seconds_bucket{le="+Inf"} 2
agent_turn_duration_seconds_sum 41.0
agent_turn_duration_seconds_count 2
`

func TestParseAgentMetrics(t *testing.T) {
	m, err := parseAgentMetrics(strings.NewReader(liveExposition))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.TokensIn != 6975 || m.TokensOut != 1362 {
		t.Fatalf("tokens wrong: in=%d out=%d", m.TokensIn, m.TokensOut)
	}
	// Tools sorted desc by count: check_sfa_additive(6) first.
	if len(m.Tools) != 3 || m.Tools[0].Name != "check_sfa_additive" || m.Tools[0].Count != 6 {
		t.Fatalf("tools wrong: %+v", m.Tools)
	}
	if m.TurnsCompleted() != 2 || m.TurnsError() != 1 {
		t.Fatalf("turns wrong: %+v", m.TurnsByOutcome)
	}
	if m.TurnCount != 2 || m.TurnSumSeconds != 41.0 {
		t.Fatalf("histogram wrong: count=%d sum=%v", m.TurnCount, m.TurnSumSeconds)
	}
	if got := m.AvgTurnSeconds(); got != 20.5 {
		t.Fatalf("avg turn = %v, want 20.5", got)
	}
}

func TestParseAgentMetrics_NoAgentFamiliesIsZero(t *testing.T) {
	// A valid exposition with only unrelated families ⇒ zero snapshot, no error
	// (so the page renders an empty card rather than failing).
	body := "# TYPE go_goroutines gauge\ngo_goroutines 7\n"
	m, err := parseAgentMetrics(strings.NewReader(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.TokensIn != 0 || len(m.Tools) != 0 || m.TurnCount != 0 {
		t.Fatalf("want zero snapshot, got %+v", m)
	}
}

func TestHTTPAgentClient_MetricsParsesLive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			_, _ = w.Write([]byte(liveExposition))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := &httpAgentClient{}
	ap := controlplane.AgentProcess{Addr: srv.Listener.Addr().String()}
	m, err := c.Metrics(context.Background(), ap)
	if err != nil {
		t.Fatalf("Metrics: %v", err)
	}
	if m.TokensIn != 6975 || m.TurnsCompleted() != 2 {
		t.Fatalf("live metrics parse wrong: %+v", m)
	}
}
