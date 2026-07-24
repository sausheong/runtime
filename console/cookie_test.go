package console

import "testing"

func TestSessionCookieSecure(t *testing.T) {
	t.Setenv("RUNTIME_COOKIE_SECURE", "")
	t.Setenv("RUNTIME_PUBLIC_URL", "https://runtime.example")
	t.Setenv("RUNTIME_OIDC_REDIRECT_URL", "")
	if !sessionCookieSecure() {
		t.Fatal("https public URL should enable Secure cookies")
	}
	t.Setenv("RUNTIME_COOKIE_SECURE", "false")
	if sessionCookieSecure() {
		t.Fatal("explicit false should support local HTTP")
	}
	t.Setenv("RUNTIME_COOKIE_SECURE", "true")
	t.Setenv("RUNTIME_PUBLIC_URL", "http://localhost:8080")
	if !sessionCookieSecure() {
		t.Fatal("explicit true should enable Secure cookies")
	}
}
