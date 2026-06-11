# Observability M1 — Prometheus Metrics + Request Correlation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Prometheus `/metrics` on runtimed (control-plane + fan-out-merged agent metrics) and agentd (turns/tokens/tools), `X-Request-ID` correlation across the process boundary, and a compose overlay shipping Prometheus + Grafana with a provisioned dashboard.

**Architecture:** New `internal/obs` package is the SOLE importer of `prometheus/client_golang`: two registry-owning structs (`ControlMetrics` for runtimed, `AgentMetrics` for agentd) with nil-safe helper methods, a `RequestID` middleware, and a fan-out handler that scrapes each supervised agent's `/metrics` (500ms cap, concurrent), parses with `expfmt`, merges metric families by name, and re-encodes one valid exposition. Spec: `docs/superpowers/specs/2026-06-11-observability-m1-metrics-design.md`.

**Tech Stack:** Go 1.25, `github.com/prometheus/client_golang` (prometheus, promhttp, testutil), `github.com/prometheus/common/expfmt`, Prometheus + Grafana via docker compose overlay.

---

## Context for every task

- Branch: `observability-m1` off `master`. All commits there.
- Run tests: `go test ./internal/obs/ ./controlplane/ ./agentruntime/ ./internal/gateway/ -count=1` (package-scoped per task; full `go test ./...` before the final task).
- `internal/obs` is the ONLY package allowed to import `prometheus/*`. Everything else calls obs helpers. All `ControlMetrics`/`AgentMetrics` methods must be **nil-receiver-safe** (a nil metrics struct = all helpers no-op) so tests and callers never need conditionals.
- Cardinality hard rules (spec §3.3): labels are ONLY agent ids, upstream/server names, tool names, route patterns, methods, statuses, outcomes, directions, reasons. NO session/tenant/path/argument labels.
- Existing code facts you need:
  - `cmd/runtimed/main.go` has `accessLog(next http.Handler)` (slog line per request, `statusRecorder` wrapper) and `buildRoot(...)`; handler chain is `accessLog(root)` in open mode or `controlplane.IdentityMiddleware(accessLog(root), authr, azr)` with identity.
  - `controlplane/api.go` `NewAPI(reg *Registry) *http.ServeMux` routes `/agents/{id}/...` via `reverseProxy(ap.Addr).ServeHTTP(w, r)` (proxy built per request in `controlplane/proxy.go:121`).
  - `controlplane/supervisor.go` `Supervisor{Spawn, Backoff, MaxBackoff}` restarts in `Run(ctx)`.
  - `agentruntime/server.go` `(m *Manager) newMux() *http.ServeMux` has `/healthz /meta /sessions ...`; `agentruntime/serve.go` `Serve()` builds `Manager` and serves `m.newMux()`; the turn runs inside `dbos.RunAsStep` closure in `sessionWorkflow` (`rt.RunTurn(stepCtx, userMsg, images, nil)` at `serve.go:159`), returning `harness/runtime.TurnResult{Done, StopReason, Entries, Err, Usage}`.
  - `internal/gateway/server.go` `toolHandler(builtFor string, t tool.Tool, forwardTenant bool)` wraps `t.Execute`; `internal/gateway/manager.go` marks upstreams up (connect success, `manager.go:125-132`) and down (`markDown`).
  - `harness/llm.Usage{InputTokens, OutputTokens, ...}` ints; `harness/session.EntryTypeToolCall` entries carry `Tool` (tool name).
  - Go 1.22+ `http.ServeMux` sets `r.Pattern` (e.g. `"GET /agents/{id}/sessions"`) on matched requests — that is the normalized route.

---

### Task 1: `internal/obs` — metric registries and helpers

**Files:**
- Create: `internal/obs/obs.go`
- Create: `internal/obs/obs_test.go`

- [ ] **Step 1: Add dependencies**

```bash
go get github.com/prometheus/client_golang@latest github.com/prometheus/common@latest
```

- [ ] **Step 2: Write the failing tests**

`internal/obs/obs_test.go`:

```go
package obs

import (
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
```

Add the shared test helper at the bottom of `obs_test.go`:

```go
func scrapeHandler(t *testing.T, h http.Handler) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	return rec.Body.String()
}
```

(add `net/http` and `net/http/httptest` to imports.)

- [ ] **Step 3: Run tests, verify they fail to compile**

Run: `go test ./internal/obs/ -count=1`
Expected: FAIL — package does not exist / undefined types.

- [ ] **Step 4: Implement `internal/obs/obs.go`**

