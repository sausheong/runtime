# Observability M2 — Distributed Tracing (OpenTelemetry) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add OpenTelemetry distributed tracing across the runtimed→agentd→gateway call chain — an end-to-end span tree per request, off by default, joined to existing logs/metrics by `request.id`/`trace_id`.

**Architecture:** `internal/obs` gains a `tracing.go` (single init point + no-op gate + attribute builders, mirroring how it owns metrics). Cross-process propagation uses the off-the-shelf `otelhttp` at three HTTP seams (runtimed edge, runtimed→agentd proxy transport, agentd inbound) carrying W3C `traceparent`. In-process spans (session workflow, turn, tool, gateway upstream) are placed at the boundaries obs-M1 already instruments for metrics, via `obs.StartSpan` + `obs.*Attr` builders.

**Tech Stack:** Go 1.25, OpenTelemetry Go SDK (`go.opentelemetry.io/otel` v1.44.0, `otelhttp` v0.68.0, `otlptracehttp` v1.37.0 — all already in go.mod transitively via DBOS), Postgres (integration tests), Docker Compose + OTel Collector + Jaeger.

**Spec:** `docs/superpowers/specs/2026-06-13-observability-m2-tracing-design.md`

**Conventions (read before starting):**
- The `go` CLI is ground truth; ignore IDE/LSP diagnostics (the `replace github.com/sausheong/harness => ../harness` cross-module setup confuses them). Trust `go build ./...` / `go test ./...`.
- Hermetic unit tests run with `go test ./...`. Integration tests use `//go:build integration` and need Postgres at `postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable` (local Postgres.app). They self-clean their DB + the `dbos` schema.
- Run `go build ./...`, `go vet ./...`, and `gofmt -l <changed .go files>` (must be empty) before each commit.
- Commit after each task with the message shown in its final step.
- OTel module versions to use (already resolved in go.mod, promote indirect→direct): `go.opentelemetry.io/otel v1.44.0`, `.../otel/sdk v1.44.0`, `.../otel/trace v1.44.0`, `.../otel/exporters/otlp/otlptrace/otlptracehttp v1.37.0`, `.../contrib/instrumentation/net/http/otelhttp v0.68.0`. Do NOT bump versions; use `go mod tidy` to promote.

---

## File Structure

**Created:**
- `internal/obs/tracing.go` — `InitTracing` (no-op gate, sampler, exporter, propagator) + `StartSpan` + attribute builders. The single owner of tracing.
- `internal/obs/tracing_test.go` — hermetic tracing-core tests (no-op default, env parse, attr recording, propagator round-trip).
- `controlplane/tracing_test.go` — edge span-name-by-route test.
- `deploy/otel/collector-config.yaml` — OTel Collector config (OTLP in → Jaeger out).
- `test/tracing_e2e_test.go` — `//go:build integration` end-to-end two-process trace test.

**Modified:**
- `controlplane/proxy.go` — wrap the reverse-proxy `Transport` with `otelhttp.NewTransport`.
- `controlplane/api.go` — wrap the `/agents` health client + (lightly) the fan-out client transports; nothing else.
- `cmd/runtimed/main.go` — `obs.InitTracing("runtimed")` + shutdown; wrap root handler with `otelhttp.NewHandler` inside `RequestID`.
- `agentruntime/server.go` — wrap `handler()` with `otelhttp.NewHandler` inside `RequestID`/`requireBearer`.
- `agentruntime/serve.go` — `obs.InitTracing(agentID)` + flush in the shutdown drain; `session.workflow` + `agent.turn` + `tool.call` spans in `sessionWorkflow`/`observeTurn`.
- `internal/gateway/server.go` — `gateway.upstream` span around the upstream `t.Execute`.
- `internal/obs/fanout.go` — (only if needed) wrap the fan-out client transport.
- `deploy/docker-compose.obs.yml` — add otel-collector + jaeger services; set `OTEL_EXPORTER_OTLP_ENDPOINT` on runtimed/agentd in the overlay.
- `README.md`, `ROADMAP.md` — tracing docs + M2-done entry.

---

## Task 1: Tracing core — `InitTracing` + no-op gate

**Files:**
- Create: `internal/obs/tracing.go`
- Test: `internal/obs/tracing_test.go`

- [ ] **Step 1: Promote the OTel deps to direct**

Run: `go get go.opentelemetry.io/otel@v1.44.0 go.opentelemetry.io/otel/sdk@v1.44.0 go.opentelemetry.io/otel/trace@v1.44.0 go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp@v1.37.0 go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp@v0.68.0`
Then: `go mod tidy`
Expected: go.mod lists these as direct (no `// indirect`). No version changes beyond promotion.

- [ ] **Step 2: Write the failing test**

Create `internal/obs/tracing_test.go`:

```go
package obs

import (
	"context"
	"testing"
)

func TestInitTracing_NoEndpointIsNoop(t *testing.T) {
	// No OTEL endpoint, no RUNTIME_TRACING_ENABLED ⇒ no-op provider, no error,
	// shutdown is a safe no-op.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("RUNTIME_TRACING_ENABLED", "")
	shutdown, err := InitTracing(context.Background(), "test-svc")
	if err != nil {
		t.Fatalf("InitTracing (off): %v", err)
	}
	if shutdown == nil {
		t.Fatal("shutdown must be non-nil even when off")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("noop shutdown: %v", err)
	}
	// TracingEnabled reports false when off.
	if TracingEnabled() {
		t.Fatal("TracingEnabled() = true with no endpoint")
	}
}

func TestInitTracing_EnabledByFlagWithoutEndpoint(t *testing.T) {
	// RUNTIME_TRACING_ENABLED=1 with no explicit endpoint ⇒ enabled using the
	// OTel default endpoint; must not error at init (exporter is lazy/batched).
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("RUNTIME_TRACING_ENABLED", "1")
	shutdown, err := InitTracing(context.Background(), "test-svc")
	if err != nil {
		t.Fatalf("InitTracing (on by flag): %v", err)
	}
	defer shutdown(context.Background())
	if !TracingEnabled() {
		t.Fatal("TracingEnabled() = false despite RUNTIME_TRACING_ENABLED=1")
	}
}

func TestSampleRatio(t *testing.T) {
	cases := map[string]float64{"": 1.0, "0.0": 0.0, "0.25": 0.25, "1": 1.0, "bogus": 1.0}
	for in, want := range cases {
		t.Setenv("RUNTIME_TRACE_SAMPLE_RATIO", in)
		if got := sampleRatio(); got != want {
			t.Fatalf("sampleRatio(%q) = %v, want %v", in, got, want)
		}
	}
}
```

