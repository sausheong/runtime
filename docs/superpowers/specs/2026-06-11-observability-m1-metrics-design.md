# Observability M1 — Prometheus Metrics + Request Correlation

**Date:** 2026-06-11
**Status:** Approved design, pre-implementation
**Sub-project:** B5 Observability, milestone 1 (of: M1 metrics + correlation IDs; M2 candidates: OTel tracing, log shipping, alerting)
**Builds on:** Spine M3 (structured `slog` access log — the precursor this milestone completes); Gateway M1–M2 (the gateway handler instrumented here); absorbs spine-hardening item A8.

## 1. Context & purpose

Every sub-project except Observability has shipped at least one milestone; the
platform runs agents, brokers secrets, federates tools, and executes sandboxed
code — all invisible beyond one access-log line per HTTP request. B5 M1 makes
the platform *operable*: Prometheus metrics for everything that moves (agent
turns, tokens, tool calls, gateway upstreams, agent health) and a request
correlation ID that stitches runtimed and agentd logs together. A compose
overlay ships Prometheus + Grafana with a provisioned dashboard so
`docker compose up` produces a working ops view.

## 2. Decisions (settled during brainstorming)

| Decision | Choice | Rationale |
|---|---|---|
| Consumer model | Prometheus pull + Grafana | De-facto on-prem standard; `/metrics` is curl-able with zero infrastructure; Grafana dashboards come free |
| Instrumentation library | `prometheus/client_golang` directly | Smallest path to idiomatic exposition; OTel's Prometheus bridge adds ceremony and renames metrics for zero benefit while Prometheus is the only consumer. M2 tracing can add OTel alongside (metrics-via-Prometheus + traces-via-OTel is the common Go production pairing) |
| Agent metric delivery | runtimed fans out to per-agent `/metrics` and merges | One scrape target; matches supervisor/proxy topology; agent ports stay private-to-runtimed; foreign shims without `/metrics` skipped (degrade-don't-fail) |
| Correlation | `X-Request-ID` in M1 | Generated/accepted at the edge, propagated through the reverse proxy, logged both sides, echoed in the response. The seed for M2 tracing, not a span |
| Scope | Control plane + agent turns + gateway | Sandboxd internals deferred — its calls already surface as gateway tool-call series; per-container detail belongs to Sandboxes M2 |
| Dashboard deliverable | Compose overlay + provisioned Grafana dashboard | The milestone's demo-able proof; `/metrics`-plus-README was judged too thin |

## 3. Architecture

```
Prometheus ──scrape──▶ runtimed GET /metrics
                            │  own registry: http, proxy, agent up/restarts, gateway
                            │  + fan-out scrape (500ms/agent timeout)
                            ▼
              agentd GET /metrics  (per supervised agent)
                  own registry: turns, tokens, tool calls — labeled agent=<id>
                            │
                  merged by expfmt parse → family-merge → re-encode
                            ▼
              one valid exposition, all series

X-Request-ID: edge middleware (accept or generate req-<128-bit hex>)
  → slog attr on runtimed access log
  → response header (echoed)
  → reverse proxy forwards header → agentd middleware → slog attr agent-side
```

### 3.1 New package `internal/obs`

Owns every metric definition and both middlewares. **Nothing else imports
`prometheus/client_golang`** — callers use typed helpers:

- `obs.HTTPObserved(route, method, status, dur)` — called from the access-log middleware.
- `obs.AgentUp(agent string, up bool)` — called from the fan-out scrape per §3.4 (reachability is the truth); `obs.AgentRestart(agent string)` — called from the supervisor respawn path.
- `obs.ProxyError(agent string)` — called from the reverse-proxy ErrorHandler.
- `obs.GatewayCall(server, tool, outcome string, dur time.Duration)` — called from the gateway tool handler.
- `obs.GatewayUpstreamUp(server string, up bool)` — called from the gateway manager's connection state transitions.
- `obs.TurnObserved(agent, outcome string, dur time.Duration, usage *llm.Usage)` — called from agentd's turn path; nil-safe on usage.
- `obs.ToolCallObserved(agent, tool string)` — called from agentd's tool dispatch.
- `obs.Handler()` — the `promhttp` handler for the calling binary's registry.
- `obs.RequestID(next http.Handler) http.Handler` — edge middleware (accept inbound `X-Request-ID`, else generate; set response header; stash in context).
- `obs.RequestIDFromContext(ctx) string` — for log enrichment.

Two registries, one per binary: agentd and runtimed each create their own
(`obs.NewAgentMetrics(agentID)` / `obs.NewControlMetrics()`), so tests get
isolated registries and the package stays free of global state beyond the
default helpers each binary wires at startup.

### 3.2 Metrics inventory (complete for M1)

Control plane (runtimed registry):

| Metric | Type | Labels | Source |
|---|---|---|---|
| `runtime_http_requests_total` | counter | `route`, `method`, `status` | access-log middleware |
| `runtime_http_request_duration_seconds` | histogram | `route`, `method` | access-log middleware |
| `runtime_agent_up` | gauge | `agent` | fan-out scrape result (§3.4 — reachability is the truth) |
| `runtime_agent_restarts_total` | counter | `agent` | supervisor respawn |
| `runtime_proxy_errors_total` | counter | `agent` | reverse-proxy ErrorHandler (503s) |
| `runtime_gateway_tool_calls_total` | counter | `server`, `tool`, `outcome` | gateway tool handler |
| `runtime_gateway_tool_call_duration_seconds` | histogram | `server` | gateway tool handler |
| `runtime_gateway_upstream_up` | gauge | `server` | gateway manager state |
| `runtime_metrics_scrape_skips_total` | counter | `agent`, `reason` | fan-out merge (timeout/parse/404) |

Agent (agentd registry; every series carries `agent=<RUNTIME_AGENT_ID>`):

| Metric | Type | Labels | Source |
|---|---|---|---|
| `agent_turns_total` | counter | `agent`, `outcome` | turn step completion (`completed`/`error`/`aborted`/`continue`) |
| `agent_turn_duration_seconds` | histogram | `agent` | turn step wall time |
| `agent_tokens_total` | counter | `agent`, `direction` (`input`/`output`) | `TurnResult.Usage` (already plumbed; nil ⇒ no increment) |
| `agent_tool_calls_total` | counter | `agent`, `tool` | tool dispatch in the turn path |

`outcome` for gateway calls: `ok` | `error` (MCP isError or transport failure).
Histogram buckets: HTTP default buckets for `runtime_http_*`; turn/gateway
durations use `[]float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120}` (LLM
turns are seconds-to-minutes; default buckets top out at 10s).

### 3.3 Cardinality rules (hard constraints)

- NO per-session, per-tenant, or per-argument labels anywhere in M1.
- `route` is the normalized mux pattern (`/agents/{id}/sessions`), never the
  raw path. The access-log middleware derives it from the matched
  `http.Request.Pattern` (Go 1.22+ `ServeMux` populates it); unmatched
  requests get `route="unmatched"`.
- `tool` on `agent_tool_calls_total` is bounded by the agent's registry size;
  gateway `tool` is bounded by the federated catalog. Both are operator-config
  scale (tens to low hundreds), accepted.
- Label values are operator-level identifiers (agent ids, upstream names, tool
  names) — never user data. This is why `/metrics` can be auth-free (§5).

### 3.4 The fan-out merge (runtimed `/metrics`)

Naive text concatenation of per-agent expositions produces duplicate
`# TYPE`/`# HELP` blocks for the same family, which Prometheus rejects. The
handler therefore:

1. Gathers its own registry (the `runtime_*` families).
2. For each supervised agent (from the registry, same source the proxy uses):
   `GET http://<addr>/metrics` with a **500ms per-agent timeout**, requests
   running **concurrently**.
3. Parses each response with `github.com/prometheus/common/expfmt`
   (`TextParser.TextToMetricFamilies`).
4. Merges families by name across agents (series are disjoint because every
   agent series carries its own `agent=` label; a duplicate-series collision —
   misconfigured duplicate agent id — is impossible because config validation
   already rejects duplicate ids).
5. Re-encodes one valid text exposition (`expfmt.MetricFamilyToText`).

Skip rules: timeout, non-200, or parse failure ⇒ that agent's families are
omitted this scrape, `runtime_metrics_scrape_skips_total{agent,reason}`
increments, and one WARN line is logged. `runtime_agent_up{agent}` reflects
the fan-out result (1 = scraped clean, 0 = skipped), making it the single
"is the agent alive" series — sourced from actual reachability, not just
supervisor bookkeeping. A foreign shim returning 404 is reason `no_metrics`
and logged at DEBUG (expected, contract makes `/metrics` optional), and does
NOT zero `runtime_agent_up` — a 404 proves the process is serving; only
timeout/connection-refused/parse failures zero it.

### 3.5 Request correlation

- `obs.RequestID` wraps the runtimed handler chain OUTSIDE `accessLog` so the
  access log can read the id from context: each access-log line gains
  `request_id=<id>`.
- The id is set as a response header (`X-Request-ID`) for client-side
  correlation, and rides the existing reverse proxy (headers pass through
  untouched) to agentd.
- agentd wraps its mux with the same middleware (inbound id always present
  when called via runtimed; direct calls get a fresh id), and its log lines
  gain `request_id=<id>`.
- agentd additionally stamps the id into the turn-step slog context so
  turn-level lines (DBOS step start/finish, turn outcome) carry it — one grep
  spans edge → proxy → agent → turn.
- `runtimectl invoke -v` prints the response's `X-Request-ID` so an operator
  can capture it at the terminal.

## 4. Wiring points (exhaustive)

| Site | Change |
|---|---|
| `cmd/runtimed/main.go` | mount `GET /metrics` (fan-out handler, outside identity — see §5); wrap handler chain with `obs.RequestID`; access log gains `request_id` attr + calls `obs.HTTPObserved` |
| `controlplane` supervisor | `obs.AgentRestart` on respawn; proxy ErrorHandler calls `obs.ProxyError` |
| `internal/gateway/server.go` | tool handler times the upstream call, calls `obs.GatewayCall` |
| `internal/gateway/manager.go` | connection state transitions call `obs.GatewayUpstreamUp` |
| `agentruntime/server.go` | mount `GET /metrics`; wrap mux with `obs.RequestID` |
| `agentruntime/turnstep.go` (or its caller) | time the turn, call `obs.TurnObserved` with outcome + usage; tool dispatch calls `obs.ToolCallObserved` |
| `deploy/docker-compose.obs.yml` | NEW overlay: Prometheus (scrape runtimed only) + Grafana (provisioned datasource + dashboard) |
| `deploy/grafana/dashboard-runtime.json` | NEW: rows = agent health, turns (rate/latency/outcome), tokens, gateway, HTTP |
| `deploy/prometheus.yml` | NEW: one job, `runtimed:8080/metrics`, 15s interval |

No harness changes: `TurnResult.Usage` already exposes token counts.

## 5. Security posture

`GET /metrics` on runtimed is served OUTSIDE the identity middleware, like
`/healthz` — standard Prometheus practice. Justification, recorded as an
explicit decision: every label value is an operator-level identifier (agent
id, upstream name, tool name, route pattern); no tenant data, session
content, or user identifiers appear in any series (enforced by the §3.3
cardinality rules — adding a tenant/session label is a spec change, not a
tweak). Operators who need to restrict it do so at the network layer (the
compose overlay keeps Prometheus on the internal network). Agent `/metrics`
endpoints sit on agent ports, which remain private-to-runtimed as today.

## 6. Failure posture

| Failure | Behavior |
|---|---|
| Agent `/metrics` slow/down | Skipped at 500ms; scrape returns 200 with everything else; `runtime_agent_up`=0; skip counter increments |
| Agent returns malformed exposition | WARN once per scrape, agent skipped, merged output stays valid |
| Foreign shim without `/metrics` (404) | Reason `no_metrics`, DEBUG log, not an error; `runtime_agent_up` unaffected (404 = serving); control-plane series about the agent unaffected; contract documents `/metrics` as OPTIONAL |
| All agents down | `/metrics` still 200 with control-plane families; all `runtime_agent_up`=0 |
| Prometheus/Grafana absent | Nothing changes — `/metrics` is pull-only; the overlay is optional |
| Scrape concurrent with agent restart | Connection refused ⇒ skip rules apply; next scrape self-heals |

## 7. Testing & done criteria

**Hermetic unit tests:**

- `internal/obs`: each helper increments the expected family/labels (use a
  fresh registry per test + `testutil.CollectAndCompare`); nil-usage turn
  observed without token increment; histogram bucket config asserted;
  RequestID middleware — inbound honored, absent generated (`req-` prefix,
  uniqueness), response header echoed, context carries it.
- Fan-out merge: two fake agent servers (one healthy, one `time.Sleep`
  hanging) ⇒ merged exposition parses cleanly, healthy agent's families
  present, hung agent skipped with reason `timeout`, `runtime_agent_up`
  correct for both; malformed-exposition fake ⇒ skipped, output valid;
  404 fake ⇒ reason `no_metrics`, up stays 1.
- agentd: `/metrics` served alongside existing routes; turn path increments
  turns/tokens/duration with `agent=` label.
- runtimed: route normalization — two different `/agents/{id}` raw paths
  produce ONE series; unmatched path produces `route="unmatched"`.
- Gateway: tool call increments `runtime_gateway_tool_calls_total` with
  correct outcome on both success and isError paths.

**Through-serve e2e** (`test/observability_e2e_test.go`): runtimed + two test
agents + a fake gateway upstream; run turns via the control plane; assert
`/metrics` shows `agent_turns_total` rising with correct labels for both
agents, `runtime_agent_up` 1/1, a gateway call counted, and the same
`X-Request-ID` appears in the invoke response header — the gateway-call
criterion (a gateway call appearing in `runtime_gateway_tool_calls_total`
against a fake upstream) is implemented as unit coverage in
`internal/gateway/metrics_test.go` rather than in the e2e (deviation recorded
at planning; the e2e stays gateway-free to keep boot scope small).

**Live proof (manual, recorded in the ROADMAP entry):**

1. `docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.obs.yml up`
   → Grafana dashboard renders; nutrition agent turns move the latency and
   token panels.
2. `kill -9` an agentd ⇒ `runtime_agent_up` drops, restart counter
   increments, dashboard shows the blip, supervisor recovers it.
3. One `runtimectl invoke`'s request id grepped across runtimed and agentd
   log streams — both sides found.

Done = the above merged to master + ROADMAP/README updates.

## 8. Out of scope (M2+ candidates, recorded)

- OTel spans / OTLP push (request IDs are the seed; tracing is its own milestone).
- sandboxd-internal metrics (active sandboxes per tenant, exec durations,
  reaper activity) — visible today as gateway series; revisit with Sandboxes M2.
- Per-tenant token accounting / billing.
- Alerting rules, recording rules.
- Console `/ui` metrics panel.
- Log shipping (Loki/promtail) — slog already structures everything.
- DBOS-internal metrics (it has its own OTel hooks; not surfaced in M1).

## 9. Risks & mitigations

- **Cardinality creep** — the §3.3 rules are spec constraints; reviews reject
  new labels outside them. The biggest risk (raw HTTP paths) is eliminated by
  pattern-based routes with an `unmatched` bucket.
- **Fan-out scrape latency** — concurrent sub-scrapes with a 500ms cap bound
  the handler at ~500ms worst case regardless of agent count; Prometheus
  default timeout (10s) is never approached.
- **expfmt API drift** — `prometheus/common` is the same org as the client
  library and versioned; the merge touches only TextParser + MetricFamilyToText.
- **Turn-path overhead** — one histogram observe + two counter adds per turn;
  nanoseconds against multi-second LLM turns.
- **Foreign shims and request IDs** — shims log their own formats; M1
  guarantees the header REACHES them (proxy passthrough) but does not require
  they log it. Documented as a contract recommendation, not a requirement.
