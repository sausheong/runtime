package console

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sausheong/runtime/controlplane"
	"github.com/sausheong/runtime/internal/config"
)

func testReg(t *testing.T) *controlplane.Registry {
	t.Helper()
	cfg := &config.Config{Agents: []config.AgentConfig{
		{ID: "support", Name: "Support", Model: "test/scripted", ListenAddr: "127.0.0.1:9001", Tenant: "alpha"},
	}}
	return controlplane.NewRegistry(cfg, "/bin/agentd", "dsn")
}

func TestConsole_Overview(t *testing.T) {
	srv := httptest.NewServer(Handler(testReg(t), OIDCConfig{}))
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
	srv := httptest.NewServer(Handler(testReg(t), OIDCConfig{}))
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
	srv := httptest.NewServer(Handler(testReg(t), OIDCConfig{}))
	defer srv.Close()
	resp, _ := http.Get(srv.URL + "/ui/agents/nope")
	if resp.StatusCode != 404 {
		t.Fatalf("unknown agent code=%d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestConsole_LoginShowsPasteWhenOIDCDisabled(t *testing.T) {
	h := Handler(testReg(t), OIDCConfig{Enabled: false})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/ui/login", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "token") {
		t.Fatalf("paste login expected; code=%d", rec.Code)
	}
}

func TestConsole_LoginRedirectsToIdPWhenEnabled(t *testing.T) {
	h := Handler(testReg(t), OIDCConfig{
		Enabled:     true,
		AuthCodeURL: func(state string) string { return "https://idp.example/authorize?state=" + state },
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/ui/login", nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("oidc login: code=%d want 303", rec.Code)
	}
	if !strings.HasPrefix(rec.Header().Get("Location"), "https://idp.example/authorize") {
		t.Fatalf("redirect to %q", rec.Header().Get("Location"))
	}
}

func TestConsole_CallbackSetsCookie(t *testing.T) {
	h := Handler(testReg(t), OIDCConfig{
		Enabled:  true,
		Exchange: func(_ context.Context, code string) (string, error) { return "id-token-" + code, nil },
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/ui/callback?code=abc&state=s", nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("callback: code=%d want 303", rec.Code)
	}
	var found bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == "runtime_token" && c.Value == "id-token-abc" {
			found = true
		}
	}
	if !found {
		t.Fatal("callback did not set runtime_token cookie to the id token")
	}
}
