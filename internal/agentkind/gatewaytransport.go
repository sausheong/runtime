package agentkind

import (
	"net/http"

	"github.com/sausheong/runtime/internal/identity"
	"github.com/sausheong/runtime/internal/rheader"
)

// assertionRoundTripper sets X-Runtime-Assertion from the per-call ctx's caller
// JWT (identity.AssertionFrom) on each outbound gateway MCP request, then
// delegates to base. No-op when the ctx carries no JWT (e.g. the SDK's
// background SSE GET on the connect ctx). It CLONES the request before mutating
// headers so it never mutates a caller-shared request.
type assertionRoundTripper struct{ base http.RoundTripper }

func (t assertionRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	if jwt := identity.AssertionFrom(r.Context()); jwt != "" {
		r = r.Clone(r.Context())
		r.Header.Set(rheader.Assertion, jwt)
	}
	return base.RoundTrip(r)
}
