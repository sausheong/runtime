//go:build integration

package test

import (
	"database/sql"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sausheong/runtime/conformance"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// operCtlAddr is the control-plane bind address used by the operability tests.
// It is distinct from the M2 test's address so the suites never collide.
const operCtlAddr = "127.0.0.1:8230"

// oneAgentTokenCfg is a single-agent config with bearer-token auth ON.
const oneAgentTokenCfg = `agents:
  - {id: solo, name: Solo, model: test/scripted, listen_addr: 127.0.0.1:8231}
tokens:
  - {token: "t0ken", label: "test"}
`

// oneAgentOpenCfg is a single-agent config with NO tokens (OPEN mode).
const oneAgentOpenCfg = `agents:
  - {id: solo, name: Solo, model: test/scripted, listen_addr: 127.0.0.1:8231}
`

// startRuntimed builds agentd + runtimed, writes cfgBody to a temp runtime.yaml,
// starts runtimed in its OWN process group, and waits for /healthz (which is
// auth-exempt, so this works even when tokens are configured). It returns the
// control-plane base URL and a stop func that reaps the whole process tree.
//
// It reuses the EXACT process-group start/kill idiom from multiagent_test.go:
// Setpgid groups runtimed with the agentd subprocesses it spawns, and a
// negative-pid SIGKILL reaps the entire group at teardown so no grandchild
// keeps the inherited stdout/stderr pipe open and blocks `go test`.
func startRuntimed(t *testing.T, cfgBody string) (string, func()) {
	t.Helper()

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatalf("ping postgres (is it running at %s?): %v", dsn, err)
	}

	// One-time durable-state cleanup so each test starts from a blank slate.
	// DBOS recreates its schema on Launch.
	mustExec(t, db, `DROP TABLE IF EXISTS session_events, sessions, agents, markers CASCADE`)
	mustExec(t, db, `DROP SCHEMA IF EXISTS dbos CASCADE`)
	resetIdentityTables(t, db)

	// Build both binaries to a temp dir. cwd is test/, so package paths are
	// ../cmd/...
	tmp := t.TempDir()
	agentd := filepath.Join(tmp, "agentd")
	if out, err := exec.Command("go", "build", "-o", agentd, "../cmd/agentd").CombinedOutput(); err != nil {
		t.Fatalf("build agentd: %v\n%s", err, out)
	}
	runtimed := filepath.Join(tmp, "runtimed")
	if out, err := exec.Command("go", "build", "-o", runtimed, "../cmd/runtimed").CombinedOutput(); err != nil {
		t.Fatalf("build runtimed: %v\n%s", err, out)
	}

	cfgPath := filepath.Join(tmp, "runtime.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(runtimed)
	cmd.Env = append(os.Environ(),
		"RUNTIME_PG_DSN="+dsn,
		"RUNTIME_CTL_ADDR="+operCtlAddr,
		"RUNTIME_AGENTD_BIN="+agentd,
		"RUNTIME_CONFIG="+cfgPath,
	)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	// Fresh process group so we can reap runtimed + the agentd kids it spawns.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	stop := func() {
		// Negative pid signals the entire process group.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}

	base := "http://" + operCtlAddr
	// /healthz is auth-exempt, so this works whether or not tokens are on.
	waitURL(t, base+"/healthz", 15*time.Second)
	return base, stop
}

// TestAuthEnforced proves bearer-token auth gates the API while /healthz stays
// open, using a config with tokens configured.
func TestAuthEnforced(t *testing.T) {
	base, stop := startRuntimed(t, oneAgentTokenCfg)
	defer stop()

	client := &http.Client{Timeout: 5 * time.Second}

	// No token → API call is rejected with 401.
	resp, err := client.Get(base + "/agents")
	if err != nil {
		t.Fatalf("GET /agents (no token): %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /agents (no token): status %d, want 401", resp.StatusCode)
	}

	// /healthz is exempt → 200 even with no token.
	resp, err = client.Get(base + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz (no token): %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz (no token): status %d, want 200", resp.StatusCode)
	}

	// Valid bearer token → 200.
	req, _ := http.NewRequest(http.MethodGet, base+"/agents", nil)
	req.Header.Set("Authorization", "Bearer t0ken")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("GET /agents (with token): %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /agents (with token): status %d, want 200", resp.StatusCode)
	}
}

// TestConsoleOverview proves the web console: /ui/login is exempt, tokenless
// /ui redirects (303) to the login page, and an authenticated /ui renders the
// overview including the agent id.
func TestConsoleOverview(t *testing.T) {
	base, stop := startRuntimed(t, oneAgentTokenCfg)
	defer stop()

	client := &http.Client{Timeout: 5 * time.Second}

	// /ui/login is exempt → 200 even with no token.
	resp, err := client.Get(base + "/ui/login")
	if err != nil {
		t.Fatalf("GET /ui/login: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /ui/login: status %d, want 200", resp.StatusCode)
	}

	// Tokenless /ui → 303 to /ui/login. The default http.Client FOLLOWS
	// redirects (and /ui/login is 200), so a normal GET would mask the 303.
	// Use a non-following client to observe the redirect itself.
	noFollow := &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err = noFollow.Get(base + "/ui")
	if err != nil {
		t.Fatalf("GET /ui (no token): %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("GET /ui (no token): status %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/ui/login" {
		t.Fatalf("GET /ui (no token): Location %q, want /ui/login", loc)
	}

	// Authenticated /ui (via the runtime_token cookie) → 200, and the overview
	// lists the agent id "solo".
	req, _ := http.NewRequest(http.MethodGet, base+"/ui", nil)
	req.AddCookie(&http.Cookie{Name: "runtime_token", Value: "t0ken"})
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("GET /ui (with cookie): %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /ui (with cookie): status %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), "solo") {
		t.Fatalf("GET /ui (with cookie): body does not contain agent id %q:\n%s", "solo", string(body))
	}
}

// tConf adapts the conformance suite's TestingT onto *testing.T. It records any
// failure without aborting the suite mid-run, so all contract checks execute and
// the test can assert the aggregate result.
type tConf struct {
	t      *testing.T
	failed bool
}

func (c *tConf) Errorf(f string, a ...any) { c.failed = true; c.t.Logf("conformance FAIL: "+f, a...) }
func (c *tConf) Fatalf(f string, a ...any) { c.failed = true; c.t.Logf("conformance FATAL: "+f, a...) }
func (c *tConf) Logf(f string, a ...any)   { c.t.Logf(f, a...) }

// TestConformanceThroughControlPlane runs the full agent contract suite against
// an agent reached THROUGH the control-plane router, exercising real routing.
//
// It deliberately uses OPEN mode (no tokens). conformance.Run builds its own
// http client with NO auth header, so an authenticated control plane would
// reject every call. Running against an OPEN-mode control plane lets the suite
// validate routing + the full contract without any auth plumbing.
func TestConformanceThroughControlPlane(t *testing.T) {
	base, stop := startRuntimed(t, oneAgentOpenCfg)
	defer stop()

	// Give the agent a moment to become routable through the control plane.
	// In open mode the per-agent healthz needs no token. runtimed spawns the
	// agent, which binds + runs DBOS Launch, so allow a generous window.
	waitURL(t, base+"/agents/solo/healthz", 20*time.Second)

	tc := &tConf{t: t}
	conformance.Run(tc, base+"/agents/solo")
	if tc.failed {
		t.Fatalf("conformance suite reported failures through the control plane (see logs above)")
	}
}
