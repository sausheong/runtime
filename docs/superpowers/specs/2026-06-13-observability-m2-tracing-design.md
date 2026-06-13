# Observability M2 — Distributed Tracing (OpenTelemetry): Design Spec

**Date:** 2026-06-13
**Status:** Approved (brainstorm complete)
**Sub-project:** B5 (Observability), Milestone 2

---

## Section 1 — Goal & Scope

**Goal:** Add OpenTelemetry distributed tracing across the
`runtimed → agentd → gateway → sandbox-call` chain, so an operator can see a
single request's full span tree (edge → proxy → turn → tool → gateway upstream)
end-to-end, joined to the existing logs and metrics by `request_id` / `trace_id`.

### In scope (Observability M2)

- A W3C-`traceparent`-propagated trace spanning the runtimed→agentd process
  boundary (the existing `X-Request-ID` seam carries it).
- Spans for: runtimed edge request, reverse-proxy hop, agentd HTTP handler, the
  DBOS session workflow, **each turn** (`RunTurn`), **each tool call**, and
  **each gateway upstream call** — onto the same boundaries obs-M1 already
  instruments for metrics. The LLM provider call inside a turn gets a span only
  if reachable without harness changes.
- OTLP/HTTP push export, **off by default** (no endpoint ⇒ no-op tracer
  provider, zero overhead); parent-based + ratio sampler when on.
- Spans carry **IDs + structural attributes only** (agent / tenant / session /
  request_id / turn / tool / gateway / outcome / tokens / HTTP route+status) —
  **no message content, no tool args/results, no secrets/prompts**.
- `internal/obs` remains the single owner of observability (the no-op gate,
  attribute discipline, and shutdown live there).
- An obs compose-overlay addition: OpenTelemetry Collector + Jaeger UI for the
  live proof.

### Out of scope (deferred)

- **sandboxd/browserd internal spans** (exec steps, browser actions, egress
  decisions) — a separate instrumentation surface (their own processes); a
  focused later milestone. M2 traces sandbox work only as the *gateway upstream
  call* span from the agent's side.
- **Truncated content/prompt/arg attributes** on spans (a PII/exfil surface) —
  an explicit off-by-default opt-in later, not the M2 default.
- Per-tenant token accounting, alerting/recording rules, log shipping,
  DBOS-internal metrics (other "Remaining B5" items).

### Framing

obs-M1 gave the fleet *aggregate* metrics + a grep-able `request_id`; M2 turns
that same correlation seed into a *structured, per-request* trace tree without
changing the aggregate posture. Foundations already present: `internal/obs` owns
all metrics with nil-safe helpers; `RequestID` middleware propagates
`X-Request-ID` edge→reverse-proxy→agentd; the OTel SDK (`otel`, `otlptracehttp`,
`otelhttp`) is already in `go.mod` (transitively, via DBOS) so no net-new heavy
dependency. DBOS v0.16.0 does **not** emit OTel spans itself (verified: no
`go.opentelemetry.io` imports in its source), so runtime owns the entire tracing
surface — no double-init.

---

## Section 2 — `internal/obs` Tracing Core

`internal/obs` stays the sole owner of observability. One new file,
`obs/tracing.go`, mirroring how the package owns metrics (nil-safe, single init
point).

### Initialization — `InitTracing`

```go
// InitTracing installs the global tracer provider + W3C propagator. When no
// OTLP endpoint is configured it installs a no-op provider (zero overhead) and
// returns a no-op shutdown — mirroring the nil-safe metrics posture. The
// returned shutdown flushes + closes the exporter; call it on process exit.
func InitTracing(ctx context.Context, serviceName string) (shutdown func(context.Context) error, err error)
```

- **Off by default.** Enabled only when `OTEL_EXPORTER_OTLP_ENDPOINT` is set, or
  `RUNTIME_TRACING_ENABLED=1` (which then uses the OTel-standard default
  endpoint). No endpoint ⇒ `noop.NewTracerProvider()`; the W3C propagator is
  still installed (so an inbound `traceparent` is honored even when we don't
  export); shutdown is a no-op. Zero new runtime cost when unused.
- **Sampler:** `sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))`, with
  `ratio` from `RUNTIME_TRACE_SAMPLE_RATIO` (default `1.0`; malformed → default
  + warn). Parent-based keeps a trace consistent across the hop — a sampled root
  pulls its whole tree.
- **Exporter:** `otlptracehttp` (already in `go.mod`), wrapped in
  `sdktrace.NewBatchSpanProcessor`.
