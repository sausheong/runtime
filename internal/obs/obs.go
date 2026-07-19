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
	"github.com/sausheong/runtime/internal/config"
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
	reg             *prometheus.Registry
	httpRequests    *prometheus.CounterVec
	httpDuration    *prometheus.HistogramVec
	agentUp         *prometheus.GaugeVec
	agentReachable  *prometheus.GaugeVec
	agentRestarts   *prometheus.CounterVec
	proxyErrors     *prometheus.CounterVec
	agentProxyCalls *prometheus.CounterVec
	gwCalls         *prometheus.CounterVec
	gwDuration      *prometheus.HistogramVec
	gwUp            *prometheus.GaugeVec
	scrapeSkips     *prometheus.CounterVec
	asDesired       *prometheus.GaugeVec
	asCurrent       *prometheus.GaugeVec
	asActive        *prometheus.GaugeVec
	asEvents        *prometheus.CounterVec
	policyDecisions *prometheus.CounterVec
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
	}, []string{"agent", "replica"})
	c.agentReachable = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "runtime_agent_reachable",
		Help: "1 when a remote agent's /healthz was reachable on the last monitor poll (remote agents only).",
	}, []string{"agent", "replica"})
	c.agentRestarts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "runtime_agent_restarts_total",
		Help: "Supervisor respawns per agent.",
	}, []string{"agent", "replica"})
	c.proxyErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "runtime_proxy_errors_total",
		Help: "Reverse-proxy failures (503s served) per agent.",
	}, []string{"agent"})
	c.agentProxyCalls = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "runtime_agent_proxy_calls_total",
		Help: "Requests the control plane proxied to an agent, by agent and kind (new_session/message/stream/other).",
	}, []string{"agent", "kind"})
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
	}, []string{"agent", "replica", "reason"})
	c.asDesired = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "runtime_agent_replicas_desired",
		Help: "Replica count the autoscaler wants for the agent (clamped to [min,max]).",
	}, []string{"agent"})
	c.asCurrent = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "runtime_agent_replicas_current",
		Help: "Live replica count for the agent (draining replicas included).",
	}, []string{"agent"})
	c.asActive = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "runtime_agent_active_sessions",
		Help: "Non-terminal session count for the agent on the last autoscale tick.",
	}, []string{"agent"})
	c.asEvents = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "runtime_autoscale_events_total",
		Help: "Autoscale actions by agent and action (up/down/undrain/reap/blocked).",
	}, []string{"agent", "action"})
	c.policyDecisions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "runtime_gateway_policy_decisions_total",
		Help: "Gateway policy decisions, by tenant and decision (allow/deny/error).",
	}, []string{"tenant", "decision"})
	c.reg.MustRegister(c.httpRequests, c.httpDuration, c.agentUp, c.agentReachable, c.agentRestarts,
		c.proxyErrors, c.agentProxyCalls, c.gwCalls, c.gwDuration, c.gwUp, c.scrapeSkips,
		c.asDesired, c.asCurrent, c.asActive, c.asEvents, c.policyDecisions)
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

func (c *ControlMetrics) AgentUp(agent string, replica int, up bool) {
	if c == nil {
		return
	}
	v := 0.0
	if up {
		v = 1
	}
	c.agentUp.WithLabelValues(agent, strconv.Itoa(replica)).Set(v)
}

// AgentReachable sets the remote-agent reachability gauge (1/0) on each
// HealthMonitor transition. Nil-safe like the other helpers.
func (c *ControlMetrics) AgentReachable(agent string, replica int, reachable bool) {
	if c == nil {
		return
	}
	v := 0.0
	if reachable {
		v = 1
	}
	c.agentReachable.WithLabelValues(agent, strconv.Itoa(replica)).Set(v)
}

func (c *ControlMetrics) AgentRestart(agent string, replica int) {
	if c == nil {
		return
	}
	c.agentRestarts.WithLabelValues(agent, strconv.Itoa(replica)).Inc()
}

func (c *ControlMetrics) ProxyError(agent string) {
	if c == nil {
		return
	}
	c.proxyErrors.WithLabelValues(agent).Inc()
}

// Proxy-call kind label values for ProxyCall. Classified from the (already
// prefix-stripped) request method+path in the /agents/{id}/ handler.
const (
	ProxyNewSession = "new_session" // POST /sessions
	ProxyMessage    = "message"     // POST /sessions/{sid}/messages
	ProxyStream     = "stream"      // GET  /sessions/{sid}/stream
	ProxyOther      = "other"       // healthz, list, get, meta, etc.
)

