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

// RequestID accepts an inbound X-Request-ID or generates req-<128-bit hex>,
// stores it in the request context, sets it on the REQUEST headers (so the
// reverse proxy forwards it to the agent), and echoes it on the response.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(HeaderRequestID)
		if id == "" {
			var b [16]byte
			_, _ = rand.Read(b[:])
			id = "req-" + hex.EncodeToString(b[:])
		}
		r.Header.Set(HeaderRequestID, id)
		w.Header().Set(HeaderRequestID, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ridKey{}, id)))
	})
}

// RequestIDFromContext returns the id stored by RequestID, or "".
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(ridKey{}).(string)
	return id
}
