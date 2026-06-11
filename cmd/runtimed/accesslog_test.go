package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sausheong/runtime/internal/obs"
)

func TestAccessLogRouteNormalization(t *testing.T) {
	// Mirror production topology (buildRoot): a root mux delegating "/" to an
	// inner mux. r.Pattern must reflect the INNER mux's matched pattern (the
	// inner ServeMux overwrites the root's "/" pattern), or every request
	// would collapse into a single "/" series.
	cm := obs.NewControlMetrics()
	root := http.NewServeMux()
	inner := http.NewServeMux()
	inner.HandleFunc("GET /agents/{id}/sessions", func(w http.ResponseWriter, r *http.Request) {})
	root.Handle("/", inner)
	h := accessLog(root, cm)
	for _, p := range []string{"/agents/support/sessions", "/agents/research/sessions"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", p, nil))
	}
	body := scrapeControl(t, cm)
	if !strings.Contains(body, `runtime_http_requests_total{method="GET",route="/agents/{id}/sessions",status="200"} 2`) {
		t.Fatalf("normalized route series missing or split:\n%s", body)
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/no/such/route", nil))
	body = scrapeControl(t, cm)
	if !strings.Contains(body, `route="unmatched"`) {
		t.Fatalf("unmatched bucket missing:\n%s", body)
	}
	if strings.Contains(body, "/no/such/route") {
		t.Fatalf("raw path leaked into labels:\n%s", body)
	}
}

func scrapeControl(t *testing.T, cm *obs.ControlMetrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	obs.FanoutHandler(cm, func() []obs.ScrapeTarget { return nil }).
		ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Body.String()
}
