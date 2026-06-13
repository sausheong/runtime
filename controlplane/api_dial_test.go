package controlplane

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sausheong/runtime/internal/config"
	"github.com/sausheong/runtime/internal/store"
)

// A remote agent behind a bearer: /agents must dial its base URL WITH the
// token and report it healthy; a proxied request must reach it.
func TestAPI_RemoteAgentHealthAndProxyUseBearer(t *testing.T) {
	const token = "abc123"
	var sawHealthAuth, sawProxyAuth string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/healthz":
			sawHealthAuth = r.Header.Get("Authorization")
			w.WriteHeader(http.StatusOK)
		default:
			sawProxyAuth = r.Header.Get("Authorization")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		}
	}))
	defer backend.Close()

	cfg := &config.Config{Agents: []config.AgentConfig{
		{ID: "r", Name: "R", Model: "m", URL: backend.URL, AuthToken: token},
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(cfg, "bin", "dsn")
	api := NewAPI(reg, nil, store.NewMemStore())

	// /agents reports healthy.
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, httptest.NewRequest("GET", "/agents", nil))
	var statuses []struct {
		ID      string `json:"id"`
		Healthy bool   `json:"healthy"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &statuses); err != nil {
		t.Fatalf("decode /agents: %v (%s)", err, rec.Body.String())
	}
	if len(statuses) != 1 || !statuses[0].Healthy {
		t.Fatalf("/agents = %+v, want one healthy", statuses)
	}
	if sawHealthAuth != "Bearer "+token {
		t.Fatalf("health check Authorization = %q", sawHealthAuth)
	}

	// Proxy a request through.
	rec = httptest.NewRecorder()
	api.ServeHTTP(rec, httptest.NewRequest("GET", "/agents/r/sessions", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ok") {
		t.Fatalf("proxy code=%d body=%q", rec.Code, rec.Body.String())
	}
	if sawProxyAuth != "Bearer "+token {
		t.Fatalf("proxy Authorization = %q", sawProxyAuth)
	}
}