```go
// Package obs owns every Prometheus metric the platform emits and the
// request-correlation middleware. It is the ONLY package in the module that
// imports prometheus/client_golang; everything else calls the typed helpers
// here. All methods are nil-receiver-safe: a nil *ControlMetrics or
// *AgentMetrics turns every helper into a no-op.
package obs

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sausheong/harness/llm"
)

// turnBuckets cover LLM-turn and gateway-call durations: seconds to minutes.
// Prometheus default buckets top out at 10s, far too short for agent turns.
var turnBuckets = []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120}

// ControlMetrics is runtimed's registry: HTTP edge, agent supervision,
// reverse proxy, gateway federation, and fan-out scrape bookkeeping.
type ControlMetrics struct {
	reg           *prometheus.Registry
	httpRequests  *prometheus.CounterVec
	httpDuration  *prometheus.HistogramVec
	agentUp       *prometheus.GaugeVec
	agentRestarts *prometheus.CounterVec
	proxyErrors   *prometheus.CounterVec
	gwCalls       *prometheus.CounterVec
	gwDuration    *prometheus.HistogramVec
	gwUp          *prometheus.GaugeVec
	scrapeSkips   *prometheus.CounterVec
}

func NewControlMetrics() *ControlMetrics {
	c := &ControlMetrics{reg: prometheus.NewRegistry()}
	c.httpRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "runtime_http_requests_total",
		Help: "Total HTTP requests handled by the control plane.",
	}, []string{"route", "method", "status"})
	c.httpDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "runtime_http_request_duration_seconds",
		Help:    "Control-plane HTTP request duration.",
		Buckets: prometheus.DefBuckets,
	}, []string{"route", "method"})
	c.agentUp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "runtime_agent_up",
		Help: "1 when the agent's /metrics was scraped cleanly on the last fan-out (404 counts as serving).",
	}, []string{"agent"})
	c.agentRestarts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "runtime_agent_restarts_total",
		Help: "Supervisor respawns per agent.",
	}, []string{"agent"})
	c.proxyErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "runtime_proxy_errors_total",
		Help: "Reverse-proxy failures (503s served) per agent.",
	}, []string{"agent"})
	c.gwCalls = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "runtime_gateway_tool_calls_total",
		Help: "Federated gateway tool calls by upstream, tool, and outcome.",
	}, []string{"server", "tool", "outcome"})
	c.gwDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "runtime_gateway_tool_call_duration_seconds",
		Help:    "Gateway tool call duration by upstream.",
		Buckets: turnBuckets,
	}, []string{"server"})
	c.gwUp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "runtime_gateway_upstream_up",
		Help: "1 when the gateway upstream connection is up.",
	}, []string{"server"})
	c.scrapeSkips = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "runtime_metrics_scrape_skips_total",
		Help: "Agents skipped during fan-out scrape, by reason.",
	}, []string{"agent", "reason"})
	c.reg.MustRegister(c.httpRequests, c.httpDuration, c.agentUp, c.agentRestarts,
		c.proxyErrors, c.gwCalls, c.gwDuration, c.gwUp, c.scrapeSkips)
	return c
}

func (c *ControlMetrics) HTTPObserved(route, method string, status int, dur time.Duration) {
	if c == nil {
		return
	}
	c.httpRequests.WithLabelValues(route, method, strconv.Itoa(status)).Inc()
	c.httpDuration.WithLabelValues(route, method).Observe(dur.Seconds())
}

func (c *ControlMetrics) AgentUp(agent string, up bool) {
	if c == nil {
		return
	}
	v := 0.0
	if up {
		v = 1
	}
	c.agentUp.WithLabelValues(agent).Set(v)
}

func (c *ControlMetrics) AgentRestart(agent string) {
	if c == nil {
		return
	}
	c.agentRestarts.WithLabelValues(agent).Inc()
}

func (c *ControlMetrics) ProxyError(agent string) {
	if c == nil {
		return
	}
	c.proxyErrors.WithLabelValues(agent).Inc()
}

func (c *ControlMetrics) GatewayCall(server, tool, outcome string, dur time.Duration) {
	if c == nil {
		return
	}
	c.gwCalls.WithLabelValues(server, tool, outcome).Inc()
	c.gwDuration.WithLabelValues(server).Observe(dur.Seconds())
}

func (c *ControlMetrics) GatewayUpstreamUp(server string, up bool) {
	if c == nil {
		return
	}
	v := 0.0
	if up {
		v = 1
	}
	c.gwUp.WithLabelValues(server).Set(v)
}

func (c *ControlMetrics) ScrapeSkip(agent, reason string) {
	if c == nil {
		return
	}
	c.scrapeSkips.WithLabelValues(agent, reason).Inc()
}

// AgentMetrics is agentd's registry. Every series carries agent=<id> so the
// fan-out merge produces disjoint series across agents.
type AgentMetrics struct {
	agentID   string
	reg       *prometheus.Registry
	turns     *prometheus.CounterVec
	turnDur   *prometheus.HistogramVec
	tokens    *prometheus.CounterVec
	toolCalls *prometheus.CounterVec
}

func NewAgentMetrics(agentID string) *AgentMetrics {
	a := &AgentMetrics{agentID: agentID, reg: prometheus.NewRegistry()}
	a.turns = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "agent_turns_total",
		Help: "Agent turns by outcome (completed/error/aborted/continue).",
	}, []string{"agent", "outcome"})
	a.turnDur = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "agent_turn_duration_seconds",
		Help:    "Agent turn wall time.",
		Buckets: turnBuckets,
	}, []string{"agent"})
	a.tokens = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "agent_tokens_total",
		Help: "LLM tokens consumed, by direction (input/output).",
	}, []string{"agent", "direction"})
	a.toolCalls = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "agent_tool_calls_total",
		Help: "Tool calls dispatched by the agent loop.",
	}, []string{"agent", "tool"})
	a.reg.MustRegister(a.turns, a.turnDur, a.tokens, a.toolCalls)
	return a
}

func (a *AgentMetrics) TurnObserved(outcome string, dur time.Duration, usage *llm.Usage) {
	if a == nil {
		return
	}
	a.turns.WithLabelValues(a.agentID, outcome).Inc()
	a.turnDur.WithLabelValues(a.agentID).Observe(dur.Seconds())
	if usage != nil {
		a.tokens.WithLabelValues(a.agentID, "input").Add(float64(usage.InputTokens))
		a.tokens.WithLabelValues(a.agentID, "output").Add(float64(usage.OutputTokens))
	}
}

func (a *AgentMetrics) ToolCallObserved(tool string) {
	if a == nil {
		return
	}
	a.toolCalls.WithLabelValues(a.agentID, tool).Inc()
}

// Handler serves this registry's exposition (agentd mounts it at /metrics).
func (a *AgentMetrics) Handler() http.Handler {
	if a == nil {
		return http.NotFoundHandler()
	}
	return promhttp.HandlerFor(a.reg, promhttp.HandlerOpts{})
}
```

- [ ] **Step 5: Run tests, verify pass**

Run: `go test ./internal/obs/ -count=1 -v`
Expected: all PASS.

- [ ] **Step 6: Commit**

```bash
go mod tidy
git add internal/obs/ go.mod go.sum
git commit -m "feat(obs): metric registries + typed helpers for control plane and agents"
```

---

### Task 2: `internal/obs` — RequestID middleware

**Files:**
- Create: `internal/obs/requestid.go`
- Create: `internal/obs/requestid_test.go`

- [ ] **Step 1: Write the failing tests**

`internal/obs/requestid_test.go`:

```go
package obs

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestIDGeneratedWhenAbsent(t *testing.T) {
	var seen string
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestIDFromContext(r.Context())
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	if !strings.HasPrefix(seen, "req-") || len(seen) != 4+32 {
		t.Fatalf("generated id = %q, want req-<32 hex>", seen)
	}
	if got := rec.Header().Get(HeaderRequestID); got != seen {
		t.Fatalf("response header = %q, want %q (echoed)", got, seen)
	}
}

func TestRequestIDInboundHonored(t *testing.T) {
	var seen, fwd string
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestIDFromContext(r.Context())
		fwd = r.Header.Get(HeaderRequestID)
	}))
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set(HeaderRequestID, "req-abc123")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if seen != "req-abc123" {
		t.Fatalf("ctx id = %q, want inbound honored", seen)
	}
	if fwd != "req-abc123" {
		t.Fatalf("request header = %q, want set for proxy forwarding", fwd)
	}
	if rec.Header().Get(HeaderRequestID) != "req-abc123" {
		t.Fatal("response header not echoed")
	}
}

func TestRequestIDUnique(t *testing.T) {
	ids := map[string]bool{}
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ids[RequestIDFromContext(r.Context())] = true
	}))
	for i := 0; i < 50; i++ {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil))
	}
	if len(ids) != 50 {
		t.Fatalf("got %d unique ids from 50 requests", len(ids))
	}
}

func TestRequestIDFromContextEmptyWithout(t *testing.T) {
	if got := RequestIDFromContext(httptest.NewRequest("GET", "/", nil).Context()); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}
```

- [ ] **Step 2: Run tests, verify failure**

Run: `go test ./internal/obs/ -run RequestID -count=1`
Expected: FAIL — undefined `RequestID`.

- [ ] **Step 3: Implement `internal/obs/requestid.go`**

```go
package obs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

// HeaderRequestID is the correlation header accepted, forwarded, and echoed.
const HeaderRequestID = "X-Request-ID"

type ridKey struct{}

// RequestID accepts an inbound X-Request-ID or generates req-<128-bit hex>,
// stores it in the request context, sets it on the REQUEST headers (so the
// reverse proxy forwards it to the agent), and echoes it on the response.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(HeaderRequestID)
		if id == "" {
			var b [16]byte
			_, _ = rand.Read(b[:])
			id = "req-" + hex.EncodeToString(b[:])
		}
		r.Header.Set(HeaderRequestID, id)
		w.Header().Set(HeaderRequestID, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ridKey{}, id)))
	})
}

// RequestIDFromContext returns the id stored by RequestID, or "".
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(ridKey{}).(string)
	return id
}
```

- [ ] **Step 4: Run tests, verify pass**