- [ ] **Step 3: Run to verify it fails**

Run: `go test ./internal/obs/ -run 'InitTracing|SampleRatio' -v`
Expected: compile failure — `InitTracing`, `TracingEnabled`, `sampleRatio` undefined.

- [ ] **Step 4: Implement `internal/obs/tracing.go`**

```go
package obs

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"sync/atomic"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// tracingOn is set true when InitTracing installs a real exporting provider.
var tracingOn atomic.Bool

// TracingEnabled reports whether a real (exporting) tracer provider is active.
func TracingEnabled() bool { return tracingOn.Load() }

// tracer is the module's single tracer; resolved lazily from the global
// provider so it honors whatever InitTracing installed (real or no-op).
func tracer() trace.Tracer { return otel.Tracer("github.com/sausheong/runtime") }

// tracingActive reports whether tracing should be turned on: an explicit
// RUNTIME_TRACING_ENABLED=1, or any OTEL_EXPORTER_OTLP_ENDPOINT set. An
// explicit RUNTIME_TRACING_ENABLED=0 forces off even if an endpoint is set.
func tracingActive() bool {
	switch os.Getenv("RUNTIME_TRACING_ENABLED") {
	case "1", "true":
		return true
	case "0", "false":
		return false
	}
	return os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != ""
}

// sampleRatio reads RUNTIME_TRACE_SAMPLE_RATIO (default 1.0; malformed → 1.0 + warn).
func sampleRatio() float64 {
	v := os.Getenv("RUNTIME_TRACE_SAMPLE_RATIO")
	if v == "" {
		return 1.0
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 || f > 1 {
		slog.Warn("ignoring malformed RUNTIME_TRACE_SAMPLE_RATIO", "value", v, "default", 1.0)
		return 1.0
	}
	return f
}

// InitTracing installs the global tracer provider + W3C propagator. When tracing
// is not active it installs NOTHING but the propagator (so an inbound traceparent
// is still honored) and returns a no-op shutdown — zero overhead, mirroring the
// nil-safe metrics posture. The returned shutdown flushes + closes the exporter.
func InitTracing(ctx context.Context, serviceName string) (func(context.Context) error, error) {
	// Always install the W3C propagator so inbound traceparent is honored.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	if !tracingActive() {
		tracingOn.Store(false)
		return func(context.Context) error { return nil }, nil
	}
	// otlptracehttp reads OTEL_EXPORTER_OTLP_ENDPOINT (and the standard OTEL_*
	// env) itself; no explicit endpoint needed here.
	exp, err := otlptracehttp.New(ctx)
	if err != nil {
		// Degrade-don't-fail: caller logs + continues with no-op.
		return func(context.Context) error { return nil }, err
	}
	res, _ := resource.New(ctx, resource.WithAttributes(
		semconv.ServiceName(serviceName),
	))
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(sampleRatio()))),
	)
	otel.SetTracerProvider(tp)
	tracingOn.Store(true)
	return tp.Shutdown, nil
}

// StartSpan starts a span on the module tracer with the given attributes. Safe
// with a no-op provider (returns a no-op span). Caller defers span.End().
func StartSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return tracer().Start(ctx, name, trace.WithAttributes(attrs...))
}

// Span attribute builders — the ONE place the "IDs/structural only, no content"
// rule lives. NO message content, tool args/results, prompts, or secrets here.
func AgentAttr(id string) attribute.KeyValue     { return attribute.String("agent.id", id) }
func TenantAttr(t string) attribute.KeyValue     { return attribute.String("tenant.id", t) }
func SessionAttr(s string) attribute.KeyValue    { return attribute.String("session.id", s) }
func RequestIDAttr(r string) attribute.KeyValue  { return attribute.String("request.id", r) }
func TurnAttr(n int) attribute.KeyValue          { return attribute.Int("turn.number", n) }
func ToolAttr(name string) attribute.KeyValue    { return attribute.String("tool.name", name) }
func OutcomeAttr(o string) attribute.KeyValue    { return attribute.String("outcome", o) }
func GatewayServerAttr(s string) attribute.KeyValue { return attribute.String("gateway.server", s) }
func GatewayToolAttr(t string) attribute.KeyValue   { return attribute.String("gateway.tool", t) }
```

NOTE: if `semconv/v1.26.0` is not the version vendored, run `ls $(go env GOMODCACHE)/go.opentelemetry.io/otel@v1.44.0/semconv/` and pick the highest `v1.*` directory present; adjust the import path accordingly. The only symbol used is `semconv.ServiceName`.

- [ ] **Step 5: Run to verify pass**

Run: `go test ./internal/obs/ -run 'InitTracing|SampleRatio' -v`
Expected: PASS. Then `go build ./... && go vet ./...`.

IMPORTANT: `InitTracing` mutates global OTel state. The two enabled/disabled tests must use `t.Setenv` (they do) and run in the same package; if they interfere, that's a real ordering concern — but each calls InitTracing fresh and asserts via the returned state, so they're independent. If `TestInitTracing_EnabledByFlagWithoutEndpoint` leaves a real provider installed that later tests dislike, add `t.Cleanup(func(){ otel.SetTracerProvider(noop.NewTracerProvider()) })` using `go.opentelemetry.io/otel/trace/noop`.

- [ ] **Step 6: Commit**

```bash
git add internal/obs/tracing.go internal/obs/tracing_test.go go.mod go.sum
git commit -m "feat(obs): tracing core — InitTracing (no-op gate), StartSpan, attr builders"
```

---

## Task 2: Span attributes + propagator round-trip tests

**Files:**
- Test: `internal/obs/tracing_test.go` (append)

- [ ] **Step 1: Write the failing tests**

Append to `internal/obs/tracing_test.go` (add imports `"go.opentelemetry.io/otel"`, `"go.opentelemetry.io/otel/propagation"`, `sdktrace "go.opentelemetry.io/otel/sdk/trace"`, `"go.opentelemetry.io/otel/sdk/trace/tracetest"`, `"net/http"`):

