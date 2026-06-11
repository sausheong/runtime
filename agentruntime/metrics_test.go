package agentruntime

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sausheong/harness/llm"
	"github.com/sausheong/harness/session"
	"github.com/sausheong/runtime/internal/obs"
)

func TestMetricsEndpointServed(t *testing.T) {
	m := &Manager{agentID: "support", metrics: obs.NewAgentMetrics("support")}
	m.metrics.TurnObserved("completed", time.Second, &llm.Usage{InputTokens: 10, OutputTokens: 5})
	rec := httptest.NewRecorder()
	m.handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`agent_turns_total{agent="support",outcome="completed"} 1`,
		`agent_tokens_total{agent="support",direction="input"} 10`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in:\n%s", want, body)
		}
	}
}

func TestHandlerEchoesRequestID(t *testing.T) {
	m := &Manager{agentID: "support", metrics: obs.NewAgentMetrics("support")}
	req := httptest.NewRequest("GET", "/healthz", nil)
	req.Header.Set(obs.HeaderRequestID, "req-test123")
	rec := httptest.NewRecorder()
	m.handler().ServeHTTP(rec, req)
	if got := rec.Header().Get(obs.HeaderRequestID); got != "req-test123" {
		t.Fatalf("X-Request-ID = %q, want echoed", got)
	}
}

func TestObserveTurnCountsToolCalls(t *testing.T) {
	met := obs.NewAgentMetrics("support")
	m := &Manager{agentID: "support", metrics: met}
	entries := []session.SessionEntry{
		session.ToolCallEntry("c1", "bash", nil),
		session.ToolCallEntry("c2", "bash", nil),
		session.ToolCallEntry("c3", "web_fetch", nil),
	}
	m.observeTurn("continue", 2*time.Second, &llm.Usage{InputTokens: 7, OutputTokens: 3}, entries)
	rec := httptest.NewRecorder()
	met.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{
		`agent_turns_total{agent="support",outcome="continue"} 1`,
		`agent_tool_calls_total{agent="support",tool="bash"} 2`,
		`agent_tool_calls_total{agent="support",tool="web_fetch"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in:\n%s", want, body)
		}
	}
}

func TestObserveTurnNilMetricsSafe(t *testing.T) {
	m := &Manager{agentID: "support"}                 // metrics nil
	m.observeTurn("completed", time.Second, nil, nil) // must not panic
}