Run: `go test ./internal/obs/ -count=1`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/obs/requestid.go internal/obs/requestid_test.go
git commit -m "feat(obs): X-Request-ID middleware with generate/honor/echo semantics"
```

---

### Task 3: `internal/obs` — fan-out merge handler

**Files:**
- Create: `internal/obs/fanout.go`
- Create: `internal/obs/fanout_test.go`

- [ ] **Step 1: Write the failing tests**

`internal/obs/fanout_test.go`:

```go
package obs

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// fakeAgent serves a fixed body (or behavior) at /metrics.
func fakeAgent(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func exposition(agent string) string {
	return fmt.Sprintf(`# HELP agent_turns_total Agent turns by outcome (completed/error/aborted/continue).
# TYPE agent_turns_total counter
agent_turns_total{agent=%q,outcome="completed"} 3
`, agent)
}

func TestFanoutMergesHealthyAgents(t *testing.T) {
	a1 := fakeAgent(t, func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, exposition("alpha")) })
	a2 := fakeAgent(t, func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, exposition("beta")) })
	c := NewControlMetrics()
	h := FanoutHandler(c, func() []ScrapeTarget {
		return []ScrapeTarget{{Agent: "alpha", Addr: a1}, {Agent: "beta", Addr: a2}}
	})
	body := scrapeHandler(t, h)
	// Exactly ONE TYPE line for the family despite two agents (merge, not concat).
	if n := strings.Count(body, "# TYPE agent_turns_total counter"); n != 1 {
		t.Fatalf("TYPE lines for agent_turns_total = %d, want 1\n%s", n, body)
	}
	for _, agent := range []string{"alpha", "beta"} {
		if !strings.Contains(body, fmt.Sprintf(`agent_turns_total{agent=%q,outcome="completed"} 3`, agent)) {
			t.Fatalf("missing %s series:\n%s", agent, body)
		}
	}
	// Control-plane families present too.
	if !strings.Contains(body, "runtime_agent_up") {
		t.Fatalf("control families missing:\n%s", body)
	}
	if v := testutil.ToFloat64(c.agentUp.WithLabelValues("alpha")); v != 1 {
		t.Fatalf("alpha up = %v, want 1", v)
	}
}

func TestFanoutSkipsHangingAgent(t *testing.T) {
	healthy := fakeAgent(t, func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, exposition("alpha")) })
	hung := fakeAgent(t, func(w http.ResponseWriter, _ *http.Request) { time.Sleep(3 * time.Second) })
	c := NewControlMetrics()
	h := FanoutHandler(c, func() []ScrapeTarget {
		return []ScrapeTarget{{Agent: "alpha", Addr: healthy}, {Agent: "hung", Addr: hung}}
	})
	start := time.Now()
	body := scrapeHandler(t, h)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("scrape took %v — hung agent not bounded by timeout", elapsed)
	}
	if !strings.Contains(body, `agent_turns_total{agent="alpha"`) {
		t.Fatal("healthy agent missing")
	}
	if v := testutil.ToFloat64(c.agentUp.WithLabelValues("hung")); v != 0 {
		t.Fatalf("hung up = %v, want 0", v)
	}
	if v := testutil.ToFloat64(c.scrapeSkips.WithLabelValues("hung", "timeout")); v != 1 {
		t.Fatalf("skip counter = %v, want 1", v)
	}
}

func TestFanoutSkipsMalformedExposition(t *testing.T) {
	bad := fakeAgent(t, func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "{{{not exposition") })
	c := NewControlMetrics()
	h := FanoutHandler(c, func() []ScrapeTarget {
		return []ScrapeTarget{{Agent: "bad", Addr: bad}}
	})
	body := scrapeHandler(t, h)
	if !strings.Contains(body, "runtime_agent_up") {
		t.Fatal("merged output invalid")
	}
	if v := testutil.ToFloat64(c.agentUp.WithLabelValues("bad")); v != 0 {
		t.Fatalf("bad up = %v, want 0", v)
	}
	if v := testutil.ToFloat64(c.scrapeSkips.WithLabelValues("bad", "parse")); v != 1 {
		t.Fatalf("skip reason parse = %v, want 1", v)
	}
}

func TestFanout404IsNoMetricsButStillUp(t *testing.T) {
	shim := fakeAgent(t, func(w http.ResponseWriter, _ *http.Request) { http.NotFound(w, nil) })
	c := NewControlMetrics()
	h := FanoutHandler(c, func() []ScrapeTarget {
		return []ScrapeTarget{{Agent: "shim", Addr: shim}}
	})
	_ = scrapeHandler(t, h)
	if v := testutil.ToFloat64(c.agentUp.WithLabelValues("shim")); v != 1 {
		t.Fatalf("shim up = %v, want 1 (404 proves serving)", v)
	}
	if v := testutil.ToFloat64(c.scrapeSkips.WithLabelValues("shim", "no_metrics")); v != 1 {
		t.Fatalf("skip reason no_metrics = %v, want 1", v)
	}
}

func TestFanoutUnreachableAgent(t *testing.T) {
	c := NewControlMetrics()
	h := FanoutHandler(c, func() []ScrapeTarget {
		return []ScrapeTarget{{Agent: "gone", Addr: "127.0.0.1:1"}}
	})
	body := scrapeHandler(t, h)
	if !strings.Contains(body, "runtime_metrics_scrape_skips_total") {
		t.Fatalf("skip counter family missing:\n%s", body)
	}
	if v := testutil.ToFloat64(c.agentUp.WithLabelValues("gone")); v != 0 {
		t.Fatalf("gone up = %v, want 0", v)
	}
}
```

- [ ] **Step 2: Run tests, verify failure**

Run: `go test ./internal/obs/ -run Fanout -count=1`
Expected: FAIL — undefined `FanoutHandler`, `ScrapeTarget`.

- [ ] **Step 3: Implement `internal/obs/fanout.go`**

```go
package obs

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
)

// ScrapeTarget is one supervised agent's metrics endpoint.
type ScrapeTarget struct {
	Agent string // agent id (used for up/skip series labels)
	Addr  string // host:port (the agent's listen_addr)
}

// perAgentTimeout bounds each sub-scrape so one sick agent can never stall
// the merged scrape (spec §3.4).
const perAgentTimeout = 500 * time.Millisecond

// FanoutHandler serves the merged exposition: the control registry's own
// families plus every healthy agent's families, merged by name (NOT text
// concatenation — duplicate TYPE/HELP blocks are invalid). Sub-scrapes run
// concurrently; skip rules: timeout/unreachable/non-200/parse ⇒ agent omitted
// this scrape + skip counter + up=0. A 404 means the process serves HTTP but
// has no /metrics (foreign shim) ⇒ reason no_metrics, up STAYS 1.
func FanoutHandler(c *ControlMetrics, targets func() []ScrapeTarget) http.Handler {
	client := &http.Client{}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		type result struct {
			agent    string
			families map[string]*dto.MetricFamily
		}
		ts := targets()
		results := make([]result, len(ts))
		var wg sync.WaitGroup
		for i, tgt := range ts {
			wg.Add(1)
			go func(i int, tgt ScrapeTarget) {
				defer wg.Done()
				fams, up, reason := scrapeOne(r.Context(), client, tgt)
				c.AgentUp(tgt.Agent, up)
				if reason != "" {
					c.ScrapeSkip(tgt.Agent, reason)
					level := slog.LevelWarn
					if reason == "no_metrics" {
						level = slog.LevelDebug
					}
					slog.Log(r.Context(), level, "metrics fan-out skip",
						"agent", tgt.Agent, "reason", reason)
				}
				results[i] = result{agent: tgt.Agent, families: fams}
			}(i, tgt)
		}
		wg.Wait()

		// Merge: own registry families first, then each agent's, by name.
		merged := map[string]*dto.MetricFamily{}
		own, err := c.reg.Gather()
		if err == nil {
			for _, mf := range own {
				merged[mf.GetName()] = mf
			}
		}
		for _, res := range results {
			for name, mf := range res.families {
				if exist, ok := merged[name]; ok {
					exist.Metric = append(exist.Metric, mf.Metric...)
				} else {
					merged[name] = mf
				}
			}
		}
		names := make([]string, 0, len(merged))
		for n := range merged {
			names = append(names, n)
		}
		sort.Strings(names)
		w.Header().Set("Content-Type", string(expfmt.NewFormat(expfmt.TypeTextPlain)))
		for _, n := range names {
			if _, err := expfmt.MetricFamilyToText(w, merged[n]); err != nil {
				return // client gone; nothing useful to do
			}
		}
	})
}

