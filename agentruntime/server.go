package agentruntime

import (
	"encoding/json"
	"net/http"
	"strconv"
)

func (m *Manager) newMux() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /meta", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"agent_id": m.agentID, "contract_version": "v1",
		})
	})
	mux.HandleFunc("POST /sessions", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Message string `json:"message"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		id, err := m.startSession(r.Context(), body.Message)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"session_id": id})
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
