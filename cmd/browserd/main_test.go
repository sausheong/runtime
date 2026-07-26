package main

import (
	"regexp"
	"testing"

	"github.com/sausheong/runtime/internal/browser"
)

// TestRandomProxyTokenSatisfiesListenerFloor pins the contract between the
// generator and the validator: a self-minted token must be strong enough that
// ValidateProxyListener accepts a non-loopback bind. If either the token length
// or the validator's floor moves independently, browserd would fail to start on
// every private-network deployment.
func TestRandomProxyTokenSatisfiesListenerFloor(t *testing.T) {
	tok, err := randomProxyToken()
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(tok) {
		t.Fatalf("token = %q, want 64 hex characters", tok)
	}
	if err := browser.ValidateProxyListener("0.0.0.0:3128", tok); err != nil {
		t.Fatalf("generated token rejected for a non-loopback bind: %v", err)
	}
}

// TestRandomProxyTokenIsUnpredictable guards the obvious failure mode of a
// self-generated credential: a constant, a counter, or a seeded PRNG would all
// still satisfy the length floor while being trivially guessable by anything
// that can reach the proxy's port.
func TestRandomProxyTokenIsUnpredictable(t *testing.T) {
	seen := map[string]bool{}
	for range 64 {
		tok, err := randomProxyToken()
		if err != nil {
			t.Fatal(err)
		}
		if seen[tok] {
			t.Fatalf("randomProxyToken repeated a value: %q", tok)
		}
		seen[tok] = true
	}
}

// TestValidateProxyListenerStillRejectsWeakExplicitTokens is the security
// counterpart to the fallback. Generating a token when none is configured must
// not soften the rule for a token that IS configured: an operator who sets a
// weak value still gets a hard failure rather than a silent upgrade to a
// generated one.
func TestValidateProxyListenerStillRejectsWeakExplicitTokens(t *testing.T) {
	if err := browser.ValidateProxyListener("0.0.0.0:3128", "short"); err == nil {
		t.Fatal("weak explicit token accepted for a non-loopback bind")
	}
	if err := browser.ValidateProxyListener("127.0.0.1:0", ""); err != nil {
		t.Fatalf("loopback bind without a token rejected: %v", err)
	}
}
