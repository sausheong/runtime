package agentkind

import (
	"net/http"

	"github.com/sausheong/runtime/internal/identity"
	"github.com/sausheong/runtime/internal/rheader"
)

// gatewayRoundTripper sets per-call platform headers on each outbound gateway
// MCP request from the request ctx: X-Runtime-Assertion (caller JWT for OBO)
// and X-Runtime-Session (session id for session-scoped sandbox/browser tools).
// No-op when the ctx carries neither (e.g. the SDK's background SSE GET on the
// connect ctx). It CLONES the request before mutating headers so it never
// mutates a caller-shared request.
type gatewayRoundTripper struct{ base http.RoundTripper }

func (t gatewayRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	jwt := identity.AssertionFrom(r.Context())
	sess := identity.SessionFrom(r.Context())
	if jwt == "" && sess == "" {
		return base.RoundTrip(r)
	}
	r = r.Clone(r.Context())
	if jwt != "" {
		r.Header.Set(rheader.Assertion, jwt)
	}
	if sess != "" {
		r.Header.Set(rheader.Session, sess)
	}
	return base.RoundTrip(r)
}
