package gateway

import (
	"context"
	"encoding/json"
)

type sessionForwardCtxKey struct{}

// WithSessionForward lands the forwarded X-Runtime-Session value on ctx for the
// injection point. Unlike the caller assertion, the session id is not a secret
// and needs no re-verification — it only selects a sandbox/browser isolation
// bucket, so the gateway forwards it as received.
func WithSessionForward(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, sessionForwardCtxKey{}, sessionID)
}

// SessionForwardFrom returns the forwarded session id, ok=false when absent.
func SessionForwardFrom(ctx context.Context) (string, bool) {
	s, ok := ctx.Value(sessionForwardCtxKey{}).(string)
	return s, ok
}

// injectSession strips any caller-supplied __rt_session from raw JSON arguments
// and sets the platform-forwarded value, so the agent can never choose its own
// session bucket. Mirrors injectTenant (server.go).
func injectSession(raw json.RawMessage, sessionID string) (json.RawMessage, error) {
	m := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
	}
	if m == nil { // legal `null` payload unmarshals to a nil map
		m = map[string]any{}
	}
	m["__rt_session"] = sessionID
	return json.Marshal(m)
}
