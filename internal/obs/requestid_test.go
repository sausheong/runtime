package obs

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestIDGeneratedWhenAbsent(t *testing.T) {
	var seen string
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestIDFromContext(r.Context())
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	if !strings.HasPrefix(seen, "req-") || len(seen) != 4+32 {
		t.Fatalf("generated id = %q, want req-<32 hex>", seen)
	}
	if got := rec.Header().Get(HeaderRequestID); got != seen {
		t.Fatalf("response header = %q, want %q (echoed)", got, seen)
	}
}

func TestRequestIDInboundHonored(t *testing.T) {
	var seen, fwd string
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestIDFromContext(r.Context())
		fwd = r.Header.Get(HeaderRequestID)
	}))
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set(HeaderRequestID, "req-abc123")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if seen != "req-abc123" {
		t.Fatalf("ctx id = %q, want inbound honored", seen)
	}
	if fwd != "req-abc123" {
		t.Fatalf("request header = %q, want set for proxy forwarding", fwd)
	}
	if rec.Header().Get(HeaderRequestID) != "req-abc123" {
		t.Fatal("response header not echoed")
	}
}

func TestRequestIDUnique(t *testing.T) {
	ids := map[string]bool{}
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ids[RequestIDFromContext(r.Context())] = true
	}))
	for i := 0; i < 50; i++ {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil))
	}
	if len(ids) != 50 {
		t.Fatalf("got %d unique ids from 50 requests", len(ids))
	}
}

func TestRequestIDFromContextEmptyWithout(t *testing.T) {
	if got := RequestIDFromContext(httptest.NewRequest("GET", "/", nil).Context()); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}
