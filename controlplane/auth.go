package controlplane

import (
	"context"
	"net/http"
	"strings"
)

type ctxKey int

const tokenLabelKey ctxKey = 0

// TokenLabelFromContext returns the matched token's label, if the request was
// authenticated. ok is false in open mode or when unset.
func TokenLabelFromContext(ctx context.Context) (label string, ok bool) {
	v, ok := ctx.Value(tokenLabelKey).(string)
	return v, ok
}

// extractToken pulls a bearer token from the Authorization header, falling back
// to the runtime_token cookie (EventSource and plain browser navigations can't
// set headers).
func extractToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	if c, err := r.Cookie("runtime_token"); err == nil {
		return c.Value
	}
	return ""
}

// AuthMiddleware gates next with bearer-token auth. tokens maps token→label.
// When tokens is empty, auth is DISABLED (open mode) — every request passes.
// GET /healthz is always exempt so liveness probes work without a token.
func AuthMiddleware(next http.Handler, tokens map[string]string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(tokens) == 0 || r.URL.Path == "/healthz" ||
			r.URL.Path == "/ui/login" || strings.HasPrefix(r.URL.Path, "/ui/static/") {
			next.ServeHTTP(w, r)
			return
		}
		tok := extractToken(r)
		label, ok := tokens[tok]
		if !ok {
			if strings.HasPrefix(r.URL.Path, "/ui") {
				http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
				return
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), tokenLabelKey, label)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
