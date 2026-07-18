//go:build integration

package test

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestMultiAgentRouting is the M2 end-to-end validation of the multi-agent
// platform. It exercises the WHOLE stack: runtimed loads a 2-agent config,
// spawns two agentd subprocesses, the control plane routes /agents/{id}/... to
// the right subprocess, BOTH sessions complete through their own DBOS-backed
// durable loop, sessions are isolated per agent, and per-session status
// (Task 5) is tracked.
//
// It deliberately shares ONE Postgres + ONE DBOS schema across both agents.
// Each session is a distinct DBOS workflow (unique id), so a single shared
// schema is correct; this test is the proof.
func TestMultiAgentRouting(t *testing.T) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatalf("ping postgres (is it running at %s?): %v", dsn, err)
	}

	// ONE-TIME durable-state cleanup at the start. Wipe control-plane tables and
	// the DBOS schema so both agents start from a blank slate. DBOS recreates the
	// schema on Launch.
	mustExec(t, db, `DROP TABLE IF EXISTS markers`)
	mustExec(t, db, `CREATE TABLE markers (id BIGSERIAL PRIMARY KEY, ran_at TIMESTAMPTZ)`)
	mustExec(t, db, `DROP TABLE IF EXISTS session_events, sessions, agents CASCADE`)
	mustExec(t, db, `DROP SCHEMA IF EXISTS dbos CASCADE`)
	resetIdentityTables(t, db)

	// Build BOTH binaries to a temp dir. cwd is test/, so package paths are
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

	// 2-agent config.
	cfgPath := filepath.Join(tmp, "runtime.yaml")
	cfg := "agents:\n" +
		"  - {id: alpha, name: Alpha, model: test/scripted, listen_addr: 127.0.0.1:8111}\n" +
		"  - {id: beta, name: Beta, model: test/scripted, listen_addr: 127.0.0.1:8112}\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	// Start runtimed. It loads the config, spawns both agentd subprocesses, and
	// serves the control plane / router. Inherit stdout/stderr so DBOS launch and
	// "supervising agent" lines are visible.
	ctlAddr := "127.0.0.1:8120"
	cmd := exec.Command(runtimed)
	cmd.Env = append(os.Environ(),
		"RUNTIME_PG_DSN="+dsn,
		"RUNTIME_CTL_ADDR="+ctlAddr,
		"RUNTIME_AGENTD_BIN="+agentd,
		"RUNTIME_CONFIG="+cfgPath,
	)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	// Put runtimed and the agentd subprocesses it spawns into a fresh process
	// group so we can reap the WHOLE tree at teardown. agentd inherits this
	// process's stdout/stderr pipes; if any grandchild survives, it keeps the
	// pipe open and `go test` blocks on exec I/O. Killing the group avoids that.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		// Negative pid signals the entire process group (runtimed + agentd kids).
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	base := "http://" + ctlAddr
	// Control plane up first, then BOTH agents reachable THROUGH the router.
	// runtimed spawns the agents; they bind + run DBOS Launch, which takes a
	// moment, so give the per-agent health checks a generous window.
	waitURL(t, base+"/healthz", 15*time.Second)
	waitURL(t, base+"/agents/alpha/healthz", 20*time.Second)
	waitURL(t, base+"/agents/beta/healthz", 20*time.Second)

	// Invoke a session on EACH agent THROUGH the router; assert each completes
	// with the scripted "final answer".
	saID, saBody := invokeOn(t, base, "alpha")
	if !strings.Contains(saBody, "final answer") {
		t.Fatalf("alpha session did not complete (no \"final answer\"):\n%s", saBody)
	}
	sbID, sbBody := invokeOn(t, base, "beta")
	if !strings.Contains(sbBody, "final answer") {
		t.Fatalf("beta session did not complete (no \"final answer\"):\n%s", sbBody)
	}
	t.Logf("alpha session=%s beta session=%s", saID, sbID)

	// Session isolation: alpha lists alpha's session and NOT beta's; vice versa.
	aRows := agentSessions(t, base, "alpha")
	bRows := agentSessions(t, base, "beta")
	if !containsID(aRows, saID) || containsID(aRows, sbID) {
		t.Fatalf("alpha sessions wrong: %+v (want %s present, %s absent)", aRows, saID, sbID)
	}
	if !containsID(bRows, sbID) || containsID(bRows, saID) {
		t.Fatalf("beta sessions wrong: %+v (want %s present, %s absent)", bRows, sbID, saID)
	}

	// Status tracking (Task 5): alpha's completed session reports status
	// "completed" and turn_count >= 1.
	var saRow *sessRow
	for i := range aRows {
		if aRows[i].ID == saID {
			saRow = &aRows[i]
		}
	}
	if saRow == nil || saRow.Status != "completed" || saRow.TurnCount < 1 {
		t.Fatalf("alpha session status/turn wrong: %+v", saRow)
	}
	// And confirm the same on beta, so isolation + status hold for both agents.
	var sbRow *sessRow
	for i := range bRows {
		if bRows[i].ID == sbID {
			sbRow = &bRows[i]
		}
	}
	if sbRow == nil || sbRow.Status != "completed" || sbRow.TurnCount < 1 {
		t.Fatalf("beta session status/turn wrong: %+v", sbRow)
	}
	t.Logf("status ok: alpha=%+v beta=%+v", saRow, sbRow)
}

// sessRow decodes one entry of GET /agents/{id}/sessions.
type sessRow struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	TurnCount int    `json:"turn_count"`
}

func containsID(rows []sessRow, id string) bool {
	for _, r := range rows {
		if r.ID == id {
			return true
		}
	}
	return false
}

// waitURL polls url every 150ms until it returns 200 OK or the deadline passes.
func waitURL(t *testing.T, url string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			ok := resp.StatusCode == http.StatusOK
			resp.Body.Close()
			if ok {
				return
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("url never healthy within %s: %s", d, url)
}

// invokeOn POSTs a session to agent through the router and streams it to
// completion, returning the session id and the accumulated stream body.
func invokeOn(t *testing.T, base, agent string) (string, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"message": "go"})
	resp, err := http.Post(base+"/agents/"+agent+"/sessions", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("invoke %s: %v", agent, err)
	}
	var out struct {
		SessionID string `json:"session_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if out.SessionID == "" {
		t.Fatalf("invoke %s: no session id in response", agent)
	}
	final := streamURL(t, base+"/agents/"+agent+"/sessions/"+out.SessionID+"/stream?since=0", 30*time.Second)
	return out.SessionID, final
}

// streamURL opens the SSE stream and reads incrementally until a done event
// appears or the deadline elapses. The accumulated body is returned either way.
func streamURL(t *testing.T, url string, d time.Duration) string {
	t.Helper()
	client := &http.Client{Timeout: d}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("stream %s: %v", url, err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 4096)
	var sb strings.Builder
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			sb.Write(buf[:n])
			if strings.Contains(sb.String(), `"type":"done"`) {
				return sb.String()
			}
		}
		if rerr != nil {
			break
		}
	}
	return sb.String()
}

// agentSessions fetches GET /agents/{id}/sessions and decodes the rows.
func agentSessions(t *testing.T, base, agent string) []sessRow {
	t.Helper()
	resp, err := http.Get(base + "/agents/" + agent + "/sessions")
	if err != nil {
		t.Fatalf("list sessions %s: %v", agent, err)
	}
	defer resp.Body.Close()
	var rows []sessRow
	_ = json.NewDecoder(resp.Body).Decode(&rows)
	return rows
}
