package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sausheong/runtime/internal/obs"
)

func TestMetricsUseSeparateManagementHandler(t *testing.T) {
	cm := obs.NewControlMetrics()
	// Every control family is a *Vec; a fresh registry with zero series
	// gathers zero families and renders an empty body. Record one
	// observation so at least one runtime_* family is present.
	cm.AgentUp("x", 0, true)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized) // simulates identity middleware
	})
	metrics := obs.FanoutHandler(cm, func() []obs.ScrapeTarget { return nil })
	// A stand-in register mux: POST /register remains pre-identity on the public
	// listener, but /metrics must not be mounted there.
	regMux := http.NewServeMux()
	regMux.HandleFunc("POST /register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	public := mountRegistration(inner, regMux)

	rec := httptest.NewRecorder()
	metrics.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 {
		t.Fatalf("management /metrics status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "runtime_") {
		t.Fatalf("/metrics body missing control families:\n%s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	public.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 401 {
		t.Fatalf("public /metrics status = %d, want identity-chain response", rec.Code)
	}

	rec = httptest.NewRecorder()
	public.ServeHTTP(rec, httptest.NewRequest("POST", "/register", nil))
	if rec.Code != 202 {
		t.Fatalf("/register status = %d, want 202 (must bypass identity, served by regMux)", rec.Code)
	}

	rec = httptest.NewRecorder()
	public.ServeHTTP(rec, httptest.NewRequest("GET", "/agents", nil))
	if rec.Code != 401 {
		t.Fatalf("inner route status = %d, want 401 (everything else still goes through the chain)", rec.Code)
	}
}
