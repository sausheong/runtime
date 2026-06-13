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
