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
// is not active it installs only the propagator (so an inbound traceparent is
// still honored) and returns a no-op shutdown — zero overhead, mirroring the
// nil-safe metrics posture. The returned shutdown flushes + closes the exporter.
func InitTracing(ctx context.Context, serviceName string) (func(context.Context) error, error) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	if !tracingActive() {
		tracingOn.Store(false)
		return func(context.Context) error { return nil }, nil
	}
	exp, err := otlptracehttp.New(ctx)
	if err != nil {
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
func AgentAttr(id string) attribute.KeyValue        { return attribute.String("agent.id", id) }
func TenantAttr(t string) attribute.KeyValue        { return attribute.String("tenant.id", t) }
func SessionAttr(s string) attribute.KeyValue       { return attribute.String("session.id", s) }
func RequestIDAttr(r string) attribute.KeyValue     { return attribute.String("request.id", r) }
func TurnAttr(n int) attribute.KeyValue             { return attribute.Int("turn.number", n) }
func ToolAttr(name string) attribute.KeyValue       { return attribute.String("tool.name", name) }
func OutcomeAttr(o string) attribute.KeyValue       { return attribute.String("outcome", o) }
func GatewayServerAttr(s string) attribute.KeyValue { return attribute.String("gateway.server", s) }
func GatewayToolAttr(t string) attribute.KeyValue   { return attribute.String("gateway.tool", t) }