// scrapeOne fetches and parses one agent's exposition.
// Returns (families, up, skipReason); skipReason "" means scraped clean.
func scrapeOne(ctx context.Context, client *http.Client, tgt ScrapeTarget) (map[string]*dto.MetricFamily, bool, string) {
	ctx, cancel := context.WithTimeout(ctx, perAgentTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", "http://"+tgt.Addr+"/metrics", nil)
	if err != nil {
		return nil, false, "error"
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, false, "timeout"
		}
		return nil, false, "unreachable"
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, true, "no_metrics" // serving, just no endpoint (foreign shim)
	case resp.StatusCode != http.StatusOK:
		return nil, false, fmt.Sprintf("status_%d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, false, "error"
	}
	var parser expfmt.TextParser
	fams, err := parser.TextToMetricFamilies(strings.NewReader(string(body)))
	if err != nil {
		return nil, false, "parse"
	}
	return fams, true, ""
}
```

Note: `github.com/prometheus/client_model` arrives transitively with client_golang; `go mod tidy` promotes it.

- [ ] **Step 4: Run tests, verify pass**

Run: `go test ./internal/obs/ -count=1`
Expected: all PASS (the hanging-agent test takes ~500ms).

- [ ] **Step 5: Commit**

```bash
go mod tidy
git add internal/obs/fanout.go internal/obs/fanout_test.go go.mod go.sum
git commit -m "feat(obs): fan-out scrape handler with expfmt family merge"
```

---

### Task 4: agentd instrumentation — /metrics, request-id log, turn + tool metrics

**Files:**
- Modify: `agentruntime/server.go` (mount /metrics, wrap mux)
- Modify: `agentruntime/serve.go` (Manager field, Serve wiring, turn-step instrumentation)
- Modify: `agentruntime/turnstep.go` (turnInput gains RequestID)
- Test: `agentruntime/metrics_test.go` (new)

- [ ] **Step 1: Write the failing tests**

`agentruntime/metrics_test.go`:

```go
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
	m := &Manager{agentID: "support"} // metrics nil
	m.observeTurn("completed", time.Second, nil, nil) // must not panic
}
```

- [ ] **Step 2: Run tests, verify failure**

Run: `go test ./agentruntime/ -run 'Metrics|RequestID|ObserveTurn' -count=1`
Expected: FAIL — `m.metrics`, `m.handler`, `m.observeTurn` undefined.

- [ ] **Step 3: Implement**

In `agentruntime/serve.go`, add to the `Manager` struct (find `type Manager struct` — it has `agentID`, `cfg`, `dbosCtx`, `st`, `subscribers`):

```go
	metrics *obs.AgentMetrics // nil-safe; turn/token/tool series labeled agent=<id>
```

In `Serve()` where `m := &Manager{...}` is built, add the field:

```go
		metrics:     obs.NewAgentMetrics(cfg.Spec.ID),
```

Change the server line in `Serve()` from `Handler: m.newMux()` to:

```go
	srv := &http.Server{Addr: listenAddr, Handler: m.handler()}
```

Add `observeTurn` to `agentruntime/serve.go` (near `sessionWorkflow`):

```go
// observeTurn records one turn's metrics: outcome counter, duration histogram,
// token counters (nil usage ⇒ skipped), and a tool-call counter per
// EntryTypeToolCall entry the turn produced. Safe with nil metrics.
func (m *Manager) observeTurn(outcome string, dur time.Duration, usage *llm.Usage, entries []session.SessionEntry) {
	m.metrics.TurnObserved(outcome, dur, usage)
	for _, e := range entries {
		if e.Type == session.EntryTypeToolCall {
			m.metrics.ToolCallObserved(e.Tool)
		}
	}
}
```

Instrument the turn inside the `RunAsStep` closure in `sessionWorkflow` (`serve.go:159` area). The closure currently ends:

```go
			tr, terr := rt.RunTurn(stepCtx, userMsg, images, nil) // headless (emit=nil)
			if terr != nil {
				return turnOutput{}, terr
			}
			return turnOutput{Done: tr.Done, Reason: tr.StopReason, Entries: tr.Entries}, nil
```

Replace with (metrics INSIDE the closure: DBOS skips the closure on replay, so replayed turns are not double-counted):

```go
			turnStart := time.Now()
			tr, terr := rt.RunTurn(stepCtx, userMsg, images, nil) // headless (emit=nil)
			if terr != nil {
				m.observeTurn("error", time.Since(turnStart), nil, nil)
				return turnOutput{}, terr
			}
			m.observeTurn(tr.StopReason, time.Since(turnStart), tr.Usage, tr.Entries)
			slog.Info("turn", "agent", m.agentID, "session", wfID, "turn", turn,
				"reason", tr.StopReason, "request_id", in.RequestID)
			return turnOutput{Done: tr.Done, Reason: tr.StopReason, Entries: tr.Entries}, nil
```

(add `"log/slog"`, `"github.com/sausheong/runtime/internal/obs"` to serve.go imports; `time`, `llm`, `session` are already imported.)

In `agentruntime/turnstep.go`, add to `turnInput`:

```go
	// RequestID is the X-Request-ID of the POST /sessions that started this
	// workflow, stamped into turn-step log lines for cross-process
	// correlation. Part of the checkpointed input: replay-safe.
	RequestID string `json:"request_id,omitempty"`
```

In `agentruntime/server.go`:

1. Add the route inside `newMux()`:

```go
	mux.Handle("GET /metrics", m.metrics.Handler())
