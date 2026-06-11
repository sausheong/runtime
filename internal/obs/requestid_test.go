package obs

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestIDGeneratedWhenAbsent(t *testing.T) {
	var seen, fwd string
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestIDFromContext(r.Context())
		fwd = r.Header.Get(HeaderRequestID)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	if !strings.HasPrefix(seen, "req-") || len(seen) != 4+32 {
		t.Fatalf("generated id = %q, want req-<32 hex>", seen)
	}
	if got := rec.Header().Get(HeaderRequestID); got != seen {
		t.Fatalf("response header = %q, want %q (echoed)", got, seen)
	}
	if fwd != seen {
		t.Fatalf("request header = %q, want %q (forwarded for proxy)", fwd, seen)
	}
}

func TestRequestIDValidation(t *testing.T) {
	cases := []struct {
		name, inbound string
		honored       bool
	}{
		{"valid uuid-ish", "550e8400-e29b-41d4-a716-446655440000", true},
		{"valid req hex", "req-0123456789abcdef0123456789abcdef", true},
		{"max length ok", strings.Repeat("a", 128), true},
		{"too long", strings.Repeat("a", 129), false},
		{"space", "abc def", false},
		{"high bytes", "abc\xffdef", false},
		{"semicolon", "abc;def", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seen string
			h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = RequestIDFromContext(r.Context())
			}))
			req := httptest.NewRequest("GET", "/x", nil)
			req.Header.Set(HeaderRequestID, tc.inbound)
			h.ServeHTTP(httptest.NewRecorder(), req)
			if tc.honored && seen != tc.inbound {
				t.Fatalf("id = %q, want inbound %q honored", seen, tc.inbound)
			}
			if !tc.honored && (seen == tc.inbound || !strings.HasPrefix(seen, "req-")) {
				t.Fatalf("id = %q, want regenerated (inbound %q rejected)", seen, tc.inbound)
			}
		})
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
