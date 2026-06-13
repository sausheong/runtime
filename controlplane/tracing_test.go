package controlplane

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/sausheong/runtime/internal/obs"
)

// The reverse proxy must inject traceparent on the proxied request when a span
// is active (otelhttp transport), and the backend must observe it.
func TestReverseProxy_InjectsTraceparent(t *testing.T) {
	tp := sdktrace.NewTracerProvider()
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

	ctx, span := obs.StartSpan(context.Background(), "edge")
	defer span.End()

	rp := reverseProxy(backend.URL, "", nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/sessions", nil).WithContext(ctx)
	rp.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("proxy code = %d", rec.Code)
	}
	if gotTP == "" {
		t.Fatal("backend did not receive traceparent (otelhttp transport not wired)")
	}
}