```

2. Add a `handler()` method composing the middleware (below `newMux`):

```go
// handler wraps the mux with request-id + a structured access log so agent
// log lines carry the same X-Request-ID as the control plane's.
func (m *Manager) handler() http.Handler {
	mux := m.newMux()
	logged := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
		if r.URL.Path == "/healthz" || r.URL.Path == "/metrics" {
			return // high-frequency probes; don't flood the log
		}
		slog.Info("agent request", "agent", m.agentID, "method", r.Method,
			"path", r.URL.Path, "request_id", obs.RequestIDFromContext(r.Context()))
	})
	return obs.RequestID(logged)
}
```

(add `"log/slog"` and `"github.com/sausheong/runtime/internal/obs"` to server.go imports.)

3. In the `POST /sessions` handler in `newMux()`, find where the turn input is built / `m.startSession`-style call is made (the handler decodes `body` then starts the workflow — follow the call into where `turnInput` is constructed, `serve.go:~195` `startTurn`/`RunWorkflow` site) and thread the request id: change the function that builds `turnInput` to accept and set `RequestID: obs.RequestIDFromContext(r.Context())`. The POST handler has `r` in scope; pass `obs.RequestIDFromContext(r.Context())` down as an extra string argument to the session-start helper and set it on the `turnInput` literal there.

- [ ] **Step 4: Run tests, verify pass**

Run: `go test ./agentruntime/ -count=1`
Expected: all PASS (existing tests must stay green — `newMux` callers in tests are unaffected; if a test serves `m.newMux()` directly it still works).

- [ ] **Step 5: Commit**

```bash
git add agentruntime/
git commit -m "feat(agentd): /metrics endpoint, turn/token/tool metrics, request-id correlation"
```

---

### Task 5: runtimed edge — HTTP metrics, request-id in access log, restart/proxy hooks

**Files:**
- Modify: `cmd/runtimed/main.go` (accessLog signature, RequestID wrap, supervisor hook)
- Modify: `controlplane/supervisor.go` (OnRestart hook)
- Modify: `controlplane/proxy.go` (reverseProxy error hook)
- Modify: `controlplane/api.go` (NewAPI threading metrics)
- Test: `controlplane/supervisor_test.go` (extend), `cmd/runtimed/accesslog_test.go` (new)

- [ ] **Step 1: Write the failing tests**

Append to `controlplane/supervisor_test.go`:

```go
func TestSupervisorOnRestartFires(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	restarts := 0
	spawns := 0
	s := &Supervisor{
		Backoff: time.Millisecond,
		OnRestart: func() { restarts++ },
		Spawn: func(ctx context.Context) <-chan error {
			spawns++
			ch := make(chan error, 1)
			if spawns >= 3 {
				cancel() // stop after third spawn
			}
			ch <- nil // exit immediately
			return ch
		},
	}
	s.Run(ctx)
	// 3 spawns = first start + 2 restarts. OnRestart must NOT fire for the
	// first spawn.
	if restarts != 2 {
		t.Fatalf("restarts = %d, want 2 (spawns=%d)", restarts, spawns)
	}
}
```

Create `cmd/runtimed/accesslog_test.go`:

```go
package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sausheong/runtime/internal/obs"
)

func TestAccessLogRouteNormalization(t *testing.T) {
	cm := obs.NewControlMetrics()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /agents/{id}/sessions", func(w http.ResponseWriter, r *http.Request) {})
	h := accessLog(mux, cm)
	// Two different raw paths, same pattern → ONE series.
	for _, p := range []string{"/agents/support/sessions", "/agents/research/sessions"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", p, nil))
	}
	body := scrapeControl(t, cm)
	if !contains(body, `runtime_http_requests_total{method="GET",route="/agents/{id}/sessions",status="200"} 2`) {
		t.Fatalf("normalized route series missing or split:\n%s", body)
	}
	// Unmatched path → route="unmatched", not the raw path.
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/no/such/route", nil))
	body = scrapeControl(t, cm)
	if !contains(body, `route="unmatched"`) {
		t.Fatalf("unmatched bucket missing:\n%s", body)
	}
	if contains(body, "/no/such/route") {
		t.Fatalf("raw path leaked into labels:\n%s", body)
	}
}

func scrapeControl(t *testing.T, cm *obs.ControlMetrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	obs.FanoutHandler(cm, func() []obs.ScrapeTarget { return nil }).
		ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Body.String()
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
```

(add `"strings"` import.)

- [ ] **Step 2: Run tests, verify failure**

Run: `go test ./controlplane/ ./cmd/runtimed/ -run 'OnRestart|AccessLog' -count=1`
Expected: FAIL — `OnRestart` field and 2-arg `accessLog` undefined.

- [ ] **Step 3: Implement**

`controlplane/supervisor.go` — add field + call:

```go
	// OnRestart fires before each RESPAWN (not the first spawn). Used for
	// restart metrics; nil ⇒ no-op.
	OnRestart func()
```

In `Run`, the loop spawns at the top of each `for` iteration. Track first spawn:

```go
	backoff := base
	first := true
	for {
		if ctx.Err() != nil {
			return
		}
		if !first && s.OnRestart != nil {
			s.OnRestart()
		}
		first = false
		start := time.Now()
		wait := s.Spawn(ctx)
		...
```

`controlplane/proxy.go` — give `reverseProxy` an error callback:

```go
// reverseProxy builds a passthrough to the agent subprocess at addr.
// FlushInterval = -1 ensures SSE/streaming responses are flushed immediately
// so events pass through promptly. onError (nil-safe) fires on each proxy
// failure before the 503 is written.
func reverseProxy(addr string, onError func()) *httputil.ReverseProxy {
	target, _ := url.Parse("http://" + addr)
	rp := httputil.NewSingleHostReverseProxy(target)
	rp.FlushInterval = -1
	rp.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		if onError != nil {
			onError()
		}
		http.Error(w, "agent unavailable", http.StatusServiceUnavailable)
	}
	return rp
}
```

`controlplane/api.go` — thread metrics through `NewAPI`:

```go
import (
	...
	"github.com/sausheong/runtime/internal/obs"
)

// NewAPI returns the control-plane HTTP handler routing /agents/{id}/... to
// each agent's subprocess, plus GET /agents and GET /healthz. m (nil-safe)
// records proxy failures.
func NewAPI(reg *Registry, m *obs.ControlMetrics) *http.ServeMux {
```

and change the proxy call site (`api.go:80`):

```go
		reverseProxy(ap.Addr, func() { m.ProxyError(ap.AgentID) }).ServeHTTP(w, r)
```

Fix ALL existing `NewAPI(reg)` callers (tests in `controlplane/`, `cmd/runtimed/main.go` `buildRoot`) to pass `nil` or the real metrics: `grep -rn "NewAPI(" --include="*.go" .` and update each. In `buildRoot`, change the signature to accept `cm *obs.ControlMetrics` and pass it to `NewAPI(reg, cm)`.

`cmd/runtimed/main.go`:

1. In `main()`, before the handler chain is built, create the metrics: `cm := obs.NewControlMetrics()`.
2. Change `accessLog` to take the metrics and record route/request-id:

```go
// accessLog logs one structured line per request, including the authenticated
// principal subject/tenant and the request id, and records HTTP metrics under
// the NORMALIZED route pattern (never the raw path — cardinality rule).
func accessLog(next http.Handler, cm *obs.ControlMetrics) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r)
		route := r.Pattern // set by ServeMux on match (Go 1.22+), e.g. "GET /agents/{id}"
		if route == "" {
			route = "unmatched"
		} else if i := strings.IndexByte(route, ' '); i >= 0 {
			route = route[i+1:] // strip "METHOD " prefix
		}
		cm.HTTPObserved(route, r.Method, rec.status, time.Since(start))
		var subject, tenant string
		if p, ok := controlplane.PrincipalFromContext(r.Context()); ok {
			subject, tenant = p.Subject, p.TenantID
		}
		slog.Info("request",
			"method", r.Method, "path", r.URL.Path,
			"status", rec.status, "subject", subject, "tenant", tenant,
			"remote", r.RemoteAddr, "request_id", obs.RequestIDFromContext(r.Context()))
	})
}
```

(add `"strings"` and the obs import; `time` already imported.)

3. Update both handler-chain sites to wrap with RequestID OUTERMOST and pass `cm`:

```go
		handler = obs.RequestID(accessLog(buildRoot(reg, nil, console.OIDCConfig{}, secretAdmin, gwHandler), cm))
