package controlplane

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestReverseProxy_503OnDeadBackend verifies the ErrorHandler returns 503
// (not the default 502) when the backend agent can't be dialed — and that
// SSE-friendly immediate flushing stays enabled.
func TestReverseProxy_503OnDeadBackend(t *testing.T) {
	// 127.0.0.1:1 is a reserved port nothing listens on → dial fails.
	rp := reverseProxy("127.0.0.1:1")
	if rp.FlushInterval != -1 {
		t.Fatalf("FlushInterval = %v, want -1 (immediate flush for SSE)", rp.FlushInterval)
	}

	rec := httptest.NewRecorder()
	rp.ServeHTTP(rec, httptest.NewRequest("GET", "/sessions", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("dead backend: code = %d, want 503", rec.Code)
	}
}

func TestSpawnFuncCommand(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out.txt")
	ap := AgentProcess{
		AgentID: "x",
		Addr:    "127.0.0.1:0",
		Command: []string{"sh", "-c", "pwd > " + out + "; printf '%s' \"$RUNTIME_AGENT_ID\" >> " + out + "; sleep 0.3"},
		WorkDir: dir,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wait := ap.SpawnFunc()(ctx)
	select {
	case <-wait:
	case <-time.After(5 * time.Second):
		t.Fatal("command did not exit in time")
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read out: %v", err)
	}
	got := string(b)
	wantDir, _ := filepath.EvalSymlinks(dir)
	if !strings.Contains(got, dir) && !strings.Contains(got, wantDir) {
		t.Errorf("cwd not applied: out=%q want contains %q", got, dir)
	}
	if !strings.Contains(got, "x") {
		t.Errorf("RUNTIME_AGENT_ID not in env: out=%q", got)
	}
}
