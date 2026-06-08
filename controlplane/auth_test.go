package controlplane

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		label, _ := TokenLabelFromContext(r.Context())
		w.Header().Set("X-Token-Label", label)
		w.WriteHeader(200)
	})
}

func TestAuth_OpenWhenNoTokens(t *testing.T) {
	h := AuthMiddleware(okHandler(), map[string]string{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/agents", nil))
	if rec.Code != 200 {
		t.Fatalf("open mode: code = %d, want 200", rec.Code)
	}
}

func TestAuth_ValidHeaderToken(t *testing.T) {
	h := AuthMiddleware(okHandler(), map[string]string{"abc": "ci"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/agents", nil)
	req.Header.Set("Authorization", "Bearer abc")
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("valid token: code = %d, want 200", rec.Code)
	}
	if rec.Header().Get("X-Token-Label") != "ci" {
		t.Fatalf("label not propagated: %q", rec.Header().Get("X-Token-Label"))
	}
}

func TestAuth_ValidCookieToken(t *testing.T) {
	h := AuthMiddleware(okHandler(), map[string]string{"abc": "ci"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/ui", nil)
	req.AddCookie(&http.Cookie{Name: "runtime_token", Value: "abc"})
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("cookie token: code = %d, want 200", rec.Code)
	}
}

func TestAuth_MissingAndInvalid(t *testing.T) {
	h := AuthMiddleware(okHandler(), map[string]string{"abc": "ci"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/agents", nil))
	if rec.Code != 401 {
		t.Fatalf("missing token: code = %d, want 401", rec.Code)
	}
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/agents", nil)
	req.Header.Set("Authorization", "Bearer nope")
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("invalid token: code = %d, want 401", rec.Code)
	}
}

func TestAuth_HealthzExempt(t *testing.T) {
	h := AuthMiddleware(okHandler(), map[string]string{"abc": "ci"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Fatalf("healthz must be exempt: code = %d", rec.Code)
	}
}