```

and in the identity branch:

```go
		handler = obs.RequestID(controlplane.IdentityMiddleware(accessLog(root, cm), authr, azr))
```

Wait — spec §3.5 says RequestID sits OUTSIDE accessLog so the log line can read the id; identity rejection responses should also carry the id, so RequestID must be outermost (outside IdentityMiddleware too). The two lines above satisfy both.

4. Wire the supervisor hook in the agent-start loop:

```go
		sup := &controlplane.Supervisor{Spawn: ap.SpawnFunc(), Backoff: time.Second,
			OnRestart: func() { cm.AgentRestart(ap.AgentID) }}
```

- [ ] **Step 4: Run tests + vet, verify pass**

Run: `go test ./controlplane/ ./cmd/runtimed/ -count=1 && go vet ./...`
Expected: all PASS, no vet errors (catches missed NewAPI callers).

- [ ] **Step 5: Commit**

```bash
git add controlplane/ cmd/runtimed/
git commit -m "feat(runtimed): HTTP metrics with route normalization, request-id access log, restart/proxy hooks"
```

---

### Task 6: runtimed — mount /metrics fan-out endpoint

**Files:**
- Modify: `cmd/runtimed/main.go`
- Test: `cmd/runtimed/metrics_mount_test.go` (new)

- [ ] **Step 1: Write the failing test**

`cmd/runtimed/metrics_mount_test.go`:

```go
package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sausheong/runtime/internal/obs"
)

func TestMountMetricsBypassesInnerHandler(t *testing.T) {
	cm := obs.NewControlMetrics()
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized) // simulates identity middleware
	})
	h := mountMetrics(inner, cm, func() []obs.ScrapeTarget { return nil })

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 {
		t.Fatalf("/metrics status = %d, want 200 (must bypass identity)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "runtime_http_requests_total") &&
		!strings.Contains(rec.Body.String(), "runtime_agent_up") {
		t.Fatalf("/metrics body missing control families:\n%s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/agents", nil))
	if rec.Code != 401 {
		t.Fatalf("inner route status = %d, want 401 (everything else still goes through the chain)", rec.Code)
	}
}
```

- [ ] **Step 2: Run test, verify failure**

Run: `go test ./cmd/runtimed/ -run MountMetrics -count=1`
Expected: FAIL — `mountMetrics` undefined.

- [ ] **Step 3: Implement in `cmd/runtimed/main.go`**

```go
// mountMetrics overlays GET /metrics OUTSIDE the identity/access-log chain
// (like /healthz — standard Prometheus practice; spec §5: label values are
// operator-level identifiers, never tenant/user data). Everything else falls
// through to the wrapped handler chain.
func mountMetrics(inner http.Handler, cm *obs.ControlMetrics, targets func() []obs.ScrapeTarget) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", obs.FanoutHandler(cm, targets))
	mux.Handle("/", inner)
	return mux
}
```

In `main()`, after the `handler = ...` chain is finalized (both branches) and before `srv := &http.Server{...}`, wrap once:

```go
	handler = mountMetrics(handler, cm, func() []obs.ScrapeTarget {
		var ts []obs.ScrapeTarget
		for _, info := range reg.List() {
			ap, _ := reg.Get(info.ID)
			ts = append(ts, obs.ScrapeTarget{Agent: ap.AgentID, Addr: ap.Addr})
		}
		return ts
	})
```

- [ ] **Step 4: Run tests, verify pass**

Run: `go test ./cmd/runtimed/ -count=1 && go build ./...`
Expected: PASS, builds.

- [ ] **Step 5: Commit**

```bash
git add cmd/runtimed/
git commit -m "feat(runtimed): /metrics fan-out endpoint outside the identity chain"
```

---

### Task 7: gateway instrumentation — tool calls + upstream up/down

**Files:**
- Modify: `internal/gateway/server.go` (Handler.Metrics field, toolHandler timing)
- Modify: `internal/gateway/manager.go` (Manager.Metrics field, up/down transitions)
- Test: `internal/gateway/metrics_test.go` (new)

- [ ] **Step 1: Write the failing test**

`internal/gateway/metrics_test.go` — follow the existing test-fixture pattern in `internal/gateway/server_test.go` (it builds a Handler over a Manager with an in-memory upstream via `sdk.NewInMemoryTransports`; reuse its helpers — read that file first). The new test, adapted to those helpers:

```go
package gateway

import (
	"strings"
	"testing"

	"github.com/sausheong/runtime/internal/obs"
)

// TestToolCallMetricsRecorded drives one successful and one isError tool call
// through toolHandler and asserts the outcome-labeled counters. Reuse the
// fixture from server_test.go that yields a connected client session; the
// fake upstream tool there returns success for normal args. For the error
// path use a tool name that does not exist on the upstream server view —
// or whatever the existing tests use to produce an errResult.
func TestToolCallMetricsRecorded(t *testing.T) {
	cm := obs.NewControlMetrics()
	// ... build handler exactly as the existing happy-path federation test
	// does, then set: h.Metrics = cm
	// ... call a federated tool successfully once
	// ... trigger one errResult call (viewer role, foreign view, or upstream isError)
	body := scrapeControlRegistry(t, cm)
	if !strings.Contains(body, `runtime_gateway_tool_calls_total{outcome="ok"`) &&
		!strings.Contains(body, `outcome="ok"`) {
		t.Fatalf("ok call not counted:\n%s", body)
	}
	if !strings.Contains(body, `outcome="error"`) {
		t.Fatalf("error call not counted:\n%s", body)
	}
}
```

with helper:

```go
func scrapeControlRegistry(t *testing.T, cm *obs.ControlMetrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	obs.FanoutHandler(cm, func() []obs.ScrapeTarget { return nil }).
		ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Body.String()
}
```

The implementer MUST adapt the `...` sections to the real fixtures in `server_test.go` — the assertions and the `h.Metrics = cm` injection are the contract. Also add an upstream-up test:

```go
func TestUpstreamUpGaugeTransitions(t *testing.T) {
	cm := obs.NewControlMetrics()
	// Build a Manager as manager_test.go does with one in-memory/fake upstream,
	// set m.Metrics = cm BEFORE Start, start it, wait for connection
	// (existing tests poll Status() for state "up").
	// Assert gauge 1 after connect; then force markDown (as the existing
	// mid-flight-failure test does) and assert gauge 0.
}
```

- [ ] **Step 2: Run tests, verify failure**

Run: `go test ./internal/gateway/ -run 'ToolCallMetrics|UpstreamUpGauge' -count=1`
Expected: FAIL — `Metrics` field undefined.

- [ ] **Step 3: Implement**

`internal/gateway/server.go` — add to the `Handler` struct:

```go
	// Metrics (nil-safe) records federated tool calls. Set once before serving.
	Metrics *obs.ControlMetrics
