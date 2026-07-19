package memory

import (
	"context"
	"testing"
)

func TestWithActor_RoundTrip(t *testing.T) {
	ctx := context.Background()
	if got := actorFrom(ctx); got != "" {
		t.Fatalf("empty ctx actor = %q, want ''", got)
	}
	ctx = WithActor(ctx, "alice")
	if got := actorFrom(ctx); got != "alice" {
		t.Fatalf("actor = %q, want alice", got)
	}
	// Empty actor is a no-op wrapper (keeps '' semantics uniform).
	if got := actorFrom(WithActor(context.Background(), "")); got != "" {
		t.Fatalf("empty WithActor = %q, want ''", got)
	}
}
