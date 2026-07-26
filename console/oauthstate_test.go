package console

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewOAuthState_RandomAndHex(t *testing.T) {
	a, err := newOAuthState()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := newOAuthState()
	if a == b {
		t.Fatal("two states must differ")
	}
	if len(a) != 64 { // 32 bytes hex
		t.Fatalf("state len=%d want 64", len(a))
	}
}

// reqWithStateCookie builds a callback request carrying ?state=q and the
// rt_oauth_state cookie set to cookieVal.
func reqWithStateCookie(q, cookieVal string) *http.Request {
	r := httptest.NewRequest("GET", "/ui/callback?code=x&state="+q, nil)
	if cookieVal != "" {
		r.AddCookie(&http.Cookie{Name: oauthStateCookie, Value: cookieVal})
	}
	return r
}

func TestValidOAuthState(t *testing.T) {
	cases := []struct {
		name          string
		query, cookie string
		want          bool
	}{
		{"match", "abc123", "abc123", true},
		{"mismatch", "abc123", "different", false},
		{"missing cookie", "abc123", "", false},
		{"missing query", "", "abc123", false},
		{"both empty", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := validOAuthState(reqWithStateCookie(c.query, c.cookie)); got != c.want {
				t.Errorf("validOAuthState(q=%q,cookie=%q)=%v want %v", c.query, c.cookie, got, c.want)
			}
		})
	}
}

func TestSetOAuthStateCookie_Attributes(t *testing.T) {
	rec := httptest.NewRecorder()
	setOAuthStateCookie(rec, "s123")
	cs := rec.Result().Cookies()
	if len(cs) != 1 {
		t.Fatalf("want 1 cookie, got %d", len(cs))
	}
	c := cs[0]
	if c.Name != oauthStateCookie || c.Value != "s123" {
		t.Fatalf("name/value: %q=%q", c.Name, c.Value)
	}
	if c.Path != "/ui/callback" {
		t.Errorf("path=%q want /ui/callback", c.Path)
	}
	if !c.HttpOnly {
		t.Error("must be HttpOnly")
	}
	// Lax is required: Strict would not be sent on the cross-site IdP redirect.
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("samesite=%v want Lax", c.SameSite)
	}
	if c.MaxAge <= 0 {
		t.Errorf("maxage=%d want >0", c.MaxAge)
	}
}

// TestOAuthStateCookieSecureOnHTTPS pins the attribute whose absence broke
// login on the live HTTPS deployment. The state cookie is only ever returned on
// a cross-site top-level redirect back from the IdP; on an https origin a
// browser will not send a non-Secure cookie in that position, so /ui/callback
// saw no cookie and every sign-in failed with "invalid login state".
//
// It must track the other console cookies rather than hardcode true: on a plain
// http dev origin, forcing Secure would break login the other way.
func TestOAuthStateCookieSecureOnHTTPS(t *testing.T) {
	t.Setenv("RUNTIME_COOKIE_SECURE", "")
	t.Setenv("RUNTIME_PUBLIC_URL", "")
	t.Setenv("RUNTIME_OIDC_REDIRECT_URL", "https://runtime.example.com/ui/callback")

	for _, tc := range []struct {
		name string
		set  func(http.ResponseWriter)
	}{
		{"set", func(w http.ResponseWriter) { setOAuthStateCookie(w, "abc") }},
		{"clear", clearOAuthStateCookie},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			tc.set(w)
			c := w.Result().Cookies()[0]
			if !c.Secure {
				t.Fatalf("%s: cookie is not Secure on an https deployment; the browser will drop it on the IdP redirect and login fails", tc.name)
			}
			if c.SameSite != http.SameSiteLaxMode {
				t.Fatalf("%s: SameSite=%v, want Lax (Strict suppresses the cross-site callback)", tc.name, c.SameSite)
			}
		})
	}
}

// TestOAuthStateCookieNotSecureOnHTTP is the converse: a plain-http dev origin
// must NOT get Secure, or the cookie is dropped there instead.
func TestOAuthStateCookieNotSecureOnHTTP(t *testing.T) {
	t.Setenv("RUNTIME_COOKIE_SECURE", "")
	t.Setenv("RUNTIME_PUBLIC_URL", "http://localhost:8080")
	t.Setenv("RUNTIME_OIDC_REDIRECT_URL", "http://localhost:8080/ui/callback")

	w := httptest.NewRecorder()
	setOAuthStateCookie(w, "abc")
	if w.Result().Cookies()[0].Secure {
		t.Fatal("Secure set on a plain-http origin; the browser would drop the cookie")
	}
}
