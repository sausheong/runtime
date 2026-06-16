package console

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
)

// OAuth login-CSRF protection via the `state` parameter (double-submit cookie).
//
// At /ui/login we mint a random state, embed it in the IdP authorize URL, and
// also set it in a short-lived cookie. The IdP echoes the state back to
// /ui/callback; we require the query `state` to equal the cookie. An attacker
// who forges a callback (to log a victim into the attacker's account, or to
// replay a stolen code) cannot also set this cookie in the victim's browser, so
// the mismatch is rejected.
//
// The cookie is scoped to the callback path and SameSite=Lax — Lax is required
// (not Strict): the callback is a cross-site top-level redirect from the IdP, and
// Strict would suppress the cookie there, breaking every login.
const oauthStateCookie = "rt_oauth_state"

// newOAuthState returns a fresh random state token (256 bits, hex).
func newOAuthState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func setOAuthStateCookie(w http.ResponseWriter, state string) {
	http.SetCookie(w, &http.Cookie{
		Name: oauthStateCookie, Value: state,
		Path: "/ui/callback", HttpOnly: true, SameSite: http.SameSiteLaxMode,
		MaxAge: 300, // 5 min: a login round-trip is seconds; bounds replay window.
	})
}

func clearOAuthStateCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: oauthStateCookie, Value: "",
		Path: "/ui/callback", HttpOnly: true, SameSite: http.SameSiteLaxMode,
		MaxAge: -1,
	})
}

// validOAuthState reports whether the ?state= query parameter matches the
// rt_oauth_state cookie (constant-time). Both must be present and non-empty.
func validOAuthState(r *http.Request) bool {
	q := r.URL.Query().Get("state")
	if q == "" {
		return false
	}
	c, err := r.Cookie(oauthStateCookie)
	if err != nil || c.Value == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(q), []byte(c.Value)) == 1
}
