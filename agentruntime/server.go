package agentruntime

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/sausheong/runtime/internal/obs"
)

// handler is the full agentd HTTP stack: RequestID outermost (it mutates
// r.Header, so nothing may observe the request first), then an access log for
// every request except the probe-noise paths /healthz and /metrics, then the
// route mux.
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
	return obs.RequestID(logged)
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
		var body struct {
			Message   string `json:"message"`
			ImageB64  string `json:"image_b64"`
			ImageMime string `json:"image_mime"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
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
