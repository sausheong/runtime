package obs

import (
	"context"
	"net/http"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestInitTracing_NoEndpointIsNoop(t *testing.T) {
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
	if TracingEnabled() {
		t.Fatal("TracingEnabled() = true with no endpoint")
	}
}

func TestInitTracing_EnabledByFlagWithoutEndpoint(t *testing.T) {
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

func TestStartSpan_RecordsStructuralAttrsNoContent(t *testing.T) {
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
	for _, banned := range []string{"message", "content", "prompt", "tool.args", "tool.result", "arguments"} {
		if _, present := got[banned]; present {
			t.Fatalf("span carries banned content attribute %q", banned)
		}
	}
}

func TestPropagator_W3CRoundTrip(t *testing.T) {
	t.Setenv("RUNTIME_TRACING_ENABLED", "1")
	shutdown, err := InitTracing(context.Background(), "test-svc")
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown(context.Background())

	prop := otel.GetTextMapPropagator()
	ctx, span := StartSpan(context.Background(), "root")
	defer span.End()

	req, _ := http.NewRequest("GET", "http://x/y", nil)
	prop.Inject(ctx, propagation.HeaderCarrier(req.Header))
	if req.Header.Get("traceparent") == "" {
		t.Fatal("traceparent not injected")
	}
	got := prop.Extract(context.Background(), propagation.HeaderCarrier(req.Header))
	if !trace.SpanContextFromContext(got).IsValid() {
		t.Fatal("extracted span context invalid")
	}
}
