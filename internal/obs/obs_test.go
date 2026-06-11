package obs

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/sausheong/harness/llm"
)

func TestControlMetricsHTTPObserved(t *testing.T) {
	c := NewControlMetrics()
	c.HTTPObserved("/agents/{id}/sessions", "POST", 200, 50*time.Millisecond)
	c.HTTPObserved("/agents/{id}/sessions", "POST", 200, 70*time.Millisecond)
	want := `
		# HELP runtime_http_requests_total Total HTTP requests handled by the control plane.
		# TYPE runtime_http_requests_total counter
		runtime_http_requests_total{method="POST",route="/agents/{id}/sessions",status="200"} 2
	`
	if err := testutil.CollectAndCompare(c.httpRequests, strings.NewReader(want)); err != nil {
		t.Fatal(err)
	}
	if n := testutil.CollectAndCount(c.httpDuration); n != 1 {
		t.Fatalf("duration series = %d, want 1", n)
	}
}

func TestControlMetricsAgentAndGateway(t *testing.T) {
	c := NewControlMetrics()
	c.AgentUp("support", true)
	c.AgentUp("research", false)
	c.AgentRestart("support")
	c.ProxyError("support")
	c.GatewayCall("sandbox", "execute_code", "ok", time.Second)
	c.GatewayCall("sandbox", "execute_code", "error", time.Second)
	c.GatewayUpstreamUp("sandbox", true)
	c.ScrapeSkip("research", "timeout")

	if v := testutil.ToFloat64(c.agentUp.WithLabelValues("support")); v != 1 {
		t.Fatalf("agent_up support = %v, want 1", v)
	}
	if v := testutil.ToFloat64(c.agentUp.WithLabelValues("research")); v != 0 {
		t.Fatalf("agent_up research = %v, want 0", v)
	}
	if v := testutil.ToFloat64(c.gwCalls.WithLabelValues("sandbox", "execute_code", "ok")); v != 1 {
		t.Fatalf("gateway ok calls = %v, want 1", v)
	}
	if v := testutil.ToFloat64(c.scrapeSkips.WithLabelValues("research", "timeout")); v != 1 {
		t.Fatalf("scrape skips = %v, want 1", v)
	}
}

func TestAgentMetricsTurnObserved(t *testing.T) {
	a := NewAgentMetrics("support")
	a.TurnObserved("completed", 2*time.Second, &llm.Usage{InputTokens: 100, OutputTokens: 40})
	a.TurnObserved("error", time.Second, nil) // nil usage must not panic or count tokens
	a.ToolCallObserved("bash")

	if v := testutil.ToFloat64(a.turns.WithLabelValues("support", "completed")); v != 1 {
		t.Fatalf("turns completed = %v, want 1", v)
	}
	if v := testutil.ToFloat64(a.tokens.WithLabelValues("support", "input")); v != 100 {
		t.Fatalf("input tokens = %v, want 100", v)
	}
	if v := testutil.ToFloat64(a.tokens.WithLabelValues("support", "output")); v != 40 {
		t.Fatalf("output tokens = %v, want 40", v)
	}
	if v := testutil.ToFloat64(a.toolCalls.WithLabelValues("support", "bash")); v != 1 {
		t.Fatalf("tool calls = %v, want 1", v)
	}
}

func TestNilReceiversAreSafe(t *testing.T) {
	var c *ControlMetrics
	var a *AgentMetrics
	// None of these may panic.
	c.HTTPObserved("/x", "GET", 200, time.Millisecond)
	c.AgentUp("a", true)
	c.AgentRestart("a")
	c.ProxyError("a")
	c.GatewayCall("s", "t", "ok", time.Millisecond)
	c.GatewayUpstreamUp("s", false)
	c.ScrapeSkip("a", "timeout")
	a.TurnObserved("completed", time.Millisecond, nil)
	a.ToolCallObserved("bash")
}

func TestAgentMetricsHandlerServesExposition(t *testing.T) {
	a := NewAgentMetrics("support")
	a.TurnObserved("completed", time.Second, nil)
	body := scrapeHandler(t, a.Handler())
	if !strings.Contains(body, `agent_turns_total{agent="support",outcome="completed"} 1`) {
		t.Fatalf("exposition missing turn counter:\n%s", body)
	}
}

func scrapeHandler(t *testing.T, h http.Handler) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	return rec.Body.String()
}
