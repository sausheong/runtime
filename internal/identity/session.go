package identity

import "context"

type sessionCtxKey struct{}

// WithSession attaches the agent session id to ctx so it can be forwarded to the
// gateway as X-Runtime-Session and used by session-scoped sandbox/browser tools.
// It is NOT a secret and NOT verified — it only selects an isolation bucket.
// Empty sessionID ⇒ ctx unchanged (so tenant-scoped callers forward nothing).
func WithSession(ctx context.Context, sessionID string) context.Context {
	if sessionID == "" {
		return ctx
	}
	return context.WithValue(ctx, sessionCtxKey{}, sessionID)
}

// SessionFrom returns the session id on ctx, or "".
func SessionFrom(ctx context.Context) string {
	s, _ := ctx.Value(sessionCtxKey{}).(string)
	return s
}
