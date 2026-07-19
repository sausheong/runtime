package memory

import "context"

type actorCtxKey struct{}

// WithActor attaches the caller's actor (subject) to ctx so the tenant-pinned
// Store can scope reads/writes to (tenant, actor_id=actor). An empty actor
// returns ctx unchanged — actorFrom then yields "", the tenant-wide bucket.
func WithActor(ctx context.Context, actor string) context.Context {
	if actor == "" {
		return ctx
	}
	return context.WithValue(ctx, actorCtxKey{}, actor)
}

// actorFrom returns the actor on ctx, or "" (the tenant-wide bucket).
func actorFrom(ctx context.Context) string {
	a, _ := ctx.Value(actorCtxKey{}).(string)
	return a
}