```go
func TestStartSpan_RecordsStructuralAttrsNoContent(t *testing.T) {
	// Install an in-memory recorder so we can inspect emitted spans/attrs.
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	_, span := StartSpan(context.Background(), "agent.turn",
		AgentAttr("a1"), TenantAttr("acme"), SessionAttr("ses-1"),
		RequestIDAttr("req-xyz"), TurnAttr(2), OutcomeAttr("completed"))
	span.End()

	spans := rec.Ended()
	if len(spans) != 1 || spans[0].Name() != "agent.turn" {
		t.Fatalf("want one agent.turn span, got %+v", spans)
	}
	got := map[string]string{}
	for _, kv := range spans[0].Attributes() {
		got[string(kv.Key)] = kv.Value.Emit()
	}
	for k, want := range map[string]string{
		"agent.id": "a1", "tenant.id": "acme", "session.id": "ses-1",
		"request.id": "req-xyz", "turn.number": "2", "outcome": "completed",
	} {
		if got[k] != want {
			t.Fatalf("attr %q = %q, want %q (all attrs: %v)", k, got[k], want, got)
		}
	}
	// The "no content" guard: assert no content-ish keys ever appear.
	for _, banned := range []string{"message", "content", "prompt", "tool.args", "tool.result", "arguments"} {
		if _, present := got[banned]; present {
			t.Fatalf("span carries banned content attribute %q", banned)
		}
	}
}

func TestPropagator_W3CRoundTrip(t *testing.T) {
	// After InitTracing installs the propagator, inject→extract round-trips.
	t.Setenv("RUNTIME_TRACING_ENABLED", "1")
	shutdown, err := InitTracing(context.Background(), "test-svc")
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown(context.Background())

	prop := otel.GetTextMapPropagator()
	// Start a span so there's an active span context to inject.
	ctx, span := StartSpan(context.Background(), "root")
	defer span.End()

	req, _ := http.NewRequest("GET", "http://x/y", nil)
	prop.Inject(ctx, propagation.HeaderCarrier(req.Header))
	if req.Header.Get("traceparent") == "" {
		t.Fatal("traceparent not injected")
	}
	// Extract on a fresh context and confirm the span context propagates.
	got := prop.Extract(context.Background(), propagation.HeaderCarrier(req.Header))
	if !trace.SpanContextFromContext(got).IsValid() {
		t.Fatal("extracted span context invalid")
	}
}
```

(Add import `"go.opentelemetry.io/otel/trace"` if not already present — it is, from StartSpan's signature use in tests... actually it isn't yet; add it.)

- [ ] **Step 2: Run to verify fail, then pass**

Run: `go test ./internal/obs/ -run 'StartSpan|Propagator' -v`
First expect FAIL only if a helper is missing (the builders exist from Task 1, so this should pass once imports are right). If it passes immediately, that's fine — these tests lock in Task 1's behavior. Confirm: `go test ./internal/obs/ -v` (whole package green), `go vet ./...`, `gofmt -l internal/obs/tracing_test.go` (empty).

- [ ] **Step 3: Commit**

```bash
git add internal/obs/tracing_test.go
git commit -m "test(obs): span attribute recording (no-content guard) + W3C propagator round-trip"
```

---

## Task 3: runtimed edge + outbound propagation (otelhttp seams)

**Files:**
- Modify: `controlplane/proxy.go` (`reverseProxy`)
- Modify: `cmd/runtimed/main.go` (wrap root handler)
- Create: `controlplane/tracing_test.go`

- [ ] **Step 1: Write the failing test (edge span named by route)**

Create `controlplane/tracing_test.go`:

```go
package controlplane

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/sausheong/runtime/internal/config"
	"github.com/sausheong/runtime/internal/obs"
)

// The reverse proxy must inject traceparent on the proxied request when a span
// is active (otelhttp transport), and the backend must observe it.
func TestReverseProxy_InjectsTraceparent(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	// Ensure the W3C propagator is installed (InitTracing does this in prod).
	_, _ = obs.InitTracing(context.Background(), "test")

	var gotTP string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTP = r.Header.Get("traceparent")
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	// Start a span so there's a context to inject from.
	ctx, span := obs.StartSpan(context.Background(), "edge")
	defer span.End()

	rp := reverseProxy(backend.URL, "", nil)
	rec2 := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/sessions", nil).WithContext(ctx)
	rp.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Fatalf("proxy code = %d", rec2.Code)
	}
	if gotTP == "" {
		t.Fatal("backend did not receive traceparent (otelhttp transport not wired)")
	}
}

var _ = config.Config{} // keep config import if unused elsewhere; remove if vet complains
```

(If the `config` import is unused, delete that line and the import — it's only there if you reuse config; you don't here, so omit both the blank-var line and the config import.)

- [ ] **Step 2: Run to verify fail**

Run: `go test ./controlplane/ -run InjectsTraceparent -v`
Expected: FAIL — backend receives no traceparent (proxy transport is plain `authTransport`, no otelhttp).

- [ ] **Step 3: Wrap the reverse-proxy transport with otelhttp**

In `controlplane/proxy.go`, add import `"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"`. In `reverseProxy`, change the transport assignment from:

```go
	rp.Transport = authTransport{token: token}
```
to:
```go
	// otelhttp wraps the auth transport: it injects traceparent from the active
	// span and records a client span. With tracing off (no-op provider) this is
	// a cheap pass-through.
	rp.Transport = otelhttp.NewTransport(authTransport{token: token})
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./controlplane/ -run InjectsTraceparent -v`
Expected: PASS. Then `go test ./controlplane/ -v` (all existing proxy/api tests still pass — the otelhttp wrap is transparent).

- [ ] **Step 5: Wrap runtimed's root handler with otelhttp (edge server span)**

In `cmd/runtimed/main.go`, add import `"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"`. The handler chain is assembled as `handler = obs.RequestID(...inner...)` in TWO branches (open-mode ~line 166 and identity ~line 208). Insert an otelhttp server span BETWEEN the existing `obs.RequestID` (which must stay outermost — it mutates `r.Header`) and the inner chain, via a small `tracedHandler` helper. Change both assignment sites:

In the open-mode branch (~line 166), change:
```go
		handler = obs.RequestID(accessLog(buildRoot(reg, nil, console.OIDCConfig{}, secretAdmin, gwHandler, cm), cm))
```
to:
```go
		handler = obs.RequestID(tracedHandler(accessLog(buildRoot(reg, nil, console.OIDCConfig{}, secretAdmin, gwHandler, cm), cm)))
```