// ProxyCall counts one request the control plane proxied to an agent, broken
// down by agent id and kind. Complements runtime_http_requests_total, whose
// route label (the normalized mux pattern) cannot distinguish individual agents.
func (c *ControlMetrics) ProxyCall(agent, kind string) {
	if c == nil {
		return
	}
	c.agentProxyCalls.WithLabelValues(agent, kind).Inc()
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

func (c *ControlMetrics) ScrapeSkip(agent string, replica int, reason string) {
	if c == nil {
		return
	}
	c.scrapeSkips.WithLabelValues(agent, strconv.Itoa(replica), reason).Inc()
}

// Autoscale action label values for AutoscaleEvent.
const (
	AutoscaleUp      = "up"
	AutoscaleDown    = "down"
	AutoscaleUndrain = "undrain"
	AutoscaleReap    = "reap"
	AutoscaleBlocked = "blocked"
)

func (c *ControlMetrics) AutoscaleDesired(agent string, n int) {
	if c == nil {
		return
	}
	c.asDesired.WithLabelValues(agent).Set(float64(n))
}

func (c *ControlMetrics) AutoscaleCurrent(agent string, n int) {
	if c == nil {
		return
	}
	c.asCurrent.WithLabelValues(agent).Set(float64(n))
}

func (c *ControlMetrics) AutoscaleActive(agent string, n int) {
	if c == nil {
		return
	}
	c.asActive.WithLabelValues(agent).Set(float64(n))
}

func (c *ControlMetrics) AutoscaleEvent(agent, action string) {
	if c == nil {
		return
	}
	c.asEvents.WithLabelValues(agent, action).Inc()
}

// PolicyDecision records one gateway policy evaluation outcome by tenant and
// decision (allow/deny/error).
func (c *ControlMetrics) PolicyDecision(tenant, decision string) {
	if c == nil {
		return
	}
	c.policyDecisions.WithLabelValues(tenant, decision).Inc()
}

// AgentMetrics is agentd's registry. Every series carries agent=<id> so the
// fan-out merge produces disjoint series across agents.
type AgentMetrics struct {
	agentID   string
	tenant    string
	model     string
	reg       *prometheus.Registry
	turns     *prometheus.CounterVec
	turnDur   *prometheus.HistogramVec
	tokens    *prometheus.CounterVec
	cost      *prometheus.CounterVec
	unpriced  *prometheus.CounterVec
	toolCalls *prometheus.CounterVec
	limitHits *prometheus.CounterVec
}

func NewAgentMetrics(agentID, tenant, model string) *AgentMetrics {
	// Deliberately no Go runtime/process collectors: unlabeled go_*/process_*
	// families from multiple agent registries would collide in the fan-out
	// merge; do not add collectors.NewGoCollector().
	a := &AgentMetrics{agentID: agentID, tenant: tenant, model: model, reg: prometheus.NewRegistry()}
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
	}, []string{"agent", "tenant", "model", "direction"})
	a.cost = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "agent_cost_usd_total",
		Help: "LLM dollar cost (tokens x per-model price, cache included).",
	}, []string{"agent", "tenant", "model"})
	a.unpriced = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "agent_cost_unpriced_total",
		Help: "Turns whose model had no price entry (cost blind spot).",
	}, []string{"agent", "tenant", "model"})
	a.toolCalls = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "agent_tool_calls_total",
		Help: "Tool calls dispatched by the agent loop.",
	}, []string{"agent", "tool"})
	a.limitHits = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "agent_session_limit_hits_total",
		Help: "Sessions terminated by a lifecycle limit, by limit name.",
	}, []string{"agent", "limit"})
	a.reg.MustRegister(a.turns, a.turnDur, a.tokens, a.cost, a.unpriced, a.toolCalls, a.limitHits)
	return a
}

// TurnObserved records one agent turn. outcome is the harness
// TurnResult.StopReason vocabulary ("completed", "error", "aborted",
// "continue") — an upstream contract, so no constants are defined here.
// price is this agent's per-model price, or nil when the model is unpriced.
// Tokens are emitted UNCONDITIONALLY before any pricing branch, so a pricing
// gap can never suppress token metering.
func (a *AgentMetrics) TurnObserved(outcome string, dur time.Duration, usage *llm.Usage, price *config.ModelPrice) {
	if a == nil {
		return
	}
	a.turns.WithLabelValues(a.agentID, outcome).Inc()
	a.turnDur.WithLabelValues(a.agentID).Observe(dur.Seconds())
	if usage == nil {
		return
	}
	a.tokens.WithLabelValues(a.agentID, a.tenant, a.model, "input").Add(float64(usage.InputTokens))
	a.tokens.WithLabelValues(a.agentID, a.tenant, a.model, "output").Add(float64(usage.OutputTokens))
	if usage.CacheCreationInputTokens > 0 {
		a.tokens.WithLabelValues(a.agentID, a.tenant, a.model, "cache_creation").Add(float64(usage.CacheCreationInputTokens))
	}
	if usage.CacheReadInputTokens > 0 {
		a.tokens.WithLabelValues(a.agentID, a.tenant, a.model, "cache_read").Add(float64(usage.CacheReadInputTokens))
	}
	if price == nil {
		a.unpriced.WithLabelValues(a.agentID, a.tenant, a.model).Inc()
		return
	}
	a.cost.WithLabelValues(a.agentID, a.tenant, a.model).Add(price.Cost(usage))
}

func (a *AgentMetrics) ToolCallObserved(tool string) {
	if a == nil {
		return
	}
	a.toolCalls.WithLabelValues(a.agentID, tool).Inc()
}

// LimitHitObserved records a session terminated by a lifecycle limit
// (turn_timeout | session_timeout | max_turns | max_tokens).
func (a *AgentMetrics) LimitHitObserved(limit string) {
	if a == nil {
		return
	}
	a.limitHits.WithLabelValues(a.agentID, limit).Inc()
}

// Handler serves this registry's exposition (agentd mounts it at /metrics).
func (a *AgentMetrics) Handler() http.Handler {
	if a == nil {
		return http.NotFoundHandler()
	}
	return promhttp.HandlerFor(a.reg, promhttp.HandlerOpts{})
}
