package console

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
)

// csrf issues and verifies synchronizer tokens bound to the session token via
// HMAC, with a per-process random key. Stateless (no storage); tokens are
// invalidated on process restart (acceptable — users re-load the form).
type csrf struct{ key []byte }

func newCSRF() *csrf {
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		panic("console: csrf key: " + err.Error())
	}
	return &csrf{key: k}
}

// issue returns the CSRF token for a session cookie value.
func (c *csrf) issue(session string) string {
	mac := hmac.New(sha256.New, c.key)
	mac.Write([]byte(session))
	return hex.EncodeToString(mac.Sum(nil))
}

// verify reports whether token matches the session (constant-time).
func (c *csrf) verify(session, token string) bool {
	if token == "" {
		return false
	}
	want, err := hex.DecodeString(token)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, c.key)
	mac.Write([]byte(session))
	return hmac.Equal(want, mac.Sum(nil))
}
