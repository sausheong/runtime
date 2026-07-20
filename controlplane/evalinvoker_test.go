package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newFakeInvoker builds an evalInvoker whose resolve seam points at base with
// the given token, keeping the HTTP drain hermetic (no real agent).
func newFakeInvoker(base, token string) *evalInvoker {
	return &evalInvoker{
		client:  &http.Client{},
		timeout: 5 * time.Second,
		resolve: func(string) (string, string, bool) { return base, token, true },
	}
}

func TestEvalInvokerDrivesToDone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/sessions":
			_ = json.NewEncoder(w).Encode(map[string]string{"session_id": "s1"})
		case r.Method == http.MethodGet && r.URL.Path == "/sessions/s1/events":
			_, _ = w.Write([]byte(`[{"seq":1,"type":"text","text":"hel"},{"seq":2,"type":"text","text":"lo"},{"seq":3,"type":"done"}]`))
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	inv := newFakeInvoker(srv.URL, "")
	out, err := inv.Invoke(context.Background(), "agent", "hi")
	if err != nil {
		t.Fatalf("Invoke: unexpected error: %v", err)
	}
	if out != "hello" {
		t.Fatalf("Invoke output = %q, want %q", out, "hello")
	}
}

func TestEvalInvokerErrorEvent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/sessions":
			_ = json.NewEncoder(w).Encode(map[string]string{"session_id": "s1"})
		case r.Method == http.MethodGet && r.URL.Path == "/sessions/s1/events":
			_, _ = w.Write([]byte(`[{"seq":1,"type":"error","error":"boom"}]`))
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	inv := newFakeInvoker(srv.URL, "")
	_, err := inv.Invoke(context.Background(), "agent", "hi")
	if err == nil {
		t.Fatal("Invoke: want error from error event, got nil")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Invoke error = %v, want it to contain %q", err, "boom")
	}
}

func TestEvalInvokerBearer(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			gotAuth = r.Header.Get("Authorization")
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/sessions":
			_ = json.NewEncoder(w).Encode(map[string]string{"session_id": "s1"})
		case r.Method == http.MethodGet && r.URL.Path == "/sessions/s1/events":
			_, _ = w.Write([]byte(`[{"seq":1,"type":"text","text":"ok"},{"seq":2,"type":"done"}]`))
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	inv := newFakeInvoker(srv.URL, "tok123")
	out, err := inv.Invoke(context.Background(), "agent", "hi")
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if out != "ok" {
		t.Fatalf("out = %q", out)
	}
	if gotAuth != "Bearer tok123" {
		t.Fatalf("Authorization header = %q, want %q", gotAuth, "Bearer tok123")
	}
}

func TestEvalInvokerNoReplica(t *testing.T) {
	inv := &evalInvoker{
		client:  &http.Client{},
		timeout: time.Second,
		resolve: func(string) (string, string, bool) { return "", "", false },
	}
	_, err := inv.Invoke(context.Background(), "ghost", "hi")
	if err == nil || !strings.Contains(err.Error(), "no replica for agent ghost") {
		t.Fatalf("want 'no replica for agent ghost' error, got %v", err)
	}
}
