package agentkind

import (
	"context"
	"net/http"
	"testing"

	"github.com/sausheong/runtime/internal/identity"
	"github.com/sausheong/runtime/internal/rheader"
)

// roundTripFunc adapts a func to an http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAssertionRoundTripper(t *testing.T) {
	var got http.Header
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		got = r.Header.Clone()
		return &http.Response{StatusCode: 200, Body: http.NoBody, Header: http.Header{}}, nil
	})
	rt := assertionRoundTripper{base: base}

	req, _ := http.NewRequestWithContext(identity.WithAssertion(context.Background(), "jwt.x"), "POST", "http://gw/", nil)
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if got.Get(rheader.Assertion) != "jwt.x" {
		t.Fatalf("assertion not set: %q", got.Get(rheader.Assertion))
	}

	req2, _ := http.NewRequest("POST", "http://gw/", nil)
	if _, err := rt.RoundTrip(req2); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if got.Get(rheader.Assertion) != "" {
		t.Fatalf("assertion leaked on bare ctx: %q", got.Get(rheader.Assertion))
	}
}
