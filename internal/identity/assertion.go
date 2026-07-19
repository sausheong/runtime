package identity

import "context"

type assertionCtxKey struct{}

// WithAssertion attaches the caller's raw verified OIDC JWT to ctx so it can be
// forwarded to the gateway as the OBO subject_token. It is a bearer secret:
// request-scoped only, never logged, never persisted. Empty jwt ⇒ ctx unchanged.
func WithAssertion(ctx context.Context, jwt string) context.Context {
	if jwt == "" {
		return ctx
	}
	return context.WithValue(ctx, assertionCtxKey{}, jwt)
}

// AssertionFrom returns the caller JWT on ctx, or "".
func AssertionFrom(ctx context.Context) string {
	s, _ := ctx.Value(assertionCtxKey{}).(string)
	return s
}