```

In `toolHandler`, instrument around the execute path. The current body returns at: view-check errResult, viewer errResult, injectTenant errResult, `t.Execute` err, `res.Error != ""`, and success. Only the calls that REACH the upstream count (gate rejections are not upstream calls — they're authz failures, already visible as nothing reached the tool). Wrap from just before `t.Execute`:

```go
		serverName, _, _ := strings.Cut(t.Name(), "__") // sound: "__" banned in server names
		start := time.Now()
		res, err := t.Execute(ctx, args)
		dur := time.Since(start)
		if err != nil {
			h.Metrics.GatewayCall(serverName, t.Name(), "error", dur)
			return errResult(err.Error()), nil
		}
		if res.Error != "" {
			h.Metrics.GatewayCall(serverName, t.Name(), "error", dur)
			return errResult(res.Error), nil
		}
		h.Metrics.GatewayCall(serverName, t.Name(), "ok", dur)
```

(add `"strings"`, `"time"`, obs imports as needed. Label `tool` uses the full namespaced `t.Name()` (`<server>__<tool>`) — bounded by catalog size, satisfies the cardinality rules, and self-disambiguates across upstreams.)

`internal/gateway/manager.go` — add to the `Manager` struct:

```go
	// Metrics (nil-safe) tracks upstream connection state. Set before Start.
	Metrics *obs.ControlMetrics
```

At the connect-success site (`manager.go:125-132`, after `u.mu.Unlock()` where "gateway: upstream connected" is logged) add:

```go
			m.Metrics.GatewayUpstreamUp(u.cfg.Name, true)
```

In `markDown`, after the connection is confirmed-current and cleared (inside the success branch of the identity check, where the down state is recorded) add:

```go
	m.Metrics.GatewayUpstreamUp(u.cfg.Name, false)
```

Also at the connect-FAILURE site (the `dial` error branch) add the same `false` call so the gauge exists (as 0) from first failed attempt, not only after an up→down transition.

`cmd/runtimed/main.go` — wire both after `cm` exists (where `gwManager`/`gwHandler` are built):

```go
	if gwManager != nil {
		gwManager.Metrics = cm
	}
	if gwHandler != nil {
		gwHandler.Metrics = cm
	}
```

(Order note: `cm := obs.NewControlMetrics()` must be created BEFORE the gateway is built in `main()` — move it to just after config load.)

- [ ] **Step 4: Run tests, verify pass**

Run: `go test ./internal/gateway/ -count=1 && go build ./...`
Expected: all PASS including pre-existing gateway tests.

- [ ] **Step 5: Commit**

```bash
git add internal/gateway/ cmd/runtimed/
git commit -m "feat(gateway): tool-call and upstream-up metrics"
```

---

### Task 8: runtimectl — print X-Request-ID on invoke -v

**Files:**
- Modify: `cmd/runtimectl/main.go`

- [ ] **Step 1: Implement (no unit test — thin CLI I/O; covered by e2e)**

In `main()`'s `case "invoke":`, support a `-v` flag in `rest`:

```go
	case "invoke":
		msg := "hello"
		verbose := false
		for _, a := range rest {
			if a == "-v" {
				verbose = true
			} else {
				msg = a
			}
		}
		invoke(base, resolveAgent(base, agent), msg, verbose)
```

Change `invoke`:

```go
func invoke(base, agent, msg string, verbose bool) {
	body, _ := json.Marshal(map[string]string{"message": msg})
	resp, err := authdPost(base+"/agents/"+agent+"/sessions", "application/json", bytes.NewReader(body))
	check(err)
	if verbose {
		fmt.Fprintln(os.Stderr, "request-id:", resp.Header.Get("X-Request-ID"))
	}
	...rest unchanged...
}
```

Update the usage line to mention `invoke [-v]`.

- [ ] **Step 2: Verify build + behavior**

Run: `go build ./... && go vet ./cmd/runtimectl/`
Expected: clean.

- [ ] **Step 3: Commit**

```bash
git add cmd/runtimectl/
git commit -m "feat(runtimectl): invoke -v prints the response X-Request-ID"
```

---

### Task 9: through-serve e2e test

**Files:**
- Create: `test/observability_e2e_test.go`

- [ ] **Step 1: Read the existing e2e scaffolding**

Read `test/gateway_e2e_test.go` and `test/gateway_sandbox_e2e_test.go` — they establish the repo's pattern for booting runtimed+agentd binaries (build, env, port choice, health polling, cleanup). REUSE that scaffolding style exactly; do not invent a new harness.

- [ ] **Step 2: Write the test**

`test/observability_e2e_test.go` (build tag `//go:build integration`, ports 8160/8161/8162 to avoid sibling collisions). Shape:

```go
//go:build integration

package test

// TestObservabilityE2E boots runtimed with TWO test agents (kind "" ⇒
// scripted; runtime config written to a temp dir, open mode — no identity),
// then asserts:
//
//  1. POST /agents/support/sessions returns X-Request-ID header (req- prefix),
//     and a second invoke with an explicit inbound X-Request-ID gets it echoed.
//  2. After driving one session per agent to completion (poll
//     GET /agents/{id}/sessions until status completed — same polling
//     pattern as the existing e2e tests), GET /metrics on runtimed:
//       - contains `agent_turns_total{agent="support"` AND `agent="research"`
//         with a value ≥ 1 (parse with expfmt or string-match; string-match ok),
//       - contains `runtime_agent_up{agent="support"} 1` and research likewise,
//       - contains `runtime_http_requests_total` with route label
//         "/agents/{id}/sessions" (normalized — assert the literal pattern
//         string appears and the raw "/agents/support/sessions" does NOT
//         appear anywhere in a label),
//       - response parses cleanly: feed the whole body to
//         expfmt.TextParser{}.TextToMetricFamilies and require no error
//         (this is the merge-validity assertion).
//  3. GET /metrics returns 200 even with identity ON: re-run a runtimed
//     variant with a service key configured (mirror the identity setup in
//     gateway_sandbox_e2e_test.go) and assert GET /metrics WITHOUT a token
//     is 200 while GET /agents without a token is 401.
//
// Gateway counter assertion is covered by internal/gateway unit tests
// (Task 7); this e2e stays gateway-free to keep boot scope small.
```

Write the full test implementing exactly that comment, reusing the sibling files' helper functions (binary build helpers, waitHealthy, etc. — whatever they're named there; if helpers are file-local, copy the minimal pieces into this file rather than refactoring shared files).

- [ ] **Step 3: Run it**

Run: `go test -tags integration ./test/ -run TestObservabilityE2E -count=1 -v -timeout 300s`
Expected: PASS. (Requires local Postgres per repo convention — same as sibling e2e tests.)

- [ ] **Step 4: Run the whole integration suite to check for port/state collisions**

Run: `go test -tags integration ./test/ -count=1 -timeout 900s`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add test/observability_e2e_test.go
git commit -m "test(e2e): metrics fan-out, route normalization, request-id, auth-free /metrics"
```

---

### Task 10: deploy overlay — Prometheus + Grafana + dashboard

**Files:**
- Create: `deploy/docker-compose.obs.yml`
- Create: `deploy/prometheus.yml`
- Create: `deploy/grafana/provisioning/datasources/prometheus.yml`
- Create: `deploy/grafana/provisioning/dashboards/provider.yml`
- Create: `deploy/grafana/dashboards/runtime.json`

- [ ] **Step 1: Write `deploy/prometheus.yml`**

```yaml
global:
  scrape_interval: 15s

scrape_configs:
  - job_name: runtime
    metrics_path: /metrics
    static_configs:
      # host.docker.internal: Prometheus runs in compose; runtimed typically on
      # the host during dev. For full-stack compose (docker-compose.full.yml),
      # override with the runtimed service name.
      - targets: ["host.docker.internal:8080"]
