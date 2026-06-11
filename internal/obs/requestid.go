package obs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

// HeaderRequestID is the correlation header accepted, forwarded, and echoed.
const HeaderRequestID = "X-Request-ID"

type ridKey struct{}

// RequestID accepts a valid inbound X-Request-ID or generates req-<128-bit
// hex>, stores it in the request context, sets it on the REQUEST headers (so
// the reverse proxy forwards it to the agent), and echoes it on the response.
//
// Mount OUTERMOST: the middleware mutates r.Header (to propagate the id
// through the reverse proxy), which is only safe when no enclosing handler
// observes the request first.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(HeaderRequestID)
		if !validRequestID(id) {
			var b [16]byte
			_, _ = rand.Read(b[:])
			id = "req-" + hex.EncodeToString(b[:])
		}
		r.Header.Set(HeaderRequestID, id)
		w.Header().Set(HeaderRequestID, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ridKey{}, id)))
	})
}

// validRequestID bounds untrusted inbound ids: correlation ids are
// operator-facing log tokens, so cap length and restrict to URL-safe
// characters; anything else is regenerated rather than rejected.
func validRequestID(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' ||
			c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

// RequestIDFromContext returns the id stored by RequestID, or "".
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(ridKey{}).(string)
	return id
}
