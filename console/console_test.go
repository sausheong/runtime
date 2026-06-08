package console

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sausheong/runtime/controlplane"
	"github.com/sausheong/runtime/internal/config"
)

func testReg() *controlplane.Registry {
	cfg := &config.Config{Agents: []config.AgentConfig{
		{ID: "support", Name: "Support", Model: "test/scripted", ListenAddr: "127.0.0.1:9001"},
	}}
	return controlplane.NewRegistry(cfg, "/bin/agentd", "dsn")
}

func TestConsole_Overview(t *testing.T) {
	srv := httptest.NewServer(Handler(testReg()))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/ui")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(body), "Support") {
		t.Fatalf("overview missing agent; code=%d body=%q", resp.StatusCode, body)
	}
}

func TestConsole_LoginPage(t *testing.T) {
	srv := httptest.NewServer(Handler(testReg()))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/ui/login")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("login page code=%d", resp.StatusCode)
	}
}

func TestConsole_UnknownAgent404(t *testing.T) {
	srv := httptest.NewServer(Handler(testReg()))
	defer srv.Close()
	resp, _ := http.Get(srv.URL + "/ui/agents/nope")
	if resp.StatusCode != 404 {
		t.Fatalf("unknown agent code=%d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}
