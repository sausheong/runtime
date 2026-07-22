package agentkind

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sausheong/runtime/internal/identity"
	"github.com/sausheong/runtime/internal/rheader"
)

func TestGatewayRoundTripperSetsBothHeaders(t *testing.T) {
	var gotAssertion, gotSession string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAssertion = r.Header.Get(rheader.Assertion)
		gotSession = r.Header.Get(rheader.Session)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	rt := gatewayRoundTripper{base: http.DefaultTransport}
	req, _ := http.NewRequest("GET", srv.URL, nil)
	ctx := identity.WithAssertion(req.Context(), "jwt-abc")
	ctx = identity.WithSession(ctx, "sess-xyz")
	resp, err := rt.RoundTrip(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if gotAssertion != "jwt-abc" {
		t.Errorf("assertion header = %q, want jwt-abc", gotAssertion)
	}
	if gotSession != "sess-xyz" {
		t.Errorf("session header = %q, want sess-xyz", gotSession)
	}
}

func TestGatewayRoundTripperNoHeadersWhenCtxBare(t *testing.T) {
	var gotAssertion, gotSession string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAssertion = r.Header.Get(rheader.Assertion)
		gotSession = r.Header.Get(rheader.Session)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	rt := gatewayRoundTripper{base: http.DefaultTransport}
	req, _ := http.NewRequest("GET", srv.URL, nil)
	resp, err := rt.RoundTrip(req) // bare ctx
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if gotAssertion != "" || gotSession != "" {
		t.Errorf("headers set on bare ctx: assertion=%q session=%q", gotAssertion, gotSession)
	}
}
