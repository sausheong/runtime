package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sausheong/runtime/internal/obs"
)

func TestMountMetricsBypassesInnerHandler(t *testing.T) {
	cm := obs.NewControlMetrics()
	// Every control family is a *Vec; a fresh registry with zero series
	// gathers zero families and renders an empty body. Record one
	// observation so at least one runtime_* family is present.
	cm.AgentUp("x", 0, true)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized) // simulates identity middleware
	})
	// A stand-in register mux: proves POST /register is served by the outer mux,
	// pre-identity, rather than falling through to the 401 inner handler.
	regMux := http.NewServeMux()
	regMux.HandleFunc("POST /register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	h := mountMetrics(inner, cm, func() []obs.ScrapeTarget { return nil }, regMux)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 {
		t.Fatalf("/metrics status = %d, want 200 (must bypass identity)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "runtime_") {
		t.Fatalf("/metrics body missing control families:\n%s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/register", nil))
	if rec.Code != 202 {
		t.Fatalf("/register status = %d, want 202 (must bypass identity, served by regMux)", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/agents", nil))
	if rec.Code != 401 {
		t.Fatalf("inner route status = %d, want 401 (everything else still goes through the chain)", rec.Code)
	}
}