```

- [ ] **Step 2: Write `deploy/docker-compose.obs.yml`**

```yaml
# Observability overlay: Prometheus + Grafana with a provisioned dashboard.
# Usage:
#   docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.obs.yml up -d
# Grafana: http://localhost:3000 (anonymous viewer enabled), Prometheus: http://localhost:9090
services:
  prometheus:
    image: prom/prometheus:latest
    command: ["--config.file=/etc/prometheus/prometheus.yml"]
    volumes:
      - ./prometheus.yml:/etc/prometheus/prometheus.yml:ro
    ports:
      - "9090:9090"
    extra_hosts:
      - "host.docker.internal:host-gateway"

  grafana:
    image: grafana/grafana:latest
    environment:
      GF_AUTH_ANONYMOUS_ENABLED: "true"
      GF_AUTH_ANONYMOUS_ORG_ROLE: Viewer
    volumes:
      - ./grafana/provisioning:/etc/grafana/provisioning:ro
      - ./grafana/dashboards:/var/lib/grafana/dashboards:ro
    ports:
      - "3000:3000"
    depends_on:
      - prometheus
```

- [ ] **Step 3: Write Grafana provisioning**

`deploy/grafana/provisioning/datasources/prometheus.yml`:

```yaml
apiVersion: 1
datasources:
  - name: Prometheus
    type: prometheus
    access: proxy
    url: http://prometheus:9090
    isDefault: true
```

`deploy/grafana/provisioning/dashboards/provider.yml`:

```yaml
apiVersion: 1
providers:
  - name: runtime
    folder: Runtime
    type: file
    options:
      path: /var/lib/grafana/dashboards
```

- [ ] **Step 4: Write `deploy/grafana/dashboards/runtime.json`**

A single dashboard, schema v39+, uid `runtime-overview`, title "Runtime Overview", 15s refresh, with these panels (timeseries unless noted; all PromQL against the default datasource):

| Panel | Type | Query |
|---|---|---|
| Agent up | stat | `runtime_agent_up` |
| Agent restarts (1h) | stat | `increase(runtime_agent_restarts_total[1h])` |
| Turn rate by agent | timeseries | `sum by (agent) (rate(agent_turns_total[5m]))` |
| Turn latency p50/p95 | timeseries | `histogram_quantile(0.5, sum by (le) (rate(agent_turn_duration_seconds_bucket[5m])))` and the 0.95 twin |
| Turn outcomes | timeseries | `sum by (outcome) (rate(agent_turns_total[5m]))` |
| Token spend by agent | timeseries | `sum by (agent, direction) (rate(agent_tokens_total[5m]))` |
| Gateway calls by upstream | timeseries | `sum by (server, outcome) (rate(runtime_gateway_tool_calls_total[5m]))` |
| Gateway upstreams up | stat | `runtime_gateway_upstream_up` |
| HTTP request rate | timeseries | `sum by (route, status) (rate(runtime_http_requests_total[5m]))` |
| HTTP latency p95 | timeseries | `histogram_quantile(0.95, sum by (le, route) (rate(runtime_http_request_duration_seconds_bucket[5m])))` |
| Proxy errors | timeseries | `sum by (agent) (rate(runtime_proxy_errors_total[5m]))` |
| Scrape skips | timeseries | `sum by (agent, reason) (rate(runtime_metrics_scrape_skips_total[5m]))` |

Author the JSON by hand following Grafana's provisioned-dashboard schema (each panel: `type`, `title`, `gridPos`, `targets[{expr}]`; dashboard: `uid`, `title`, `panels`, `refresh: "15s"`, `time: {from: "now-1h", to: "now"}`, `schemaVersion: 39`). Keep `gridPos` a simple 2-column flow (w:12, h:8). Validate it is parseable JSON: `python3 -m json.tool deploy/grafana/dashboards/runtime.json > /dev/null`.

- [ ] **Step 5: Validate compose config**

Run: `docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.obs.yml config -q && python3 -m json.tool deploy/grafana/dashboards/runtime.json > /dev/null`
Expected: exit 0 both. (Daemon need not be running for `config -q`.)

- [ ] **Step 6: Commit**

```bash
git add deploy/
git commit -m "feat(deploy): Prometheus+Grafana overlay with provisioned runtime dashboard"
```

---

### Task 11: docs — README + ROADMAP + contract note

**Files:**
- Modify: `README.md`
- Modify: `ROADMAP.md`
- Modify: `conformance/` doc comment or contract doc IF one documents agent endpoints (grep for where /healthz + /meta are documented as the contract; add `/metrics` as OPTIONAL there)

- [ ] **Step 1: README**

Add an "## Observability" section after the sandbox section: `/metrics` on runtimed (what it aggregates, auth-free rationale + one-line security note), the metrics inventory table (copy from spec §3.2), `X-Request-ID` behavior + `runtimectl invoke -v`, the compose overlay quick start, and the cardinality promise. Add a features-table row and TOC entry. Update the Testing section with the new e2e test name.

- [ ] **Step 2: ROADMAP**

Mark B5 first milestone DONE with a summary entry following the established format (what shipped, key decisions, remaining B5 work: OTel tracing, sandboxd internals, per-tenant accounting, alerting, console panel, log shipping).

- [ ] **Step 3: Contract note**

Find where the agent HTTP contract is documented (`grep -rn "healthz" conformance/ docs/ --include="*.md" --include="*.go" -l`); add: agents MAY expose `GET /metrics` (Prometheus text format); the platform skips agents without it (reason `no_metrics`, not an error).

- [ ] **Step 4: Full check + commit**

```bash
go build ./... && go vet ./... && go test ./... -count=1
git add README.md ROADMAP.md conformance/ docs/
git commit -m "docs: README observability section + ROADMAP B5 M1 entry + optional /metrics contract note"
```

---

## Self-review (done at planning time)

- **Spec coverage:** §3.1 helpers → Task 1; §3.5 RequestID → Tasks 2/4/5; §3.4 fan-out → Task 3/6; agentd metrics + turn instrumentation → Task 4; control-plane HTTP/restart/proxy → Task 5; §4 gateway wiring → Task 7; runtimectl -v → Task 8; e2e → Task 9; compose overlay + dashboard → Task 10; README/ROADMAP/contract-optional-/metrics → Task 11. Live proof happens at milestone close-out (not a plan task, per repo convention).
- **Known judgment calls baked in:** gateway authz rejections (view/viewer gates) are NOT counted as gateway calls — only executions that reach the upstream; gateway `tool` label is the namespaced `t.Name()`; turn metrics live INSIDE the DBOS step closure so replays don't double-count; agentd skips access-log lines for /healthz//metrics probes.
- **Type consistency:** `obs.ControlMetrics`/`obs.AgentMetrics` method names match across Tasks 1, 4, 5, 6, 7; `ScrapeTarget{Agent, Addr}` matches Tasks 3, 6, e2e; `accessLog(next, cm)` matches Tasks 5 test and impl.
- **Honest gaps:** Task 7's test contains `...` fixture-adaptation markers because the gateway test fixtures must be reused, not duplicated blind — the implementer reads `server_test.go` first; assertions are fully specified. Task 9 similarly mandates reusing sibling e2e scaffolding with the full assertion list spelled out. Task 10's dashboard JSON is specified panel-by-panel with exact queries rather than inlined (≈400 lines of JSON would be noise; the table IS the spec).
