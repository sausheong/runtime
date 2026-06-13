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
	c.AgentUp("support", 0, true)
	c.AgentUp("research", 0, false)
	c.AgentRestart("support", 0)
	c.ProxyError("support")
	c.GatewayCall("sandbox", "execute_code", "ok", time.Second)
	c.GatewayCall("sandbox", "execute_code", "error", time.Second)
	c.GatewayUpstreamUp("sandbox", true)
	c.ScrapeSkip("research", 0, "timeout")

	if v := testutil.ToFloat64(c.agentUp.WithLabelValues("support", "0")); v != 1 {
		t.Fatalf("agent_up support = %v, want 1", v)
	}
	if v := testutil.ToFloat64(c.agentUp.WithLabelValues("research", "0")); v != 0 {
		t.Fatalf("agent_up research = %v, want 0", v)
	}
	if v := testutil.ToFloat64(c.gwCalls.WithLabelValues("sandbox", "execute_code", "ok")); v != 1 {
		t.Fatalf("gateway ok calls = %v, want 1", v)
	}
	if v := testutil.ToFloat64(c.scrapeSkips.WithLabelValues("research", "0", "timeout")); v != 1 {
		t.Fatalf("scrape skips = %v, want 1", v)
	}
}

func TestControlMetrics_ReplicaLabels(t *testing.T) {
	c := NewControlMetrics()
	// New signatures take a replica index; must not panic and must register.
	c.AgentUp("a", 0, true)
	c.AgentReachable("rem", 0, true)
	c.AgentRestart("a", 1)
	c.ScrapeSkip("a", 2, "timeout")
	// Nil-safe (no panic on nil receiver).
	var n *ControlMetrics
	n.AgentUp("a", 0, false)
	n.AgentReachable("a", 0, false)
	n.AgentRestart("a", 0)
	n.ScrapeSkip("a", 0, "x")
}

func TestAgentMetricsTurnObserved(t *testing.T) {
	a := NewAgentMetrics("support")
	a.TurnObserved("completed", 2*time.Second, &llm.Usage{
		InputTokens: 100, OutputTokens: 40,
		CacheCreationInputTokens: 7, CacheReadInputTokens: 9,
	})
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
	if v := testutil.ToFloat64(a.tokens.WithLabelValues("support", "cache_creation")); v != 7 {
		t.Fatalf("cache_creation tokens = %v, want 7", v)
	}
	if v := testutil.ToFloat64(a.tokens.WithLabelValues("support", "cache_read")); v != 9 {
		t.Fatalf("cache_read tokens = %v, want 9", v)
	}
}

func TestAgentMetricsNoCacheSeriesWithoutCacheTokens(t *testing.T) {
	// Usage without cache fields must NOT create cache_creation/cache_read
	// series — only input and output.
	a := NewAgentMetrics("support")
	a.TurnObserved("completed", time.Second, &llm.Usage{InputTokens: 100, OutputTokens: 40})
	if n := testutil.CollectAndCount(a.tokens); n != 2 {
		t.Fatalf("token series = %d, want 2 (input/output only)", n)
	}
}

func TestAuthRejectedCountsWithoutDurationSample(t *testing.T) {
	c := NewControlMetrics()
	c.AuthRejected(401)
	c.AuthRejected(401)
	c.AuthRejected(403)
	want := `
		# HELP runtime_http_requests_total Total HTTP requests handled by the control plane.
		# TYPE runtime_http_requests_total counter
		runtime_http_requests_total{method="",route="auth_rejected",status="401"} 2
		runtime_http_requests_total{method="",route="auth_rejected",status="403"} 1
	`
	if err := testutil.CollectAndCompare(c.httpRequests, strings.NewReader(want)); err != nil {
		t.Fatal(err)
	}
	// Rejections must NOT pollute the latency histogram with zero-second
	// samples: no duration series may exist.
	if n := testutil.CollectAndCount(c.httpDuration); n != 0 {
		t.Fatalf("duration series after AuthRejected = %d, want 0", n)
	}
}

func TestTurnDurationUsesTurnBucketsNotDefBuckets(t *testing.T) {
	// Regression guard: agent_turn_duration_seconds must use the custom
	// turnBuckets (0.1..120), not prometheus.DefBuckets (0.005..10) — agent
	// turns routinely exceed 10s.
	a := NewAgentMetrics("support")
	a.TurnObserved("completed", 45*time.Second, nil)
	body := scrapeHandler(t, a.Handler())
	for _, want := range []string{`le="60"`, `le="120"`} {
		if !strings.Contains(body, `agent_turn_duration_seconds_bucket{agent="support",`+want) {
			t.Fatalf("exposition missing turnBuckets boundary %s:\n%s", want, body)
		}
	}
	if strings.Contains(body, `agent_turn_duration_seconds_bucket{agent="support",le="0.005"`) {
		t.Fatalf("turn duration histogram uses DefBuckets (le=0.005 present):\n%s", body)
	}
}

func TestGatewayDurationUsesTurnBucketsNotDefBuckets(t *testing.T) {
	// Same guard for runtime_gateway_tool_call_duration_seconds, scraped
	// through the fan-out handler (no agent targets needed).
	c := NewControlMetrics()
	c.GatewayCall("sandbox", "execute_code", OutcomeOK, 45*time.Second)
	body := scrapeHandler(t, FanoutHandler(c, func() []ScrapeTarget { return nil }))
	for _, want := range []string{`le="60"`, `le="120"`} {
		if !strings.Contains(body, `runtime_gateway_tool_call_duration_seconds_bucket{server="sandbox",`+want) {
			t.Fatalf("exposition missing turnBuckets boundary %s:\n%s", want, body)
		}
	}
	if strings.Contains(body, `runtime_gateway_tool_call_duration_seconds_bucket{server="sandbox",le="0.005"`) {
		t.Fatalf("gateway duration histogram uses DefBuckets (le=0.005 present):\n%s", body)
	}
}

func TestNilReceiversAreSafe(t *testing.T) {
	var c *ControlMetrics
	var a *AgentMetrics
	// None of these may panic.
	c.HTTPObserved("/x", "GET", 200, time.Millisecond)
	c.AuthRejected(401)
	c.AgentUp("a", 0, true)
	c.AgentRestart("a", 0)
	c.ProxyError("a")
	c.GatewayCall("s", "t", "ok", time.Millisecond)
	c.GatewayUpstreamUp("s", false)
	c.ScrapeSkip("a", 0, "timeout")
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

func TestNilAgentMetricsHandlerIs404(t *testing.T) {
	var a *AgentMetrics
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("nil handler status = %d, want 404", rec.Code)
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
