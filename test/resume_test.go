//go:build integration

package test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// dsn is the live Postgres used by this integration test. It must be a real,
// reachable database — the whole point of this test is to exercise the durable
// path against real Postgres + real DBOS + a real agentd subprocess.
const dsn = "postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable"

// listenAddr is the agentd HTTP bind address for both process generations.
const listenAddr = "127.0.0.1:8091"

// baseURL is the agentd contract base URL.
const baseURL = "http://127.0.0.1:8091"

// TestResumeAfterKill is the flagship integration test for the runtime spine.
//
// It proves the central durability claim of the milestone: if an agent
// subprocess is killed mid-turn (after a committed side effect but before the
// turn step checkpoints), a fresh subprocess started against the same Postgres
// recovers the in-flight DBOS workflow and drives the session to completion.
//
// Phase 1: start agentd with CRASH_AFTER_MARKER=1. POST a session; the workflow
// runs turn 1 (calls the marker tool, which INSERTs a row then os.Exit(1)s).
// The process dies before the turn step checkpoints.
//
// Phase 2: start the SAME agentd binary WITHOUT the crash flag. dbos.Launch
// recovers the pending workflow (same executorID "local", same app version =
// same binary hash) and re-drives it: turn 1 re-runs the marker → checkpoints →
// turn 2 emits "final answer" → done.
//
// We then re-attach to the SSE stream and assert it contains "final answer" and
// exactly one done event. The marker count is >= 1 (2 demonstrates the spec's
// at-least-once semantics: the side effect re-runs on resume because the crash
// happened after the INSERT but before the checkpoint).
func TestResumeAfterKill(t *testing.T) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatalf("ping postgres (is it running at %s?): %v", dsn, err)
	}

	// ----------------------------------------------------------------------
	// ONE-TIME durable-state cleanup, BEFORE process 1. Wipe the control-plane
	// tables AND the DBOS schema so process 1 starts from a blank slate. This
	// is the ONLY cleanup in the whole test — we NEVER clean between the crash
	// and the restart, because the in-flight DBOS workflow MUST persist across
	// the crash. That persistence is exactly what we are testing.
	// ----------------------------------------------------------------------
	mustExec(t, db, `DROP TABLE IF EXISTS markers`)
	mustExec(t, db, `DROP TABLE IF EXISTS session_events, sessions, agents CASCADE`)
	mustExec(t, db, `DROP SCHEMA IF EXISTS dbos CASCADE`) // DBOS recreates it on Launch
	// Recreate markers up front (agentd also creates it IF NOT EXISTS).
	mustExec(t, db, `CREATE TABLE markers (id BIGSERIAL PRIMARY KEY, ran_at TIMESTAMPTZ)`)

	// Build the agentd binary ONCE. Both process generations run this exact
	// binary so DBOS computes an identical application version (binary hash),
	// which is required for recovery to match the pending workflow.
	bin := buildAgentd(t)

	env := map[string]string{
		"RUNTIME_PG_DSN":      dsn,
		"RUNTIME_LISTEN_ADDR": listenAddr,
		"RUNTIME_AGENT_ID":    "default",
	}

	// ----------------------------------------------------------------------
	// Phase 1: crash mid-turn.
	// ----------------------------------------------------------------------
	p1env := map[string]string{}
	for k, v := range env {
		p1env[k] = v
	}
	p1env["CRASH_AFTER_MARKER"] = "1"

	p1, p1done := startAgentd(t, bin, p1env)
	// p1 is expected to self-crash, but defer a kill so an early Fatalf
	// (e.g. waitHealthy/postSession failing on infra issues) never orphans it.
	defer func() { _ = p1.Process.Kill() }()
	waitHealthy(t)

	sessionID := postSession(t, db, "go")
	t.Logf("phase 1: session id = %s", sessionID)

	// The marker INSERTs, then the process self-exits (os.Exit(1)) after a
	// short flush sleep. Wait for the process to actually die.
	if err := waitExit(p1done, 10*time.Second); err != nil {
		_ = p1.Process.Kill()
		t.Fatalf("phase 1 agentd did not exit after crash: %v", err)
	}
	t.Logf("phase 1: agentd exited (crash fired)")

	// Sanity: the marker ran (its side effect committed) before the crash.
	if n := count(t, db, `SELECT count(*) FROM markers`); n < 1 {
		t.Fatalf("expected marker count >= 1 after crash, got %d", n)
	}

	// ----------------------------------------------------------------------
	// Phase 2: restart WITHOUT the crash flag. DBOS recovers the workflow.
	// NO state cleanup happens here — the pending workflow must survive.
	// ----------------------------------------------------------------------
	p2, p2done := startAgentd(t, bin, env)
	defer func() {
		_ = p2.Process.Kill()
		<-p2done
	}()
	waitHealthy(t)
	t.Logf("phase 2: agentd restarted; awaiting workflow recovery")

	// Re-attach to the durable event stream and read until done.
	body := streamUntilDone(t, sessionID, 30*time.Second)
	t.Logf("phase 2: stream body:\n%s", body)

	// ----------------------------------------------------------------------
	// ASSERTIONS
	// ----------------------------------------------------------------------
	// CORE: the session resumed and completed after the kill.
	if !strings.Contains(body, "final answer") {
		t.Fatalf("CORE ASSERTION FAILED: stream did not contain \"final answer\" — "+
			"the workflow did not resume/complete after the crash.\nstream body:\n%s", body)
	}
	// The loop terminated exactly once.
	if got := strings.Count(body, `"type":"done"`); got != 1 {
		t.Fatalf("expected exactly one done event, got %d\nstream body:\n%s", got, body)
	}

	// Marker count: >= 1 is the hard requirement. 2 is correct and expected
	// (at-least-once: crash after side effect, before checkpoint → re-run on
	// resume). Assert <= 2 to catch runaway re-execution.
	markers := count(t, db, `SELECT count(*) FROM markers`)
	t.Logf("marker count after resume = %d "+
		"(1 = no re-run; 2 = at-least-once re-run on resume, per spec §7)", markers)
	if markers < 1 {
		t.Fatalf("expected marker count >= 1, got %d", markers)
	}
	if markers > 2 {
		t.Fatalf("marker count %d > 2 — runaway re-execution; the workflow "+
			"re-ran the side effect more than once on resume", markers)
	}
}