- **Resource:** `service.name` = the passed name (`runtimed`, or the agent id
  for agentd), plus `service.version` when available — this is how Jaeger
  separates the services in the tree.
- **Propagator:** `propagation.TraceContext{}` (W3C `traceparent`/`tracestate`)
  set globally via `otel.SetTextMapPropagator`, so `otelhttp` inject/extract
  works without custom code.
- **Degrade-don't-fail:** an exporter-construction failure is returned to the
  caller, which logs a warning and continues with a no-op provider — tracing
  must never block a process from starting.

### Span helpers (thin, over the global tracer)

```go
// StartSpan starts a span on the "runtime" tracer with the given attributes.
// Safe with a no-op provider (returns a no-op span). The caller defers end().
func StartSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span)

// Span attribute builders — the ONE place the "IDs/structural only, no content"
// rule is enforced and reviewable.
func AgentAttr(id string) attribute.KeyValue      // "agent.id"
func TenantAttr(t string) attribute.KeyValue      // "tenant.id"
func SessionAttr(s string) attribute.KeyValue     // "session.id"
func RequestIDAttr(r string) attribute.KeyValue   // "request.id"  ← logs↔traces join
func TurnAttr(n int) attribute.KeyValue           // "turn.number"
func ToolAttr(name string) attribute.KeyValue     // "tool.name"
func OutcomeAttr(o string) attribute.KeyValue     // "outcome"
```

- `request.id` on the root span is the explicit logs↔traces bridge: one
  `X-Request-ID` grep finds the logs; the same value on the span finds the trace.
- Token-count attributes reuse the obs-M1 direction set
  (`tokens.input/output/cache_creation/cache_read`).
- All helpers are no-op-safe — instrumented code never checks whether tracing is
  on (same contract as the metrics helpers).

### Files

`obs/tracing.go` (init + helpers), `obs/tracing_test.go` (no-op default;
ratio/endpoint/enabled env parsing; an in-memory `tracetest.SpanRecorder`
proving `StartSpan` records the right attributes and that the "no content" set
is what is emitted; W3C propagator inject→extract round-trip).

---

## Section 3 — Cross-Process Propagation (the HTTP seams)

We use the off-the-shelf `otelhttp` (already in `go.mod`) at three seams rather
than hand-rolling `traceparent` parsing.

### runtimed outbound → agentd (the reverse-proxy hop)

- The reverse proxy already sets a custom `Transport` (`authTransport`, from
  C3). Compose it: `rp.Transport = otelhttp.NewTransport(authTransport{...})` —
  every proxied request gets a client span **and** the `traceparent` header
  injected from the current span context. The existing `X-Request-ID` forwarding
  is untouched; both ride the same request.
- The `/agents` health client and the metrics fan-out client get the same
  `otelhttp.NewTransport` wrap — cheap, keeps health/scrape hops in-trace when
  they originate under a span, and a harmless no-op when tracing is off.

### runtimed inbound (the edge)

- Wrap the root handler with `otelhttp.NewHandler(root, "runtimed")` **inside**
  `RequestID` (RequestID stays outermost — it mutates `r.Header` and seeds
  `request.id`; the otel handler then extracts any inbound `traceparent` and
  starts the server span). The matched mux pattern becomes the span name via
  `otelhttp.WithSpanNameFormatter`, reusing obs-M1's route-normalization
  discipline (never raw paths).

### agentd inbound

- Wrap agentd's `handler()` with `otelhttp.NewHandler(..., "agentd")`, placed
  just inside `RequestID` and the optional `requireBearer` — so the extracted
  `traceparent` from runtimed becomes the **parent** of agentd's server span,
  linking the two processes into one trace. Same span-name-formatter (route, not
  raw path).

### Ordering invariant (explicit)

On both processes the middleware order is: `RequestID` (outermost) → `otelhttp`
handler → (agentd only: `requireBearer`) → access-log → mux. RequestID must stay
outermost (its `r.Header` mutation contract from obs-M1); otelhttp sits directly
inside it so the server span wraps everything meaningful and `request.id` is
already in context to stamp on the span.

### Result

An inbound request to runtimed starts (or continues) a trace; the proxy hop
injects `traceparent`; agentd extracts it and its work hangs under the runtimed
span — a real two-process tree, with `request.id` on the root linking to the
existing logs.

---

## Section 4 — In-Process Spans (workflow, turn, tool, gateway)

