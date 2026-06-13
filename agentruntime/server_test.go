package agentruntime

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sausheong/runtime/internal/store"
)

func newTestManager() *Manager {
	return &Manager{
		agentID:     "a",
		st:          store.NewMemStore(),
		subscribers: map[string][]chan WireEvent{},
	}
}

func TestHealthzAndMeta(t *testing.T) {
	srv := httptest.NewServer(newTestManager().newMux())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("healthz: err=%v status=%v", err, resp.StatusCode)
	}
}

func TestListSessionsEndpoint(t *testing.T) {
	m := newTestManager()
	ctx := context.Background()
	id1, _ := m.st.CreateSession(ctx, "a")
	_ = m.st.SetSessionStatus(ctx, id1, "completed")
	_ = m.st.SetTurnCount(ctx, id1, 2)
	_, _ = m.st.CreateSession(ctx, "a")

	srv := httptest.NewServer(m.newMux())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), id1) || !strings.Contains(string(body), `"status":"completed"`) || !strings.Contains(string(body), `"turn_count":2`) {
		t.Fatalf("/sessions body = %q", body)
	}
}

func TestStreamReplaysBufferedTerminal(t *testing.T) {
	m := newTestManager()
	ctx := context.Background()
	id, _ := m.st.CreateSession(ctx, "a")
	_, _ = m.st.AppendEvent(ctx, id, "text", []byte(`{"type":"text","text":"a"}`))
	_, _ = m.st.AppendEvent(ctx, id, "done", []byte(`{"type":"done"}`))

	srv := httptest.NewServer(m.newMux())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/sessions/" + id + "/stream")
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body) // returns because replay hit terminal "done"
	s := string(body)
	if !strings.Contains(s, `"text":"a"`) || !strings.Contains(s, `"type":"done"`) {
		t.Fatalf("stream body missing replayed events: %q", s)
	}
	// Replayed events must carry an SSE id: line (the store-assigned seq) so
	// clients get Last-Event-ID dedupe/resume semantics (I1 wire-level seq).
	if !strings.Contains(s, "id: ") {
		t.Fatalf("stream body missing id: line on replay: %q", s)
	}
}

func TestRequireBearer(t *testing.T) {
	const token = "agent-tok"
	mkSrv := func(tok string) *httptest.Server {
		m := newTestManager()
		m.authToken = tok
		return httptest.NewServer(m.handler())
	}

	t.Run("no token configured: open (back-compat)", func(t *testing.T) {
		srv := mkSrv("")
		defer srv.Close()
		resp, err := http.Get(srv.URL + "/healthz")
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("healthz open: err=%v status=%v", err, resp.StatusCode)
		}
	})

	t.Run("token set: 401 without header, including /healthz and /metrics", func(t *testing.T) {
		srv := mkSrv(token)
		defer srv.Close()
		for _, path := range []string{"/healthz", "/metrics", "/sessions"} {
			resp, err := http.Get(srv.URL + path)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("%s without token: status=%d, want 401", path, resp.StatusCode)
			}
		}
	})

	t.Run("token set: 200 with correct bearer", func(t *testing.T) {
		srv := mkSrv(token)
		defer srv.Close()
		req, _ := http.NewRequest("GET", srv.URL+"/healthz", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("healthz with token: err=%v status=%v", err, resp.StatusCode)
		}
	})

	t.Run("token set: 401 with wrong bearer", func(t *testing.T) {
		srv := mkSrv(token)
		defer srv.Close()
		req, _ := http.NewRequest("GET", srv.URL+"/healthz", nil)
		req.Header.Set("Authorization", "Bearer wrong")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("healthz wrong token: status=%d, want 401", resp.StatusCode)
		}
	})
}
