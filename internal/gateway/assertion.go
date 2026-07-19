package gateway

import "context"

type callerAssertionCtxKey struct{}

type callerAssertion struct{ subject, jwt string }

// WithCallerAssertion lands a re-verified, tenant-bound caller identity + raw JWT
// on ctx for the OBO dispatch point (gate #5). Only the gateway sets this, after
// re-verification — it is never derived from an unverified header.
func WithCallerAssertion(ctx context.Context, subject, jwt string) context.Context {
	return context.WithValue(ctx, callerAssertionCtxKey{}, callerAssertion{subject, jwt})
}

// CallerAssertionFrom returns the verified caller subject + raw JWT, ok=false when absent.
func CallerAssertionFrom(ctx context.Context) (subject, jwt string, ok bool) {
	a, ok := ctx.Value(callerAssertionCtxKey{}).(callerAssertion)
	return a.subject, a.jwt, ok
}