These hang under the agentd / runtimed server spans (Section 3) and sit at the
exact boundaries obs-M1 already instruments for metrics, so spans and metrics
share call sites, names, and attributes. Every in-process span is created via
`obs.StartSpan` with `obs.*Attr` builders — no raw attribute strings scattered
across packages.

### Session workflow span (agentd)

- In `sessionWorkflow`, wrap the durable loop in a `session.workflow` span with
  `agent.id`, `tenant.id`, `session.id`, `request.id`.
- **DBOS replay caveat:** the workflow replays on recovery. The span is started
  from the *live* execution context, never on replay — a span is a
  live-execution concern, not durable state. We do **not** checkpoint span
  context into the workflow input; a recovered run's trace simply begins at
  recovery (the honest representation). This mirrors how obs-M1 kept metrics out
  of durable state.

### Turn span (agentd)

- Each `RunTurn` call gets a child `agent.turn` span with `turn.number`; on
  completion it carries the same attributes `observeTurn` already computes:
  `outcome`, token counts (`tokens.input/output/cache_creation/cache_read`).
  Duration is implicit in the span. Highest-value span — "which turn was slow,
  and how many tokens."
- **LLM provider call:** an `llm.call` child span with `model` + token attrs is
  added **only if reachable without harness changes** (the provider is called
  inside `RunTurn`). M2 is runtime-side; we do not fork harness for this. The
  turn span already captures the dominant cost.

### Tool-call span (agentd)

- Where obs-M1 calls `ToolCallObserved(tool)` (in `observeTurn`, reading session
  entries), emit a `tool.call` span per tool with `tool.name` and `outcome`.
  These are reconstructed post-turn from session entries (obs-M1's existing
  pattern), so they are recorded as completed child spans with accurate names —
  not live-wrapped around execution (which would need harness hooks). Honest and
  useful; live-wrapped tool spans are a later refinement.

### Gateway upstream span (runtimed)

- Where obs-M1 calls `GatewayCall(server, tool, outcome, dur)`, wrap the upstream
  call in a `gateway.upstream` span with `gateway.server`, `gateway.tool`,
  `outcome`. The gateway runs in-process in runtimed under the edge server span,
  so these nest naturally. When the agent reaches the gateway over MCP,
  propagation carries the trace and the gateway→upstream call is the leaf.

---

## Section 5 — Process Wiring, Export & Compose Overlay

### Process wiring (both binaries)

