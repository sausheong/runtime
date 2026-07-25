# Observability

Runtime provides fleet-wide Prometheus metrics, structured request logs,
request correlation, OpenTelemetry traces, Grafana dashboards, Prometheus
rules, and Alertmanager wiring.

## Management listener

`runtimed` serves the application API on `RUNTIME_CTL_ADDR` and metrics on a
separate `RUNTIME_METRICS_ADDR`, defaulting to `127.0.0.1:9091`.

```bash
curl http://127.0.0.1:9091/metrics
```

The public control-plane listener does not mount `/metrics`. This separation is
intentional because fleet series contain operator-level agent, tenant, model,
tool, upstream, and cost labels. Bind the management listener to a private
interface, restrict it with host/network policy, and let only the monitoring
system scrape it.

The turnkey Compose stack keeps this listener internal. Prometheus scrapes it
and Grafana reads Prometheus; operators normally do not expose the listener
directly.

## How fleet collection works

Each native agent has a private Prometheus registry. On a management scrape,
the control plane:

1. collects its own registry;
2. discovers current agent replicas;
3. fetches each replica's metrics with the replica's internal credential;
4. rewrites/enforces authoritative agent labels;
5. merges valid families into one response.

An unavailable or malformed agent registry does not fail the entire fleet
scrape. `runtime_agent_up` and `runtime_metrics_scrape_skips_total` show the
gap. Runtime deliberately does not merge Go runtime/process collectors from
each replica because their unlabeled family names would collide.

Use fixed route patterns, outcomes, roles, tools, and categories as labels.
Never add session IDs, request IDs, URLs, subjects, error strings, prompts, or
other unbounded values to Prometheus labels.

## Metrics inventory

Control-plane HTTP and fleet health:

| Metric | Labels | Meaning |
|---|---|---|
| `runtime_http_requests_total` | `route,method,status` | Requests handled by the control plane |
| `runtime_http_request_duration_seconds` | `route,method` | Request latency |
| `runtime_agent_up` | `agent,replica` | Last agent-metrics scrape result |
| `runtime_agent_reachable` | `agent,replica` | Last remote health poll |
| `runtime_agent_restarts_total` | `agent,replica` | Supervisor respawns |
| `runtime_proxy_errors_total` | `agent` | Reverse-proxy failures |
| `runtime_agent_proxy_calls_total` | `agent,kind` | Proxied new-session, message, stream, and other calls |
| `runtime_metrics_scrape_skips_total` | `agent,replica,reason` | Replica omitted from fan-out |

Pools and autoscaling:

| Metric | Labels | Meaning |
|---|---|---|
| `runtime_agent_replicas_desired` | `agent` | Policy target |
| `runtime_agent_replicas_current` | `agent` | Live replicas, including draining |
| `runtime_agent_active_sessions` | `agent` | Last active-session count |
| `runtime_autoscale_events_total` | `agent,action` | Scale, drain, undrain, reap, or blocked actions |

Gateway and enforcement:

| Metric | Labels | Meaning |
|---|---|---|
| `runtime_gateway_tool_calls_total` | `server,tool,outcome` | Federated calls |
| `runtime_gateway_tool_call_duration_seconds` | `server` | Upstream latency |
| `runtime_gateway_upstream_up` | `server` | Upstream connection state |
| `runtime_gateway_policy_decisions_total` | `tenant,decision` | Allow, deny, or policy error |
| `runtime_gateway_quota_rejections_total` | `tenant,server` | Calls rejected by rate quota |
| `runtime_gateway_credential_errors_total` | `tenant,server` | Closed credential-resolution failures |

Golden-set evaluations:

| Metric | Labels | Meaning |
|---|---|---|
| `runtime_eval_runs_total` | `tenant,status` | Finalised runs |
| `runtime_eval_cases_total` | `tenant,result` | Passed and failed cases |

Agent execution:

| Metric | Labels | Meaning |
|---|---|---|
| `agent_turns_total` | `agent,outcome` | Turn completion outcome |
| `agent_turn_duration_seconds` | `agent` | Turn wall time |
| `agent_tokens_total` | `agent,tenant,model,direction` | Input, output, cache-creation, and cache-read tokens |
| `agent_cost_usd_total` | `agent,tenant,model` | Priced model cost |
| `agent_cost_unpriced_total` | `agent,tenant,model` | Turns missing a price |
| `agent_tool_calls_total` | `agent,tool` | Tool dispatches |
| `agent_session_limit_hits_total` | `agent,limit` | Lifecycle-limit terminations |

Memory and online evaluation:

| Metric | Labels | Meaning |
|---|---|---|
| `agent_memory_summary_writes_total` | `agent,tenant,model` | Rolling summary writes |
| `agent_memory_gc_deleted_total` | `agent,tenant` | Dead memory rows reaped |
| `agent_memory_episode_writes_total` | `agent,tenant` | Episodic records written |
| `agent_eval_sessions_scored_total` | `agent,tenant` | Sampled online sessions |
| `agent_eval_criteria_total` | `agent,tenant,result` | Online criteria results |
| `agent_eval_failures_total` | `agent,tenant,category` | Terminal failure taxonomy |

Counters reset when the process that owns them restarts. Prometheus provides
the long-term time series; the database remains authoritative for durable
session and evaluation records.

## Request correlation and logs

The control plane accepts or creates `X-Request-ID`, returns it to the client,
and forwards it to the selected agent. Use it to connect proxy, control-plane,
and agent logs. Request IDs are log fields, not metric labels.

`runtimectl invoke -v` prints the returned request ID. Set:

```bash
RUNTIME_LOG_FORMAT=json
```

for structured logs suitable for ingestion. Logs include stable operational
attributes such as route, status, agent, replica, and request ID where
available. Do not log bearer keys, secret values, raw authorisation headers, or
unredacted evaluation transcripts.

Health endpoints:

- `GET /healthz` confirms the process is serving;
- `GET /readyz` checks readiness dependencies;
- agent health/readiness are also used by supervision and remote monitoring.

Liveness is not readiness. Deployment probes should use the endpoint
appropriate to whether they are deciding to restart a process or send it
traffic.

## Distributed tracing

Tracing is enabled when either `RUNTIME_TRACING_ENABLED` is truthy or
`OTEL_EXPORTER_OTLP_ENDPOINT` is set.

```bash
RUNTIME_TRACING_ENABLED=1
OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4318
RUNTIME_TRACE_SAMPLE_RATIO=0.1
```

The sample ratio is between 0 and 1. Trace context is propagated over
control-plane-to-agent and supported upstream calls so one request can be
followed across the edge, agent turn, and tool invocation.

Tracing can contain sensitive operation names or attributes depending on
instrumentation. Apply access control and retention to the trace backend and
do not attach prompts, secrets, or high-cardinality session content as span
attributes.

## Turnkey monitoring stack

The Compose profile includes:

- Prometheus for collection and rule evaluation;
- Grafana with provisioned dashboards;
- Alertmanager for routing;
- OpenTelemetry Collector;
- Jaeger for trace exploration.

The quickstart binds Grafana, Prometheus, Alertmanager, and Jaeger to host
loopback. Grafana anonymous access is disabled and its administrator password
is generated into `deploy/compose/.env`.

Provisioned rules cover important availability and safety signals such as
unreachable agents, restart loops, proxy failures, scrape gaps, policy or
credential failures, quota rejection, and unpriced model use. Operators must
configure real Alertmanager receivers; an alerting service with no receiver is
only a local diagnostic surface.

## Operational checks

After deployment:

1. verify `/healthz` and `/readyz` on the application listener;
2. verify `/metrics` only on the private listener;
3. check every expected replica has `runtime_agent_up == 1`;
4. run one agent session and confirm turn/token series appear;
5. exercise a gateway tool and confirm call/upstream series;
6. confirm request IDs connect control-plane and agent logs;
7. send a test alert through the configured receiver;
8. create a trace and find it in the backend.

Dashboards and alerts are starting points. Tune thresholds to workload
baselines and revisit them when agents, replica counts, or model providers
change.
