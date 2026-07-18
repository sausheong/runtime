package conformance

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func goodAgent() http.Handler { return agentWithSessionShape(true) }

func agentWithSessionShape(valid bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("GET /meta", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"agent_id": "a", "contract_version": "v1"})
	})
	mux.HandleFunc("POST /sessions", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"session_id": "ses-1"})
	})
	mux.HandleFunc("GET /sessions", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{{"id": "ses-1", "status": "completed", "turn_count": 1}})
	})
	mux.HandleFunc("GET /sessions/{id}", func(w http.ResponseWriter, _ *http.Request) {
		if !valid {
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "finished"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "ses-1", "status": "completed", "turn_count": 1})
	})
	mux.HandleFunc("GET /sessions/{id}/stream", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("id: 1\ndata: {\"type\":\"text\",\"text\":\"hi\"}\n\n"))
		_, _ = w.Write([]byte("id: 2\ndata: {\"type\":\"done\"}\n\n"))
	})
	return mux
}

func TestValidStatusIncludesLimitExceeded(t *testing.T) {
	if !validStatus("limit_exceeded") {
		t.Error("limit_exceeded must be a valid contract status")
	}
}

func TestRun_RejectsIncompleteSessionShape(t *testing.T) {
	srv := httptest.NewServer(agentWithSessionShape(false))
	defer srv.Close()
	rec := &recorder{}
	Run(rec, srv.URL)
	if rec.fails == 0 {
		t.Fatal("invalid session response should fail conformance")
	}
}

type recorder struct{ fails int }

func (r *recorder) Errorf(string, ...any) { r.fails++ }
func (r *recorder) Fatalf(string, ...any) { r.fails++ }
func (r *recorder) Logf(string, ...any)   {}

func TestRun_GoodAgentPasses(t *testing.T) {
	srv := httptest.NewServer(goodAgent())
	defer srv.Close()
	rec := &recorder{}
	Run(rec, srv.URL)
	if rec.fails != 0 {
		t.Fatalf("good agent should pass; got %d failures", rec.fails)
	}
}

func TestRun_BrokenAgentFails(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("GET /meta", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"agent_id":"a"}`)) // missing contract_version
	})
	// no /sessions endpoints at all
	srv := httptest.NewServer(mux)
	defer srv.Close()
	rec := &recorder{}
	Run(rec, srv.URL)
	if rec.fails == 0 {
		t.Fatal("broken agent should fail conformance")
	}
}
