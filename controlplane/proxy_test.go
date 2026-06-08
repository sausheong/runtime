package controlplane

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestReverseProxy_503OnDeadBackend verifies the ErrorHandler returns 503
// (not the default 502) when the backend agent can't be dialed — and that
// SSE-friendly immediate flushing stays enabled.
func TestReverseProxy_503OnDeadBackend(t *testing.T) {
	// 127.0.0.1:1 is a reserved port nothing listens on → dial fails.
	rp := reverseProxy("127.0.0.1:1")
	if rp.FlushInterval != -1 {
		t.Fatalf("FlushInterval = %v, want -1 (immediate flush for SSE)", rp.FlushInterval)
	}

	rec := httptest.NewRecorder()
	rp.ServeHTTP(rec, httptest.NewRequest("GET", "/sessions", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("dead backend: code = %d, want 503", rec.Code)
	}
}
