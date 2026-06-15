package console

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// POST /ui/logout must clear the runtime_token cookie and redirect to login.
func TestLogoutClearsCookieAndRedirects(t *testing.T) {
	h, _ := newTestConsole()
	r := httptest.NewRequest("POST", "/ui/logout", nil)
	r.AddCookie(&http.Cookie{Name: "runtime_token", Value: "sess-1"})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("logout: want 303 got %d", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/ui/login" {
		t.Fatalf("logout: want redirect to /ui/login, got %q", loc)
	}
	// The response must expire the session cookie (MaxAge<0 / empty value).
	var cleared bool
	for _, c := range w.Result().Cookies() {
		if c.Name == "runtime_token" && c.Value == "" && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("logout did not expire the runtime_token cookie")
	}
}
