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

// Outcome label values for GatewayCall.
const (
	OutcomeOK    = "ok"
	OutcomeError = "error"
)

// turnBuckets cover LLM-turn and gateway-call durations: seconds to minutes.
// Prometheus default buckets top out at 10s, far too short for agent turns.
var turnBuckets = []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120}

// ControlMetrics is runtimed's registry: HTTP edge, agent supervision,
// reverse proxy, gateway federation, and fan-out scrape bookkeeping.
type ControlMetrics struct {
	reg            *prometheus.Registry
	httpRequests   *prometheus.CounterVec
	httpDuration   *prometheus.HistogramVec
	agentUp        *prometheus.GaugeVec
	agentReachable *prometheus.GaugeVec
	agentRestarts  *prometheus.CounterVec
	proxyErrors    *prometheus.CounterVec
	gwCalls        *prometheus.CounterVec
	gwDuration     *prometheus.HistogramVec
	gwUp           *prometheus.GaugeVec
	scrapeSkips    *prometheus.CounterVec
}

func NewControlMetrics() *ControlMetrics {
	// Deliberately no Go runtime/process collectors: unlabeled go_*/process_*
	// families from multiple agent registries would collide in the fan-out
	// merge; do not add collectors.NewGoCollector().
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
	c.agentReachable = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "runtime_agent_reachable",
		Help: "1 when a remote agent's /healthz was reachable on the last monitor poll (remote agents only).",
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
	c.reg.MustRegister(c.httpRequests, c.httpDuration, c.agentUp, c.agentReachable, c.agentRestarts,
		c.proxyErrors, c.gwCalls, c.gwDuration, c.gwUp, c.scrapeSkips)
	return c
}

// HTTPObserved records one control-plane HTTP request. route must be the
// matched mux pattern (e.g. /agents/{id}/sessions), never the raw URL path —
// raw paths explode label cardinality.
func (c *ControlMetrics) HTTPObserved(route, method string, status int, dur time.Duration) {
	if c == nil {
		return
	}
	c.httpRequests.WithLabelValues(route, method, strconv.Itoa(status)).Inc()
	c.httpDuration.WithLabelValues(route, method).Observe(dur.Seconds())
}

// AuthRejected counts an identity-middleware rejection (401/403/404/303)
// under route="auth_rejected" WITHOUT recording a duration observation —
// rejections aren't timed and would pollute the latency histogram with
// zero-second samples.
func (c *ControlMetrics) AuthRejected(status int) {
	if c == nil {
		return
	}
	c.httpRequests.WithLabelValues("auth_rejected", "", strconv.Itoa(status)).Inc()
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

// AgentReachable sets the remote-agent reachability gauge (1/0) on each
// HealthMonitor transition. Nil-safe like the other helpers.
func (c *ControlMetrics) AgentReachable(agent string, reachable bool) {
	if c == nil {
		return
	}
	v := 0.0
	if reachable {
		v = 1
	}
	c.agentReachable.WithLabelValues(agent).Set(v)
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
	// Deliberately no Go runtime/process collectors: unlabeled go_*/process_*
	// families from multiple agent registries would collide in the fan-out
	// merge; do not add collectors.NewGoCollector().
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
		Help: "LLM tokens consumed, by direction (input/output/cache_creation/cache_read).",
	}, []string{"agent", "direction"})
	a.toolCalls = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "agent_tool_calls_total",
		Help: "Tool calls dispatched by the agent loop.",
	}, []string{"agent", "tool"})
	a.reg.MustRegister(a.turns, a.turnDur, a.tokens, a.toolCalls)
	return a
}

// TurnObserved records one agent turn. outcome is the harness
// TurnResult.StopReason vocabulary ("completed", "error", "aborted",
// "continue") — an upstream contract, so no constants are defined here.
func (a *AgentMetrics) TurnObserved(outcome string, dur time.Duration, usage *llm.Usage) {
	if a == nil {
		return
	}
	a.turns.WithLabelValues(a.agentID, outcome).Inc()
	a.turnDur.WithLabelValues(a.agentID).Observe(dur.Seconds())
	if usage != nil {
		a.tokens.WithLabelValues(a.agentID, "input").Add(float64(usage.InputTokens))
		a.tokens.WithLabelValues(a.agentID, "output").Add(float64(usage.OutputTokens))
		if usage.CacheCreationInputTokens > 0 {
			a.tokens.WithLabelValues(a.agentID, "cache_creation").Add(float64(usage.CacheCreationInputTokens))
		}
		if usage.CacheReadInputTokens > 0 {
			a.tokens.WithLabelValues(a.agentID, "cache_read").Add(float64(usage.CacheReadInputTokens))
		}
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
