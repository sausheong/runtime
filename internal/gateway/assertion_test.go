package gateway

import (
	"context"
	"testing"
)

func TestCallerAssertion_RoundTrip(t *testing.T) {
	// Absent ⇒ ok=false, empty values.
	if sub, jwt, ok := CallerAssertionFrom(context.Background()); ok || sub != "" || jwt != "" {
		t.Fatalf("empty ctx = (%q,%q,%v), want ('','',false)", sub, jwt, ok)
	}
	// Set ⇒ subject+jwt returned, ok=true.
	ctx := WithCallerAssertion(context.Background(), "alice", "jwt.abc.def")
	sub, jwt, ok := CallerAssertionFrom(ctx)
	if !ok || sub != "alice" || jwt != "jwt.abc.def" {
		t.Fatalf("got (%q,%q,%v), want ('alice','jwt.abc.def',true)", sub, jwt, ok)
	}
}
