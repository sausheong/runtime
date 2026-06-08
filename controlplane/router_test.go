package controlplane

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sausheong/runtime/internal/config"
)

func TestRouter_DispatchAndList(t *testing.T) {
	backendA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "A:"+r.URL.Path)
	}))
	defer backendA.Close()
	backendB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "B:"+r.URL.Path)
	}))
	defer backendB.Close()

	addrOf := func(s string) string { return strings.TrimPrefix(s, "http://") }
	cfg := &config.Config{Agents: []config.AgentConfig{
		{ID: "a", Name: "A", Model: "m", ListenAddr: addrOf(backendA.URL)},
		{ID: "b", Name: "B", Model: "m", ListenAddr: addrOf(backendB.URL)},
	}}
	reg := NewRegistry(cfg, "/bin/agentd", "dsn")

	srv := httptest.NewServer(NewAPI(reg))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/agents/a/sessions")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "A:/sessions" {
		t.Fatalf("dispatch a = %q, want A:/sessions", body)
	}

	resp, _ = http.Get(srv.URL + "/agents/b/healthz")
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "B:/healthz" {
		t.Fatalf("dispatch b = %q, want B:/healthz", body)
	}

	resp, _ = http.Get(srv.URL + "/agents/zzz/sessions")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown agent status = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()

	resp, _ = http.Get(srv.URL + "/agents")
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"id":"a"`) || !strings.Contains(string(body), `"id":"b"`) {
		t.Fatalf("/agents list = %q", body)
	}

	resp, _ = http.Get(srv.URL + "/healthz")
	if resp.StatusCode != 200 {
		t.Fatalf("/healthz status = %d", resp.StatusCode)
	}
	resp.Body.Close()
}