In the identity branch (~line 208), change:
```go
		handler = obs.RequestID(controlplane.IdentityMiddleware(accessLog(root, cm), authr, azr, func(status int) {
			cm.AuthRejected(status)
		}))
```
to:
```go
		handler = obs.RequestID(tracedHandler(controlplane.IdentityMiddleware(accessLog(root, cm), authr, azr, func(status int) {
			cm.AuthRejected(status)
		})))
```

Then add a small helper near `buildRoot` in `cmd/runtimed/main.go`:
```go
// tracedHandler wraps h with an otelhttp server span named by matched route
// (never the raw path — cardinality-safe). Placed inside RequestID so the id is
// already in context; transparent when tracing is off (no-op provider).
func tracedHandler(h http.Handler) http.Handler {
	return otelhttp.NewHandler(h, "runtimed.request",
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			if r.Pattern != "" {
				return r.Method + " " + r.Pattern
			}
			return r.Method
		}),
	)
}
```
Delete the earlier "insert before srv" block idea — use ONLY the `tracedHandler` wrapping at the two assignment sites. (`r.Pattern` is set by the inner mux on match; otelhttp's formatter runs after ServeHTTP only for the name — note that `r.Pattern` may be empty at span-start. That's acceptable: span name falls back to method; the route is also captured by otelhttp's own `http.route` attribute when available. Do not over-engineer.)

- [ ] **Step 6: Build, vet, fmt**

Run: `go build ./... && go vet ./... && gofmt -l cmd/runtimed/main.go controlplane/proxy.go controlplane/tracing_test.go`
Expected: build/vet clean, gofmt prints nothing.
Run: `go test ./controlplane/ -v` (all pass).

- [ ] **Step 7: Commit**

```bash
git add controlplane/proxy.go controlplane/tracing_test.go cmd/runtimed/main.go
git commit -m "feat(obs): runtimed edge server span + traceparent injection on the proxy hop"
```

---

## Task 4: agentd inbound server span (otelhttp), continuing the parent

**Files:**
- Modify: `agentruntime/server.go` (`handler()`)
- Test: `agentruntime/server_test.go` (append)

- [ ] **Step 1: Write the failing test (extracted parent)**

Append to `agentruntime/server_test.go` (add imports `"context"` if missing — it's present; `"go.opentelemetry.io/otel"`, `"go.opentelemetry.io/otel/propagation"`, `sdktrace "go.opentelemetry.io/otel/sdk/trace"`, `"go.opentelemetry.io/otel/sdk/trace/tracetest"`, `"go.opentelemetry.io/otel/trace"`):

```go
func TestHandler_ContinuesInboundTrace(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	srv := httptest.NewServer(newTestManager().handler())
	defer srv.Close()

	// Build a parent span context and inject it into the request headers.
	ctx, parent := tp.Tracer("test").Start(context.Background(), "client")
	req, _ := http.NewRequest("GET", srv.URL+"/healthz", nil)
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
	parentTID := parent.SpanContext().TraceID()
	parent.End()

	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("healthz: err=%v status=%v", err, resp.StatusCode)
	}
	resp.Body.Close()

	// The agentd server span must share the inbound trace id (same trace).
	var found bool
	for _, s := range rec.Ended() {
		if s.SpanContext().TraceID() == parentTID && s.SpanKind() == trace.SpanKindServer {
			found = true
		}
	}
	if !found {
		t.Fatal("no agentd server span continued the inbound trace id")
	}
}
```

- [ ] **Step 2: Run to verify fail**

Run: `go test ./agentruntime/ -run ContinuesInboundTrace -v`
Expected: FAIL — agentd's `handler()` has no otelhttp, so no server span is created under the inbound trace.

- [ ] **Step 3: Wrap agentd's handler with otelhttp**

In `agentruntime/server.go`, add import `"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"`. In `handler()`, the current end is:

```go
	var h http.Handler = logged
	if m.authToken != "" {
		h = requireBearer(m.authToken, logged)
	}
	return obs.RequestID(h)
```

Change to insert otelhttp just inside RequestID (and inside requireBearer, so authenticated requests still get a span; an unauthenticated 401 short-circuits before the span — acceptable, matching that probes need auth):

```go
	var h http.Handler = logged
	// otelhttp server span: continues an inbound traceparent (parent) so the
	// agentd work nests under runtimed's trace. Named by route, not raw path.
	h = otelhttp.NewHandler(h, "agentd.request",
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			if r.Pattern != "" {
				return r.Method + " " + r.Pattern
			}
			return r.Method
		}),
	)
	if m.authToken != "" {
		h = requireBearer(m.authToken, h)
	}
	return obs.RequestID(h)
```

(Order: RequestID outermost → requireBearer → otelhttp → logged → mux. requireBearer stays outside otelhttp so a 401 isn't traced as a full handled request — minor, and keeps unauthorized noise out of traces.)

- [ ] **Step 4: Run to verify pass**

Run: `go test ./agentruntime/ -run ContinuesInboundTrace -v`
Expected: PASS. Then `go test ./agentruntime/ -v` (all existing pass, incl. the bearer-auth tests — confirm the otelhttp wrap didn't disturb them).

- [ ] **Step 5: Build, vet, fmt, commit**

Run: `go build ./... && go vet ./... && gofmt -l agentruntime/server.go agentruntime/server_test.go`
```bash
git add agentruntime/server.go agentruntime/server_test.go
git commit -m "feat(obs): agentd inbound server span continues the inbound traceparent"
```

---

## Task 5: In-process spans — session workflow, turn, tool

**Files:**
- Modify: `agentruntime/serve.go` (`sessionWorkflow`, `observeTurn`)

- [ ] **Step 1: The ACTUAL current shape (verified — read before editing)**

`sessionWorkflow(ctx dbos.DBOSContext, in turnInput) (string, error)` (in `agentruntime/serve.go`):
- The session id is `wfID, _ := dbos.GetWorkflowID(ctx)` near the top — **there is NO `SessionID` field** on `turnInput`. `turnInput` (in `agentruntime/turnstep.go`) has: `UserMsg`, `ImageB64`, `ImageMime`, `RequestID`. So use `wfID` for the session id and `in.RequestID` for the request id.
- **There is NO tenant field** on `m.cfg.Spec` (AgentSpec) or the Manager — do NOT add a `TenantAttr` to these spans (omit it; do not invent a field).
- The loop is `for turn := 0; ; turn++ {` (loop var is `turn`). Inside it, the turn work runs **inside a `dbos.RunAsStep` closure**:
  ```go
  out, stepErr := dbos.RunAsStep(ctx, func(stepCtx context.Context) (turnOutput, error) {
      ...
      start := time.Now()
      tr, terr := rt.RunTurn(stepCtx, userMsg, images, nil)
      elapsed := time.Since(start)
      if terr != nil {
          m.observeTurn("error", elapsed, nil, nil)
          slog.Warn("turn failed", ...)
          return turnOutput{}, terr
      }
      m.observeTurn(tr.StopReason, elapsed, tr.Usage, tr.Entries)
      slog.Info("turn", ...)
      ... // builds and returns turnOutput
  })
  ```
  So the **turn span and tool spans must be created INSIDE this closure, parented on `stepCtx`** (the DBOS step context, which is a real `context.Context`). The closure runs once per real turn and is SKIPPED on replay — which is exactly the "live-execution only" semantics we want for spans (no extra replay guard needed; DBOS already skips completed steps).
- There is NO unit test for sessionWorkflow (needs DBOS+Postgres); this task is covered by the Task 8 integration test + the live proof. Verify via build/vet here.

`obs`, `context`, `time`, `slog`, `session`, `json`, `llm` are all already imported in serve.go.

- [ ] **Step 2: Add the session.workflow span (wraps the outer loop)**

In `sessionWorkflow`, after `wfID` is obtained and `canonical` is created but BEFORE the `for turn := 0; ; turn++` loop, start the workflow span from the live `ctx`. `dbos.DBOSContext` embeds `context.Context`, so it can be passed to `obs.StartSpan` directly (it satisfies the interface). Insert:

```go
	// Live-execution span only (NOT checkpointed): on DBOS replay the workflow
	// body re-runs but completed turn STEPS are skipped, so spans created here
	// and inside the step closure naturally reflect only live work. A span is a
	// live concern, never durable state.
	wctx, wspan := obs.StartSpan(ctx, "session.workflow",
		obs.AgentAttr(m.agentID), obs.SessionAttr(wfID), obs.RequestIDAttr(in.RequestID))
	defer wspan.End()
```

If the compiler rejects passing `ctx` (a `dbos.DBOSContext`) where `context.Context` is wanted, wrap it: `obs.StartSpan(context.Context(ctx), ...)`. Verify by building. `wctx` is the workflow span context — but note the turn loop calls `dbos.RunAsStep(ctx, ...)` using the ORIGINAL `ctx` (DBOSContext), which MUST stay the DBOS context for durability. Do NOT pass `wctx` to `RunAsStep`. Instead, the turn span (Step 3) is parented on the closure's `stepCtx`, and to link it under the workflow span we accept that the step closure's `stepCtx` derives from the DBOS `ctx` (same trace root if tracing context flows through DBOS; if it does not, the turn spans will be roots sharing the trace via the inbound traceparent on the agentd server span — acceptable for M2, since the agentd server span is the true parent of all agent work). Keep it simple: parent turn spans on `stepCtx`.

- [ ] **Step 3: Add agent.turn + tool.call spans INSIDE the RunAsStep closure**

Inside the `dbos.RunAsStep(ctx, func(stepCtx context.Context) (turnOutput, error) {` closure, wrap the `RunTurn`/`observeTurn` region. The closure already computes `start`/`elapsed` and has an error branch (`return turnOutput{}, terr`) and a success path. Edit as follows.

Immediately before `start := time.Now()` (or just before `rt.RunTurn`), add:
```go
			turnCtx, tspan := obs.StartSpan(stepCtx, "agent.turn", obs.TurnAttr(turn))
```

In the ERROR branch, before the existing `m.observeTurn("error", ...)`, set outcome + end the span:
```go
			if terr != nil {
				tspan.SetAttributes(obs.OutcomeAttr("error"))
				tspan.End()
				m.observeTurn("error", elapsed, nil, nil)
				slog.Warn("turn failed", "agent", m.agentID, "session", wfID,
					"turn", turn, "request_id", in.RequestID, "err", terr)
				return turnOutput{}, terr
			}
```

In the SUCCESS path, after `tr` is known, set outcome, emit one `tool.call` child span per tool entry (names only — no args), then end the turn span, keeping the existing `m.observeTurn(tr.StopReason, ...)` call:
```go
			tspan.SetAttributes(obs.OutcomeAttr(tr.StopReason))
			for _, e := range tr.Entries {
				if e.Type == session.EntryTypeToolCall {
					var td session.ToolCallData
					if err := json.Unmarshal(e.Data, &td); err == nil && td.Tool != "" {
						_, toolSpan := obs.StartSpan(turnCtx, "tool.call", obs.ToolAttr(td.Tool))
						toolSpan.End()
					}
				}
			}
			tspan.End()
			m.observeTurn(tr.StopReason, elapsed, tr.Usage, tr.Entries)
```

IMPORTANT:
- Tool spans are parented on `turnCtx` (the turn span's context) so they nest tool→turn in the trace.
- Match the EXISTING control flow — do NOT change `RunAsStep`, the loop control, the `slog` lines, the `turnOutput` construction, or duplicate `observeTurn`. Only insert the three span fragments (start before RunTurn; end+outcome in each branch; tool loop in the success path). The tool-entry loop mirrors the unmarshal `observeTurn` already does, reusing `session.ToolCallData` (already imported).
- The `_ = wctx` is unused if you don't reference it; since `wspan` is ended via defer and turn spans parent on `stepCtx`, you may not need `wctx`. If `wctx` is unused, name it `_`: `_, wspan := obs.StartSpan(...)`. (Go will error on an unused local — use `_` for the context if you don't consume it.)

- [ ] **Step 4: Build, vet, fmt**

Run: `go build ./... && go vet ./... && gofmt -l agentruntime/serve.go`
Expected: clean. (Behavior is covered by the Task 8 integration test; there's no hermetic unit test for the DBOS workflow.)

- [ ] **Step 5: Commit**

```bash
git add agentruntime/serve.go
git commit -m "feat(obs): session.workflow + agent.turn + tool.call spans (live-execution, no content)"
```

---

## Task 6: Gateway upstream span

**Files:**
- Modify: `internal/gateway/server.go` (around `t.Execute`)

- [ ] **Step 1: Read the call site**

In `internal/gateway/server.go`, the tool-call handler computes `serverName`, then:
```go
		start := time.Now()
		res, err := t.Execute(ctx, args)
		dur := time.Since(start)
		if err != nil { h.Metrics.GatewayCall(serverName, t.Name(), obs.OutcomeError, dur); return errResult(err.Error()), nil }
		if res.Error != "" { h.Metrics.GatewayCall(serverName, t.Name(), obs.OutcomeError, dur); return errResult(res.Error), nil }
		h.Metrics.GatewayCall(serverName, t.Name(), obs.OutcomeOK, dur)
```
`obs` is already imported (used for `obs.OutcomeError`/`obs.OutcomeOK`).

- [ ] **Step 2: Wrap the execute in a gateway.upstream span**

Change the block to start a span before `t.Execute` and set its outcome alongside each `GatewayCall`:

```go
		start := time.Now()
		uctx, uspan := obs.StartSpan(ctx, "gateway.upstream",
			obs.GatewayServerAttr(serverName), obs.GatewayToolAttr(t.Name()))
		res, err := t.Execute(uctx, args)
		dur := time.Since(start)
		if err != nil {
			uspan.SetAttributes(obs.OutcomeAttr(obs.OutcomeError))
			uspan.End()
			h.Metrics.GatewayCall(serverName, t.Name(), obs.OutcomeError, dur)
			return errResult(err.Error()), nil
		}
		if res.Error != "" {
			uspan.SetAttributes(obs.OutcomeAttr(obs.OutcomeError))
			uspan.End()
			h.Metrics.GatewayCall(serverName, t.Name(), obs.OutcomeError, dur)
			return errResult(res.Error), nil
		}
		uspan.SetAttributes(obs.OutcomeAttr(obs.OutcomeOK))
		uspan.End()
		h.Metrics.GatewayCall(serverName, t.Name(), obs.OutcomeOK, dur)
```

(Pass `uctx` to `t.Execute` so a traced MCP transport, if any, continues the trace; harmless otherwise.)

- [ ] **Step 3: Build, vet, fmt, test**

Run: `go build ./... && go vet ./... && gofmt -l internal/gateway/server.go`
Run: `go test ./internal/gateway/ -v` (existing gateway tests still pass — the span wrap is transparent).

- [ ] **Step 4: Commit**

```bash
git add internal/gateway/server.go
git commit -m "feat(obs): gateway.upstream span around federated tool execution"
```

---

## Task 7: Process wiring (InitTracing in both binaries) + compose overlay + docs

**Files:**
- Modify: `cmd/runtimed/main.go` (InitTracing + shutdown)
- Modify: `cmd/agentd/main.go` or `agentruntime/serve.go` (InitTracing + flush in drain)
- Create: `deploy/otel/collector-config.yaml`
- Modify: `deploy/docker-compose.obs.yml`
- Modify: `README.md`, `ROADMAP.md`

- [ ] **Step 1: Wire InitTracing in runtimed**

In `cmd/runtimed/main.go`, right after `setupLogging()` (line ~30) and after `ctx, stop := signal.NotifyContext(...)` exists (line ~49) — place it after the ctx is created so shutdown can use it. Insert after line ~50 (`defer stop()`):

```go
	traceShutdown, terr := obs.InitTracing(ctx, "runtimed")
	if terr != nil {
		slog.Warn("tracing init failed; continuing without traces", "err", terr)
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = traceShutdown(sctx)
	}()
```

(`obs`, `slog`, `context`, `time` are already imported.)

- [ ] **Step 2: Wire InitTracing in agentd**

In `agentruntime/serve.go` `Serve`, near the top (after `listenAddr` is validated, before `store.NewPGStore`), insert:

```go
	traceShutdown, terr := obs.InitTracing(ctx, cfg.Spec.ID)
	if terr != nil {
		slog.Warn("agentd tracing init failed; continuing without traces", "err", terr)
	}
```

(`obs` already imported for metrics; `slog` is imported.) Then in the shutdown drain (the `case <-ctx.Done():` block at ~line 310, which runs `srv.Shutdown` then returns, letting deferred `dbos.Shutdown`/`st.Close` run), flush traces BEFORE returning — add after `_ = srv.Shutdown(shutCtx)`:

```go
		fctx, fcancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = traceShutdown(fctx)
		fcancel()
```

(Place the trace flush after HTTP drain so in-flight spans complete, before dbos shutdown. `context` is imported.)

- [ ] **Step 3: Create the collector config**

Create `deploy/otel/collector-config.yaml`:

```yaml
# Minimal OTel Collector: receive OTLP/HTTP, batch, export to Jaeger (OTLP).
receivers:
  otlp:
    protocols:
      http:
        endpoint: 0.0.0.0:4318

processors:
  batch: {}

exporters:
  otlp/jaeger:
    endpoint: jaeger:4317
    tls:
      insecure: true

service:
  pipelines:
    traces:
      receivers: [otlp]
      processors: [batch]
      exporters: [otlp/jaeger]
```

- [ ] **Step 4: Add collector + jaeger to the obs overlay**

In `deploy/docker-compose.obs.yml`, add under `services:` (alongside prometheus/grafana):

```yaml
  otel-collector:
    image: otel/opentelemetry-collector-contrib:latest
    command: ["--config=/etc/otel/collector-config.yaml"]
    volumes:
      - ./otel/collector-config.yaml:/etc/otel/collector-config.yaml:ro
    ports:
      - "4318:4318"   # OTLP HTTP
    depends_on:
      - jaeger

  jaeger:
    image: jaegertracing/all-in-one:latest
    environment:
      COLLECTOR_OTLP_ENABLED: "true"
    ports:
      - "16686:16686"  # Jaeger UI
```

And document at the top of the overlay file (extend the header comment) that Jaeger UI is at http://localhost:16686. If runtimed/agentd run INSIDE compose (check `deploy/docker-compose.yml` for the runtimed service), add `OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4318` to their environment in the overlay via a service override. If they run on the host (the obs overlay uses `host.docker.internal` for Prometheus scraping — implying host-run binaries), then the operator sets `OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318` on the host process; document that in the README instead. INSPECT `deploy/docker-compose.yml` to determine which, and wire accordingly. Do not guess — match the existing topology.

- [ ] **Step 5: Docs — README tracing subsection + ROADMAP M2 entry**

In `README.md`, find the observability/metrics section (search "Grafana" or "Prometheus"). Add a "Distributed tracing" subsection:

```markdown
### Distributed tracing (OpenTelemetry)

Tracing is **off by default**. Enable it by pointing the binaries at an OTLP
endpoint (no endpoint ⇒ a no-op tracer, zero overhead):

- `OTEL_EXPORTER_OTLP_ENDPOINT` — e.g. `http://localhost:4318` (presence enables tracing)
- `RUNTIME_TRACING_ENABLED` — explicit `1`/`0` override
- `RUNTIME_TRACE_SAMPLE_RATIO` — `0.0`–`1.0` (default `1.0`)

The obs compose overlay runs an OpenTelemetry Collector + Jaeger:

\`\`\`bash
docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.obs.yml up -d
\`\`\`

Open Jaeger at http://localhost:16686. A single request produces one trace
spanning `runtimed` and the `agentd` (per-agent service), with the
edge→proxy→handler→session.workflow→turn→tool span tree. Spans carry IDs and
structural attributes only (agent/tenant/session/turn/tool/outcome/tokens) —
no message content or tool arguments. The root span's `request.id` equals the
`X-Request-ID` echoed on the response, joining traces to logs.
```

(Replace the `\`\`\`` with real triple backticks when writing.)

In `ROADMAP.md`, under the Observability sub-project (search "Observability — tracing, metrics"), after the M1 entry, add a `**Second milestone DONE (2026-06-13):**` dense-prose paragraph in the house style summarizing: OTel tracing across runtimed→agentd→gateway; `internal/obs/tracing.go` single owner (no-op gate, parent-based+ratio sampler, W3C propagator, attr builders enforcing IDs-only/no-content); otelhttp at 3 seams (edge, proxy transport, agentd inbound) carrying traceparent; in-process spans session.workflow/agent.turn/tool.call/gateway.upstream at the obs-M1 metric boundaries; off-by-default (no endpoint ⇒ no-op, zero overhead); compose overlay adds OTel Collector + Jaeger (:16686); DBOS-replay caveat (spans live-only, not checkpointed); tool/LLM spans are post-turn reconstruction (live-wrapped deferred); sandboxd internals deferred. Note remaining B5. Cite spec/plan `docs/superpowers/{specs,plans}/2026-06-13-observability-m2-tracing*`.

- [ ] **Step 6: Build, vet, fmt, full hermetic suite**

Run: `go build ./... && go vet ./... && go test ./...`
Run: `gofmt -l cmd/runtimed/main.go agentruntime/serve.go`
Expected: all pass; gofmt empty.

- [ ] **Step 7: Commit**

```bash
git add cmd/runtimed/main.go agentruntime/serve.go deploy/otel/collector-config.yaml deploy/docker-compose.obs.yml README.md ROADMAP.md
git commit -m "feat(obs): InitTracing wiring in both binaries + Collector/Jaeger overlay + docs"
```

---

## Task 8: Integration test — two-process end-to-end trace

**Files:**
- Create: `test/tracing_e2e_test.go` (`//go:build integration`)

- [ ] **Step 1: Understand the harness**

The `test` package already has `dsn`, `mustExec`, and starts runtimed/agentd subprocesses (see `test/multiagent_test.go`, `test/remote_agent_test.go`). For tracing we need both processes to export to a receiver THIS test controls. Simplest: run an in-test OTLP/HTTP receiver (a plain `httptest.Server` that accepts POST `/v1/traces` and records the protobuf), set `OTEL_EXPORTER_OTLP_ENDPOINT` to its URL for both subprocesses, drive one session, and assert spans from both services share a trace id.

Decoding OTLP protobuf in-test is heavy. SIMPLER + sufficient: assert the cross-process linkage at the HTTP layer instead — that the proxied request to agentd carries a `traceparent` whose trace-id matches the one runtimed used — OR assert via the receiver that it received ≥2 resource spans with service.name `runtimed` and the agent id. Choose the receiver-count approach but keep decoding minimal: the OTLP/HTTP body is protobuf; to avoid a protobuf dep, assert only that the receiver got at least one POST to `/v1/traces` from a traced run AND zero when tracing is off. For the trace-id-shared assertion, use the SDK directly in-process is not possible across subprocesses — so this e2e proves EXPORT happens end-to-end (both processes push traces when enabled; none when disabled), and the hermetic tests (Tasks 2–4) prove the propagation/parenting semantics.

- [ ] **Step 2: Write the integration test**

Create `test/tracing_e2e_test.go`:

```go
//go:build integration

package test

import (
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestTracingE2E proves both runtimed and agentd export spans to an OTLP
// endpoint when tracing is enabled, and export NOTHING when disabled.
func TestTracingE2E(t *testing.T) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatalf("ping postgres: %v", err)
	}
	mustExec(t, db, `DROP TABLE IF EXISTS markers`)
	mustExec(t, db, `CREATE TABLE markers (id BIGSERIAL PRIMARY KEY, ran_at TIMESTAMPTZ)`)
	mustExec(t, db, `DROP TABLE IF EXISTS session_events, sessions, agents CASCADE`)
	mustExec(t, db, `DROP SCHEMA IF EXISTS dbos CASCADE`)

	// In-test OTLP/HTTP receiver: count POSTs to /v1/traces.
	var tracePosts atomic.Int32
	otlp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/traces" {
			_, _ = io.Copy(io.Discard, r.Body)
			tracePosts.Add(1)
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer otlp.Close()

	tmp := t.TempDir()
	agentd := filepath.Join(tmp, "agentd")
	if out, err := exec.Command("go", "build", "-o", agentd, "../cmd/agentd").CombinedOutput(); err != nil {
		t.Fatalf("build agentd: %v\n%s", err, out)
	}
	runtimed := filepath.Join(tmp, "runtimed")
	if out, err := exec.Command("go", "build", "-o", runtimed, "../cmd/runtimed").CombinedOutput(); err != nil {
		t.Fatalf("build runtimed: %v\n%s", err, out)
	}

	cfgPath := filepath.Join(tmp, "runtime.yaml")
	if err := os.WriteFile(cfgPath, []byte(
		"agents:\n  - {id: tracer, name: Tracer, model: test/scripted, listen_addr: 127.0.0.1:8412}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctlAddr := "127.0.0.1:8420"
	rt := exec.Command(runtimed)
	rt.Env = append(os.Environ(),
		"RUNTIME_PG_DSN="+dsn,
		"RUNTIME_CTL_ADDR="+ctlAddr,
		"RUNTIME_AGENTD_BIN="+agentd,
		"RUNTIME_CONFIG="+cfgPath,
		"OTEL_EXPORTER_OTLP_ENDPOINT="+otlp.URL,
		"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL=http/protobuf",
		"RUNTIME_TRACE_SAMPLE_RATIO=1.0",
	)
	rt.Stdout, rt.Stderr = os.Stdout, os.Stderr
	rt.SysProcAttr = setpgid()
	if err := rt.Start(); err != nil {
		t.Fatal(err)
	}
	defer killGroup(rt)

	waitCtlHealthy(t, "http://"+ctlAddr+"/healthz", 30*time.Second)

	// Drive a session (proxied to agentd) — both processes should emit spans.
	resp, err := http.Post("http://"+ctlAddr+"/agents/tracer/sessions", "application/json",
		stringReader(`{"message":"trace me"}`))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("session create status=%d", resp.StatusCode)
	}

	// Spans are batched; give the exporters time to flush.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if tracePosts.Load() > 0 {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if tracePosts.Load() == 0 {
		t.Fatal("no OTLP trace exports received with tracing enabled")
	}
}
```

NOTE: helper names `setpgid()`, `killGroup()`, `waitCtlHealthy()`, `stringReader()` may not exist in the `test` package. INSPECT first (`grep -rn "func setpgid\|func killGroup\|func waitCtlHealthy\|func stringReader\|SysProcAttr\|func waitHealthy" test/*.go`). The existing tests use inline `&syscall.SysProcAttr{Setpgid: true}` and `syscall.Kill(-pid, ...)`. REUSE the existing patterns directly inline rather than inventing helpers: replace `setpgid()` with `&syscall.SysProcAttr{Setpgid: true}` (import `syscall`), `killGroup(rt)` with an inline `func(){ syscall.Kill(-rt.Process.Pid, syscall.SIGKILL); rt.Process.Wait() }()`, `waitCtlHealthy` with the C3 test's `rmtWaitHealthy(t, url, "", d)` IF it's in-package (it is — `test/remote_agent_test.go` defines it), and `stringReader(s)` with `strings.NewReader(s)` (import `strings`). Confirm `rmtWaitHealthy` is visible (same package, both integration-tagged) — if so reuse it; otherwise inline a poll loop. Adjust imports accordingly.

- [ ] **Step 3: Run the integration test**

Run: `go test -tags integration ./test/ -run TestTracingE2E -v -timeout 180s`
Expected: PASS — at least one OTLP trace export received. Watch stderr for runtimed/agentd "tracing" behavior (no explicit log line required).
If it fails with no exports: confirm `OTEL_EXPORTER_OTLP_ENDPOINT` reaches both processes (runtimed passes its own env to spawned agentd via `os.Environ()` in the spawn path — verify agentd inherits it; it should, since SpawnFunc uses `os.Environ()`). Confirm the otlptracehttp default path is `/v1/traces`.

- [ ] **Step 4: Commit**

```bash
git add test/tracing_e2e_test.go
git commit -m "test(obs): integration — both processes export OTLP traces end-to-end"
```

---

## Final verification (after all tasks)

- [ ] Hermetic suite green: `go test ./... && go vet ./...`
- [ ] gofmt clean: `gofmt -l $(git diff --name-only master..HEAD | grep '\.go$')` prints nothing
- [ ] Integration: `go test -tags integration ./test/ -run TestTracingE2E -v` (Postgres.app up)
- [ ] Off-by-default proof: `go test ./...` passes WITHOUT any OTEL env set (zero-overhead path is the default test environment — confirms back-compat)
- [ ] Live proof (manual milestone gate — spec §6): bring up the obs overlay (collector+Jaeger), drive a `runtimectl invoke -v` session, open Jaeger :16686, show the end-to-end runtimed+agentd trace with the span tree + ID attributes (no content), prove request.id↔X-Request-ID join, show a gateway.upstream span, and show off=zero with the base compose.
- [ ] Then `finishing-a-development-branch` (merge to `master`).

---

## Self-Review (completed by plan author)

**Spec coverage:**
- §2 tracing core (InitTracing, no-op gate, sampler, propagator, StartSpan, attr builders) → Tasks 1–2.
- §3 cross-process propagation (otelhttp at edge, proxy transport, agentd inbound) → Tasks 3–4.
- §4 in-process spans (session.workflow, agent.turn, tool.call, gateway.upstream) → Tasks 5–6.
- §5 wiring (both binaries InitTracing+shutdown), export overlay (collector+Jaeger), env surface, docs → Task 7.
- §6 testing: hermetic (Tasks 1–4), integration (Task 8), live proof (final verification).
- Out-of-scope items (sandbox internals, content attrs, live-wrapped spans) correctly NOT implemented; recorded in ROADMAP (Task 7).

**Type/signature consistency:** `InitTracing(ctx, name) (func(context.Context) error, error)` defined Task 1, called Task 7 (both binaries). `StartSpan(ctx, name, ...attribute.KeyValue) (context.Context, trace.Span)` defined Task 1, used Tasks 2/5/6. Attr builders (`AgentAttr`/`TenantAttr`/`SessionAttr`/`RequestIDAttr`/`TurnAttr`/`ToolAttr`/`OutcomeAttr`/`GatewayServerAttr`/`GatewayToolAttr`) defined Task 1, used Tasks 2/5/6. `tracedHandler` defined+used Task 3. `TracingEnabled`/`sampleRatio` defined+tested Task 1.

**Known cross-file edits flagged for implementers:** otelhttp transport wrap touches the existing `reverseProxy` (Task 3, transparent to its tests); the runtimed handler-assembly has TWO branches to update (Task 3 Step 5); agentd `handler()` middleware order is explicit (Task 4); the `sessionWorkflow`/`observeTurn` span insertion MUST match existing control flow and real `turnInput`/loop-var field names (Task 5 — inspect before editing, do not invent fields); the compose wiring depends on whether binaries run in-compose or on-host (Task 7 Step 4 — inspect docker-compose.yml); integration-test helper reuse vs. inline (Task 8 — inspect test package first).

**Risk note:** Task 5 is the highest-judgment task (editing the durable loop's control flow). The implementer must read the real `sessionWorkflow` body and adapt span calls into existing branches without changing loop control or duplicating `observeTurn`. If the loop variable/field names differ from the plan's placeholders, use the real ones. The integration test (Task 8) + live proof are the safety net.