// mustExec runs a statement and fails the test on error.
func mustExec(t *testing.T, db *sql.DB, q string) {
	t.Helper()
	if _, err := db.Exec(q); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

// resetIdentityTables makes tests that intentionally boot runtimed in open
// mode independent of earlier identity-enabled integration tests. Identity is
// persisted, so clearing only session/DBOS state is not sufficient: any
// leftover tenant flips the next control plane into enforced mode.
func resetIdentityTables(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, q := range []string{
		`DROP TABLE IF EXISTS service_keys CASCADE`,
		`DROP TABLE IF EXISTS identity_users CASCADE`,
		`DROP TABLE IF EXISTS tenants CASCADE`,
	} {
		mustExec(t, db, q)
	}
}

// count runs a single-column count query and returns the int.
func count(t *testing.T, db *sql.DB, q string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", q, err)
	}
	return n
}

// buildAgentd compiles cmd/agentd to a temp path and returns the binary path.
// cwd of the test is the test/ directory, so the package path is ../cmd/agentd.
func buildAgentd(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "agentd")
	cmd := exec.Command("go", "build", "-o", bin, "../cmd/agentd")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build agentd: %v", err)
	}
	return bin
}

// startAgentd launches the agentd binary with the given env (merged onto the
// current environment) and returns the cmd plus a channel that receives the
// process exit error when it terminates. Stdout/stderr are inherited so DBOS
// recovery logs are visible when debugging.
func startAgentd(t *testing.T, bin string, env map[string]string) (*exec.Cmd, <-chan error) {
	t.Helper()
	cmd := exec.Command(bin)
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start agentd: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	return cmd, done
}

// waitHealthy polls GET /healthz every 100ms until 200 OK or 10s elapse.
func waitHealthy(t *testing.T) {
	t.Helper()
	client := &http.Client{Timeout: 1 * time.Second}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(baseURL + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("agentd did not become healthy within 10s")
}

// waitExit waits for the process exit signal or returns an error on timeout.
func waitExit(done <-chan error, timeout time.Duration) error {
	select {
	case <-done: // any exit (the crash is os.Exit(1), a non-nil error) is fine
		return nil
	case <-time.After(timeout):
		return errors.New("timeout waiting for process to exit")
	}
}

// postSession POSTs to /sessions and returns the session id. Primary source is
// the JSON response; if the POST fails (e.g. the process died before the
// response flushed), it falls back to the most-recent session row in Postgres.
func postSession(t *testing.T, db *sql.DB, msg string) string {
	t.Helper()
	reqBody, _ := json.Marshal(map[string]string{"message": msg})
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(baseURL+"/sessions", "application/json", bytes.NewReader(reqBody))
	if err == nil {
		defer resp.Body.Close()
		var out struct {
			SessionID string `json:"session_id"`
		}
		b, _ := io.ReadAll(resp.Body)
		if json.Unmarshal(b, &out) == nil && out.SessionID != "" {
			return out.SessionID
		}
		t.Logf("postSession: response did not carry a session id (%q); falling back to DB", string(b))
	} else {
		t.Logf("postSession: POST failed (%v); falling back to DB query", err)
	}

	// Fallback: the workflow row is created before the crash, so the latest
	// session in Postgres is the one we just started.
	id, derr := latestSessionID(db)
	if derr != nil {
		t.Fatalf("postSession: no session id from POST and DB fallback failed: %v", derr)
	}
	return id
}

// latestSessionID returns the id of the most recently created session.
func latestSessionID(db *sql.DB) (string, error) {
	var id string
	err := db.QueryRow(`SELECT id FROM sessions ORDER BY created_at DESC LIMIT 1`).Scan(&id)
	return id, err
}

// streamUntilDone opens GET /sessions/{id}/stream?since=0 and reads the SSE
// body incrementally, accumulating into a builder. It returns as soon as a
// done event appears or the deadline elapses. The whole accumulated body is
// returned either way so assertions can inspect it.
func streamUntilDone(t *testing.T, sessionID string, timeout time.Duration) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		baseURL+"/sessions/"+sessionID+"/stream?since=0", nil)
	if err != nil {
		t.Fatalf("build stream request: %v", err)
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer resp.Body.Close()

	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			sb.Write(buf[:n])
			if strings.Contains(sb.String(), `"type":"done"`) {
				return sb.String()
			}
		}
		if rerr != nil {
			// EOF / context deadline / connection close: return what we have.
			return sb.String()
		}
	}
}