- `cmd/runtimed/main.go`: call
  `shutdown, err := obs.InitTracing(ctx, "runtimed")` early in `main` (after
  logging setup, before the server starts); `defer shutdown(...)` on exit
  (alongside the existing graceful-shutdown path). Exporter-init failure is
  logged as a warning and falls back to no-op — tracing must never block the
  control plane from starting (degrade-don't-fail).
- `cmd/agentd/main.go` / `agentruntime.Serve`: same, with `service.name` =
  the agent id (`cfg.Spec.ID`), so Jaeger shows each agent as its own service.
  Init before the HTTP server starts; flush on shutdown — hook the trace flush
  into agentd's existing SIGINT/SIGTERM drain, before `dbos.Shutdown`.

### Env surface (operator-facing, all optional)

- `OTEL_EXPORTER_OTLP_ENDPOINT` — standard OTel var; presence enables tracing
  (e.g. `http://otel-collector:4318`).
- `RUNTIME_TRACING_ENABLED` — explicit on/off override (disable even with an
  endpoint set, or enable with the default endpoint).
- `RUNTIME_TRACE_SAMPLE_RATIO` — `0.0`–`1.0`, default `1.0`.

These flow through the C2 Helm chart and C3 remote-agent env exactly like the
existing `RUNTIME_*` vars (operator-provisioned for remote agents, consistent
with C3 M1). The Helm `values.yaml` gains an optional `tracing:` block,
documented, off by default.

### Export topology (the obs overlay)

- `deploy/docker-compose.obs.yml` (the M1 overlay adding Prometheus + Grafana)
  gains:
  - an **OpenTelemetry Collector** (receives OTLP/HTTP on `:4318`, batches,
    exports to Jaeger) with a small `deploy/otel/collector-config.yaml`;
  - **Jaeger** (all-in-one) with its UI on **`:16686`**.
- The base compose's runtimed/agentd services get
  `OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4318` set **in the overlay
  only**, so the base stack stays trace-free unless you opt into the overlay
  (mirroring how M1 kept Prometheus opt-in).
- Backend-agnostic: swapping Jaeger for Tempo/Honeycomb is a collector-config
  change, not a code change.

### Docs

The README observability section gains a "Distributed tracing" subsection
(enable via the overlay, the env vars, the Jaeger URL, the
`trace_id`↔`request_id` join), sibling to the existing Grafana/Prometheus
section.

---

## Section 6 — Testing & Live Proof

### Hermetic unit tests (no collector; run in `go test ./...`)

| Area | Test file | Cases |
|---|---|---|
| Init posture | `internal/obs/tracing_test.go` | no endpoint ⇒ no-op provider + no-op shutdown (zero overhead); endpoint set ⇒ real provider; `RUNTIME_TRACE_SAMPLE_RATIO` parse (default 1.0; malformed → default + warn); `RUNTIME_TRACING_ENABLED` override both directions |
| Span attrs | `internal/obs/tracing_test.go` | `StartSpan` + `*Attr` builders record exactly the ID/structural set via an in-memory `tracetest.SpanRecorder`; assert **no** content/arg/prompt keys are emitted (the "no content" guard, as a test) |
| Propagator | `internal/obs/tracing_test.go` | global propagator is W3C TraceContext; inject→extract round-trips a span context |
| Edge span naming | `controlplane` (new `tracing_test.go`) | the otelhttp handler names spans by matched route, never raw path (cardinality discipline, mirroring the metrics route test) |
| Cross-process parent | `agentruntime/server_test.go` | a request carrying a `traceparent` makes agentd's server span a child of that context (extracted parent), proven with a `SpanRecorder` |

### Integration test (`//go:build integration`, Postgres.app)

`test/tracing_e2e_test.go` — run runtimed + a real agentd configured to export to
an **in-process OTLP receiver** (a tiny test collector, or the SDK in-memory
exporter wired via a test-only endpoint), then assert:

- a single `POST /agents/{id}/sessions` produces spans from **both** services
  sharing **one trace_id**, with the expected parent/child shape (runtimed edge
  → proxy → agentd handler → `session.workflow` → `agent.turn`);
- the root span carries the same `request.id` the response's `X-Request-ID`
  header returned (the logs↔traces join, proven);
- **tracing-off path:** with no endpoint, the same request emits **zero** spans
  and still succeeds (back-compat + zero overhead).

Integration tests self-clean their DB + the `dbos` schema, per workspace
convention.

### Live proof (milestone gate; mirrors M1's Prometheus+Grafana proof)

1. Bring up the stack with the obs overlay:
   `docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.obs.yml up -d`
   (now including otel-collector + Jaeger).
2. Drive a real multi-turn session through runtimed (`runtimectl invoke -v`,
   capturing the `X-Request-ID`).
3. Open Jaeger (`:16686`) and show the **end-to-end trace**: one trace with
   runtimed and agentd as distinct services, the edge→proxy→agentd-handler→
   `session.workflow`→turn(s)→tool tree; verify span attributes
   (agent/tenant/session/turn/tokens/outcome) are present and **no content/args**
   appear.
4. Prove the **logs↔traces join**: the `request.id` on the root span equals the
   `X-Request-ID` from `runtimectl invoke -v`, and grepping that id in the logs
   hits the same request (extends M1's proven correlation chain to traces).
5. Prove a **gateway span**: a gateway-enabled agent turn calling an upstream
   tool shows the `gateway.upstream` span nested in the trace.
6. Prove **off = zero**: with the base compose (no overlay, no endpoint), the
   same session produces no spans and works normally.
7. Optionally show **sampling**: `RUNTIME_TRACE_SAMPLE_RATIO=0.0` → no traces;
   `1.0` → all.

### Conventions honored

- The `go` CLI is ground truth (ignore IDE/LSP diagnostics from the
  `replace ../harness` setup).
- Integration tests use Postgres.app at
  `postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable` and
  self-clean.
- No secrets or content in spans or logs.
- Scripted model (`test/scripted`) so no LLM key is needed for the proof.

---

## Open items deferred to later Observability milestones

- **sandboxd/browserd internal spans** (own processes; second propagation path).
- **Content/prompt/arg span attributes** as an explicit off-by-default opt-in.
- **Live-wrapped tool/LLM spans** (need harness hooks) vs. M2's post-turn
  reconstruction.
- Per-tenant token accounting, alerting/recording rules, console `/ui` trace
  panel, log shipping, DBOS-internal metrics (remaining B5 backlog).
