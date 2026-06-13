package console

import "testing"

func TestCSRFTokenRoundTrip(t *testing.T) {
	c := newCSRF()
	session := "session-token-abc"
	tok := c.issue(session)
	if tok == "" {
		t.Fatal("empty token")
	}
	if !c.verify(session, tok) {
		t.Fatal("valid token rejected")
	}
	if c.verify(session, tok+"x") {
		t.Fatal("tampered token accepted")
	}
	if c.verify("different-session", tok) {
		t.Fatal("token accepted for wrong session")
	}
	if c.verify(session, "") {
		t.Fatal("empty token accepted")
	}
}

func TestCSRFDeterministicPerInstance(t *testing.T) {
	c := newCSRF()
	if c.issue("sess") != c.issue("sess") {
		t.Fatal("issue must be deterministic for the same session within one instance")
	}
}

func TestCSRFKeyIsPerInstance(t *testing.T) {
	a, b := newCSRF(), newCSRF()
	// Two instances have independent random keys, so a token from one must not
	// verify on the other (proves the key is actually random/per-instance).
	if b.verify("sess", a.issue("sess")) {
		t.Fatal("token from one instance must not verify on another (keys must differ)")
	}
}
