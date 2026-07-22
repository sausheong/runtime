package identity

import (
	"context"
	"testing"
)

func TestWithSessionRoundTrip(t *testing.T) {
	ctx := WithSession(context.Background(), "sess-123")
	if got := SessionFrom(ctx); got != "sess-123" {
		t.Fatalf("SessionFrom = %q, want sess-123", got)
	}
}

func TestSessionFromAbsent(t *testing.T) {
	if got := SessionFrom(context.Background()); got != "" {
		t.Fatalf("SessionFrom on bare ctx = %q, want empty", got)
	}
}

func TestWithSessionEmptyIsNoop(t *testing.T) {
	ctx := WithSession(context.Background(), "")
	if got := SessionFrom(ctx); got != "" {
		t.Fatalf("SessionFrom after empty WithSession = %q, want empty", got)
	}
}
