package controlplane

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
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

// fakeBroker implements SecretBroker for spawn-path tests.
type fakeBroker struct {
	secrets map[string]map[string]string // tenant -> name -> value
	err     error
}

func (f fakeBroker) SecretsFor(_ context.Context, tenant string) (map[string]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.secrets[tenant], nil
}

func TestBuildEnv_TenantSecretsShadowAfterRuntimeVars(t *testing.T) {
	ap := AgentProcess{
		AgentID: "a1", Addr: "127.0.0.1:9", PGDSN: "dsn", Kind: "", Tenant: "alpha",
		broker: fakeBroker{secrets: map[string]map[string]string{
			"alpha": {"OPENAI_API_KEY": "sk-alpha"},
		}},
	}
	env, err := ap.buildEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	idxRuntime := lastIndexWithPrefix(env, "RUNTIME_AGENT_ID=")
	idxSecret := lastIndexWithPrefix(env, "OPENAI_API_KEY=")
	if idxRuntime < 0 || idxSecret < 0 {
		t.Fatalf("missing vars: runtime=%d secret=%d env=%v", idxRuntime, idxSecret, env)
	}
	if idxSecret < idxRuntime {
		t.Fatalf("tenant secret must come after RUNTIME_* vars: secret@%d runtime@%d", idxSecret, idxRuntime)
	}
	if !slices.Contains(env, "OPENAI_API_KEY=sk-alpha") {
		t.Fatalf("tenant secret value missing: %v", env)
	}
}

func TestBuildEnv_NilBrokerMatchesLegacy(t *testing.T) {
	// Ensure the operator env doesn't leak a same-named var into the assertion:
	// the nil-broker path must inject nothing, regardless of inherited env.
	t.Setenv("OPENAI_API_KEY", "")
	os.Unsetenv("OPENAI_API_KEY")
	ap := AgentProcess{AgentID: "a1", Addr: "127.0.0.1:9", PGDSN: "dsn", Tenant: "alpha"}
	env, err := ap.buildEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if lastIndexWithPrefix(env, "OPENAI_API_KEY=") >= 0 {
		t.Fatal("nil broker must not inject secrets")
	}
	if lastIndexWithPrefix(env, "RUNTIME_AGENT_ID=a1") < 0 {
		t.Fatal("RUNTIME_AGENT_ID still expected with nil broker")
	}
}

func TestBuildEnv_BrokerErrorFailsClosed(t *testing.T) {
	ap := AgentProcess{AgentID: "a1", Tenant: "alpha", broker: fakeBroker{err: errBrokerTest}}
	if _, err := ap.buildEnv(context.Background()); err == nil {
		t.Fatal("buildEnv must return broker error (fail closed)")
	}
}

var errBrokerTest = errors.New("broker boom")

// lastIndexWithPrefix returns the index of the last env entry with the prefix.
func lastIndexWithPrefix(env []string, prefix string) int {
	idx := -1
	for i, e := range env {
		if strings.HasPrefix(e, prefix) {
			idx = i
		}
	}
	return idx
}
