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
