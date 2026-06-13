package obs

import (
	"context"
	"testing"
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
