package agentruntime

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/sausheong/runtime/internal/obs"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// maxSessionBodyBytes bounds the only agent-contract request that can carry
// inline binary data. Sixteen MiB leaves room for a roughly 12 MiB source image
// after base64 expansion while preventing an unauthenticated/local agent port
// from buffering an arbitrarily large JSON body.
const maxSessionBodyBytes int64 = 16 << 20

// handler is the full agentd HTTP stack, outermost to innermost:
// RequestID (mutates r.Header, so nothing may observe the request first) →
// requireBearer (only when an auth token is set; a 401 short-circuits before
// any span) → otelhttp server span (continues an inbound traceparent, named by
// matched route) → access log (skips the probe paths /healthz and /metrics) →
// the route mux.
func (m *Manager) handler() http.Handler {
	mux := m.newMux()
	logged := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" && r.URL.Path != "/metrics" {
			slog.Info("http",
				"method", r.Method,
				"path", r.URL.Path,
				"request_id", obs.RequestIDFromContext(r.Context()))
		}
		mux.ServeHTTP(w, r)
	})
	var h http.Handler = logged
	// otelhttp server span: continues an inbound traceparent (parent) so the
	// agentd work nests under runtimed's trace. Named by route, not raw path.
	h = otelhttp.NewHandler(h, "agentd.request",
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			if r.Pattern != "" {
				return r.Method + " " + r.Pattern
			}
			return r.Method
		}),
	)
	if m.authToken != "" {
		h = requireBearer(m.authToken, h)
	}
	return obs.RequestID(h)
}

// requireBearer rejects any request whose Authorization header is not exactly
// "Bearer <token>" with 401, using a constant-time compare. It guards every
// path EXCEPT "GET /healthz", which is exempt: K8s liveness/readiness probes hit
// it with no Authorization header, and it returns a static "ok" with zero data
// (an unauthenticated handler also harmlessly ignores any bearer runtimed's own
// C3 M1 health checks still send). /metrics is NOT exempt — it exposes per-agent
// metric values, so it stays guarded.
func requireBearer(token string, next http.Handler) http.Handler {
	want := "Bearer " + token
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		got := r.Header.Get("Authorization")
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (m *Manager) newMux() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.Handle("GET /metrics", m.metrics.Handler())
	mux.HandleFunc("GET /meta", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"agent_id": m.agentID, "contract_version": "v1",
		})
	})
	mux.HandleFunc("POST /sessions", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxSessionBodyBytes)
		var body struct {
			Message   string `json:"message"`
			ImageB64  string `json:"image_b64"`
			ImageMime string `json:"image_mime"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		id, err := m.startSession(r.Context(), body.Message, body.ImageB64, body.ImageMime, obs.RequestIDFromContext(r.Context()))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"session_id": id})
	})
	mux.HandleFunc("GET /sessions", func(w http.ResponseWriter, r *http.Request) {
		rows, err := m.st.ListSessions(r.Context(), m.agentID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		type sessOut struct {
			ID        string `json:"id"`
			Status    string `json:"status"`
			TurnCount int    `json:"turn_count"`
		}
		out := make([]sessOut, 0, len(rows))
		for _, s := range rows {
			out = append(out, sessOut{ID: s.ID, Status: s.Status, TurnCount: s.TurnCount})
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("GET /sessions/{id}/stream", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		var since int64
		if s := r.URL.Query().Get("since"); s != "" {
			since, _ = strconv.ParseInt(s, 10, 64)
		}

		live, unsub := m.subscribe(id)
		defer unsub()

		buffered, err := m.st.EventsSince(r.Context(), id, since)
		if err == nil {
			for _, e := range buffered {
				var ev WireEvent
				if json.Unmarshal(e.Payload, &ev) == nil {
					ev.Seq = e.Seq
					_ = writeSSE(w, ev)
				}
			}
			flusher.Flush()
			if n := len(buffered); n > 0 && (buffered[n-1].Type == "done" || buffered[n-1].Type == "error") {
				return // pure-replay terminal: stream already complete
			}
		}

		for {
			select {
			case <-r.Context().Done():
				return
			case ev := <-live:
				_ = writeSSE(w, ev)
				flusher.Flush()
				if ev.Type == "done" || ev.Type == "error" {
					return
				}
			}
		}
	})
	mux.HandleFunc("GET /sessions/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		var since int64
		if s := r.URL.Query().Get("since"); s != "" {
			since, _ = strconv.ParseInt(s, 10, 64)
		}
		limit := 50
		if s := r.URL.Query().Get("limit"); s != "" {
			if n, err := strconv.Atoi(s); err == nil && n > 0 {
				limit = n
			}
		}
		// Non-blocking, unlike the SSE stream: a pure read of stored events. A
		// non-terminal session returns whatever is buffered so far and returns
		// immediately (no subscribe).
		evs, err := m.st.EventsSince(r.Context(), id, since)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if len(evs) > limit {
			evs = evs[len(evs)-limit:] // events are seq-ascending; keep the most recent
		}
		type evOut struct {
			Seq  int64  `json:"seq"`
			Type string `json:"type"`
			Text string `json:"text,omitempty"`
			Err  string `json:"error,omitempty"`
		}
		out := make([]evOut, 0, len(evs))
		for _, e := range evs {
			var ev WireEvent
			_ = json.Unmarshal(e.Payload, &ev) // bad/empty payload → empty fields, never a 500
			out = append(out, evOut{Seq: e.Seq, Type: ev.Type, Text: ev.Text, Err: ev.Err})
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("GET /sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		row, err := m.st.GetSession(r.Context(), r.PathValue("id"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": row.ID, "status": row.Status, "turn_count": row.TurnCount,
		})
	})
	return mux
}
