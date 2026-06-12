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
	rp := reverseProxy("http://127.0.0.1:1", "", nil)
	if rp.FlushInterval != -1 {
		t.Fatalf("FlushInterval = %v, want -1 (immediate flush for SSE)", rp.FlushInterval)
	}

	rec := httptest.NewRecorder()
	rp.ServeHTTP(rec, httptest.NewRequest("GET", "/sessions", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("dead backend: code = %d, want 503", rec.Code)
	}
}

// TestReverseProxy_DedupesRequestIDEcho verifies ModifyResponse strips the
// backend agent's X-Request-Id echo so the client sees exactly one value
// (runtimed's own echo) — ReverseProxy copies backend headers with Add
// semantics, which would otherwise duplicate the header.
func TestReverseProxy_DedupesRequestIDEcho(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// agentd echoes the forwarded id on its own response.
		w.Header().Set("X-Request-Id", "req-from-agent")
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	rp := reverseProxy(backend.URL, "", nil)
	outer := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// runtimed's middleware echoes the id before proxying.
		w.Header().Set("X-Request-Id", "req-from-runtimed")
		rp.ServeHTTP(w, r)
	})

	rec := httptest.NewRecorder()
	outer.ServeHTTP(rec, httptest.NewRequest("GET", "/sessions", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	got := rec.Header().Values("X-Request-Id")
	if len(got) != 1 {
		t.Fatalf("X-Request-Id values = %v, want exactly one", got)
	}
	if got[0] != "req-from-runtimed" {
		t.Fatalf("X-Request-Id = %q, want runtimed's echo to win", got[0])
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

func TestBuildEnv_InjectsTenantAndMemory(t *testing.T) {
	ap := AgentProcess{AgentID: "a1", Addr: "127.0.0.1:9111", PGDSN: "dsn", Tenant: "alpha", Memory: true}
	env, err := ap.buildEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "RUNTIME_AGENT_TENANT=alpha") {
		t.Fatalf("missing tenant in env:\n%s", joined)
	}
	if !strings.Contains(joined, "RUNTIME_AGENT_MEMORY=1") {
		t.Fatalf("missing memory flag in env:\n%s", joined)
	}
}

func TestBuildEnv_NoMemoryFlagWhenDisabled(t *testing.T) {
	ap := AgentProcess{AgentID: "a1", Addr: "127.0.0.1:9111", PGDSN: "dsn", Tenant: "alpha"}
	env, _ := ap.buildEnv(context.Background())
	// Memory off is shadowed with an explicit empty entry (exec.Cmd takes the
	// last duplicate; agentd requires "1") so an inherited operator var can't
	// enable the feature. Assert the LAST entry is the empty shadow.
	idx := lastIndexWithPrefix(env, "RUNTIME_AGENT_MEMORY=")
	if idx < 0 || env[idx] != "RUNTIME_AGENT_MEMORY=" {
		t.Fatalf("memory flag must be shadowed empty when disabled: idx=%d env=%v", idx, env)
	}
	if !strings.Contains(strings.Join(env, "\n"), "RUNTIME_AGENT_TENANT=alpha") {
		t.Fatalf("tenant must always be present:\n%s", strings.Join(env, "\n"))
	}
}

func TestBuildEnvGateway(t *testing.T) {
	t.Run("gateway on: url and key injected", func(t *testing.T) {
		a := AgentProcess{
			AgentID: "x", Addr: "127.0.0.1:1", BinPath: "bin", PGDSN: "dsn",
			Tenant: "acme", GatewayOn: true,
			GatewayURL: "http://127.0.0.1:8080/gateway/mcp",
			GatewayKey: "svk-test",
		}
		env, err := a.buildEnv(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		assertHasEnv(t, env, "RUNTIME_GATEWAY_URL=http://127.0.0.1:8080/gateway/mcp")
		assertHasEnv(t, env, "RUNTIME_GATEWAY_KEY=svk-test")
	})

	// The off/no-key paths inject explicit EMPTY-value entries rather than
	// omitting the vars: exec.Cmd uses the last duplicate env entry, so the
	// empty shadow neutralizes any same-named var inherited from the operator
	// environment (agentd treats empty as unset).
	t.Run("gateway on, no key (open mode): url set, key shadowed empty", func(t *testing.T) {
		a := AgentProcess{
			AgentID: "x", Addr: "127.0.0.1:1", BinPath: "bin", PGDSN: "dsn",
			Tenant: "default", GatewayOn: true,
			GatewayURL: "http://127.0.0.1:8080/gateway/mcp",
		}
		env, err := a.buildEnv(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		assertHasEnv(t, env, "RUNTIME_GATEWAY_URL=http://127.0.0.1:8080/gateway/mcp")
		assertLastEnvEmpty(t, env, "RUNTIME_GATEWAY_KEY")
	})

	t.Run("gateway off: both shadowed empty", func(t *testing.T) {
		a := AgentProcess{AgentID: "x", Addr: "127.0.0.1:1", BinPath: "bin", PGDSN: "dsn", Tenant: "t"}
		env, err := a.buildEnv(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		assertLastEnvEmpty(t, env, "RUNTIME_GATEWAY_URL")
		assertLastEnvEmpty(t, env, "RUNTIME_GATEWAY_KEY")
	})
}

// assertLastEnvEmpty asserts the LAST entry for name is the empty shadow
// "name=" — present (so it overrides any inherited value) and valueless.
func assertLastEnvEmpty(t *testing.T, env []string, name string) {
	t.Helper()
	idx := lastIndexWithPrefix(env, name+"=")
	if idx < 0 {
		t.Fatalf("env missing empty shadow %q=", name)
	}
	if env[idx] != name+"=" {
		t.Fatalf("last %s entry = %q, want empty shadow %q", name, env[idx], name+"=")
	}
}

func assertHasEnv(t *testing.T, env []string, want string) {
	t.Helper()
	for _, e := range env {
		if e == want {
			return
		}
	}
	t.Fatalf("env missing %q", want)
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

func TestBuildEnvGatewaySearchMode(t *testing.T) {
	a := AgentProcess{
		AgentID: "x", Addr: "127.0.0.1:1", BinPath: "bin", PGDSN: "dsn",
		Tenant: "acme", GatewayOn: true, GatewaySearch: true,
		GatewayURL: "http://127.0.0.1:8080/gateway/mcp",
		GatewayKey: "svk-test",
	}
	env, err := a.buildEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	assertHasEnv(t, env, "RUNTIME_GATEWAY_URL=http://127.0.0.1:8080/gateway/mcp?mode=search")
	assertHasEnv(t, env, "RUNTIME_GATEWAY_KEY=svk-test")
}

func TestAuthTransport_AddsBearerWhenSet(t *testing.T) {
	var gotAuth string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	req := httptest.NewRequest("GET", backend.URL, nil)
	at := authTransport{token: "sekret"}
	resp, err := at.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotAuth != "Bearer sekret" {
		t.Fatalf("Authorization = %q, want %q", gotAuth, "Bearer sekret")
	}
	if req.Header.Get("Authorization") != "" {
		t.Fatal("authTransport leaked header onto caller's request")
	}
}

func TestAuthTransport_NoBearerWhenEmpty(t *testing.T) {
	var gotAuth string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	resp, err := authTransport{}.RoundTrip(httptest.NewRequest("GET", backend.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotAuth != "" {
		t.Fatalf("Authorization = %q, want empty", gotAuth)
	}
}

func TestBaseURL_LocalFallbackAndRemote(t *testing.T) {
	local := AgentProcess{Addr: "127.0.0.1:8101"}
	if local.baseURL() != "http://127.0.0.1:8101" {
		t.Fatalf("local baseURL = %q", local.baseURL())
	}
	remote := AgentProcess{BaseURL: "https://h:8443"}
	if remote.baseURL() != "https://h:8443" {
		t.Fatalf("remote baseURL = %q", remote.baseURL())
	}
}

func TestReverseProxy_SendsBearer(t *testing.T) {
	var gotAuth string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	rp := reverseProxy(backend.URL, "tok-123", nil)
	rec := httptest.NewRecorder()
	rp.ServeHTTP(rec, httptest.NewRequest("GET", "/sessions", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if gotAuth != "Bearer tok-123" {
		t.Fatalf("backend saw Authorization = %q", gotAuth)
	}
}
