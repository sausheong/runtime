package identity

import (
	"context"
	"testing"
)

func TestWithAssertion_RoundTrip(t *testing.T) {
	if got := AssertionFrom(context.Background()); got != "" {
		t.Fatalf("empty ctx = %q, want ''", got)
	}
	ctx := WithAssertion(context.Background(), "jwt.abc.def")
	if got := AssertionFrom(ctx); got != "jwt.abc.def" {
		t.Fatalf("got %q", got)
	}
	if got := AssertionFrom(WithAssertion(context.Background(), "")); got != "" {
		t.Fatalf("empty WithAssertion = %q, want ''", got)
	}
}
