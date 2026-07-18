//go:build integration

package test

import (
	"database/sql"
	"encoding/json"
	"io"
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

// This file is the P1.2 end-to-end proof that operator lifecycle limits
// (config `limits:` → RUNTIME_AGENT_LIMITS → durable enforcement in
// agentruntime) actually terminate sessions against a REAL
// runtimed + agentd + Postgres stack:
//
//	TestLimitMaxTurns                 — max_turns trips a never-finishing agent.
//	TestLimitTurnTimeout              — turn_timeout aborts a stuck tool call.
//	TestLimitTokenBudgetSurvivesRecovery — max_tokens budget is rebuilt from
//	                                    DBOS checkpoints across a SIGKILL.
//
// Boot scaffolding follows test/autoscale_test.go (build binaries, write
// runtime.yaml to a temp dir, start runtimed in its own process group, poll
// /healthz); shared helpers (mustExec, count, resetIdentityTables,
// asEventually, asGet200, rpListenPID) are reused from the other files in
// this package.
//
// Ports: ctl 8900/8901/8902, agents 8910/8920/8930 — no collision with any
// other integration test (autoscale 88xx, replica_pools 87xx, multiagent
// 81xx, resume 80xx).

// lmBoot wipes durable state, builds both binaries, writes cfgYAML to a temp
// runtime.yaml, and starts runtimed with the given extra env (TESTAGENT_* mode
// selectors — agentd inherits them through buildEnv = os.Environ() + delta).
// It registers a t.Cleanup that reaps the whole process group and returns the
// control-plane base URL.
func lmBoot(t *testing.T, db *sql.DB, cfgYAML, ctlAddr string, extraEnv ...string) string {
	t.Helper()

	// ONE-TIME durable-state cleanup so this runtimed starts from a blank
	// slate (same idiom as autoscale/multiagent). agentd recreates markers.
	mustExec(t, db, `DROP TABLE IF EXISTS markers`)
	mustExec(t, db, `DROP TABLE IF EXISTS session_events, sessions, agents CASCADE`)
	mustExec(t, db, `DROP SCHEMA IF EXISTS dbos CASCADE`)
	resetIdentityTables(t, db)

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
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(runtimed)
	cmd.Env = append(os.Environ(),
		"RUNTIME_PG_DSN="+dsn,
		"RUNTIME_CTL_ADDR="+ctlAddr,
		"RUNTIME_AGENTD_BIN="+agentd,
		"RUNTIME_CONFIG="+cfgPath,
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	// Fresh process group so teardown reaps runtimed + every agentd child; a
	// surviving grandchild would hold the inherited stdout pipe and block
	// `go test` on exec I/O (same idiom as autoscale_test.go).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	base := "http://" + ctlAddr
	if !asEventually(t, 20*time.Second, func() bool { return asGet200(base + "/healthz") }) {
		t.Fatalf("control plane never healthy at %s", base)
	}
	return base
}

// lmOpenDB opens + pings the shared integration Postgres.
func lmOpenDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping postgres (running at %s?): %v", dsn, err)
	}
	return db
}

// lmPostSession POSTs one session through the router and returns its id.
// Unlike invokeOn it does NOT stream to completion — limit-terminated sessions
// end with an "error" event, never "done", so callers poll status instead.
func lmPostSession(t *testing.T, base, agent string) string {
	t.Helper()
	resp, err := http.Post(base+"/agents/"+agent+"/sessions",
		"application/json", strings.NewReader(`{"message":"go"}`))
	if err != nil {
		t.Fatalf("post session to %s: %v", agent, err)
	}
	defer resp.Body.Close()
	var out struct {
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.SessionID == "" {
		t.Fatalf("post session to %s: no session id (decode err %v)", agent, err)
	}
	return out.SessionID
}

// lmSessionRow fetches GET /agents/{agent}/sessions/{sid} through the router.
func lmSessionRow(t *testing.T, base, agent, sid string) (status string, turnCount int) {
	t.Helper()
	resp, err := http.Get(base + "/agents/" + agent + "/sessions/" + sid)
	if err != nil {
		return "", 0 // transient (e.g. replica mid-restart); caller polls
	}
	defer resp.Body.Close()
	var row struct {
		Status    string `json:"status"`
		TurnCount int    `json:"turn_count"`
	}
	if json.NewDecoder(resp.Body).Decode(&row) != nil {
		return "", 0
	}
	return row.Status, row.TurnCount
}

// lmEvent is one row of GET /agents/{id}/sessions/{sid}/events.
type lmEvent struct {
	Seq  int64  `json:"seq"`
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	Err  string `json:"error,omitempty"`
}

// lmEvents fetches the stored (non-streaming) event log for a session.
func lmEvents(t *testing.T, base, agent, sid string) []lmEvent {
	t.Helper()
	resp, err := http.Get(base + "/agents/" + agent + "/sessions/" + sid + "/events")
	if err != nil {
		t.Fatalf("get events: %v", err)
	}
	defer resp.Body.Close()
	var evs []lmEvent
	if err := json.NewDecoder(resp.Body).Decode(&evs); err != nil {
		t.Fatalf("decode events: %v", err)
	}
	return evs
}

// lmMetricsBody GETs the control plane's merged /metrics exposition.
func lmMetricsBody(t *testing.T, base string) string {
	t.Helper()
	resp, err := http.Get(base + "/metrics")
	if err != nil {
		t.Fatalf("get metrics: %v", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read metrics: %v", err)
	}
	return string(b)
}

// lmHasLimitHit reports whether the merged exposition carries
// agent_session_limit_hits_total{agent=...,limit=...} with value want. The
// fan-out injects a replica label server-side, so we match on label content,
// not an exact rendered series string.
func lmHasLimitHit(body, agent, limit string, want string) bool {
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "agent_session_limit_hits_total{") {
			continue
		}
		if strings.Contains(line, `agent="`+agent+`"`) &&
			strings.Contains(line, `limit="`+limit+`"`) &&
			strings.HasSuffix(strings.TrimSpace(line), " "+want) {
			return true
		}
	}
	return false
}

// lmTerminal reports whether a session status is terminal.
func lmTerminal(s string) bool {
	return s == "completed" || s == "error" || s == "limit_exceeded"
}

// TestLimitMaxTurns: an agent configured with limits: {max_turns: 3} and
// TESTAGENT_MODE=loop (a marker tool call EVERY turn — the session never
// finishes on its own) must be terminated BY THE PLATFORM:
//
//   - terminal status "limit_exceeded";
//   - the stored events end with an error event whose text is exactly
//     "limit exceeded: max_turns (3/3)" (the loop's turn counter reaches the
//     configured cap entering the 4th iteration's check, observed == 3);
//   - the merged /metrics exposition carries
//     agent_session_limit_hits_total{agent="lim",limit="max_turns"} 1.
func TestLimitMaxTurns(t *testing.T) {
	db := lmOpenDB(t)
	cfg := "agents:\n" +
		"  - {id: lim, name: Lim, model: test/scripted, listen_addr: 127.0.0.1:8910, limits: {max_turns: 3}}\n"
	base := lmBoot(t, db, cfg, "127.0.0.1:8900", "TESTAGENT_MODE=loop")
	waitURL(t, base+"/agents/lim/healthz", 30*time.Second)

	sid := lmPostSession(t, base, "lim")
	t.Logf("session id = %s", sid)

	var finalStatus string
	if !asEventually(t, 30*time.Second, func() bool {
		s, _ := lmSessionRow(t, base, "lim", sid)
		finalStatus = s
		return lmTerminal(s)
	}) {
		t.Fatalf("session never terminal; last status %q", finalStatus)
	}
	if finalStatus != "limit_exceeded" {
		t.Fatalf("status = %q, want %q", finalStatus, "limit_exceeded")
	}

	evs := lmEvents(t, base, "lim", sid)
	if len(evs) == 0 {
		t.Fatal("no stored events for limit-terminated session")
	}
	last := evs[len(evs)-1]
	const wantErr = "limit exceeded: max_turns (3/3)"
	if last.Type != "error" || last.Err != wantErr {
		t.Fatalf("last event = {type:%q err:%q}, want {type:\"error\" err:%q}\nall events: %+v",
			last.Type, last.Err, wantErr, evs)
	}
	t.Logf("event OK: %q", last.Err)

	// Metric: poll the merged exposition briefly (fan-out sub-scrapes are
	// live, but give the agent a beat after termination).
	if !asEventually(t, 10*time.Second, func() bool {
		return lmHasLimitHit(lmMetricsBody(t, base), "lim", "max_turns", "1")
	}) {
		body := lmMetricsBody(t, base)
		var got []string
		for _, line := range strings.Split(body, "\n") {
			if strings.Contains(line, "limit_hits") {
				got = append(got, line)
			}
		}
		t.Fatalf("merged /metrics missing agent_session_limit_hits_total{agent=\"lim\",limit=\"max_turns\"} 1;\n"+
			"limit_hits lines present: %v", got)
	}
	t.Log("metric OK: agent_session_limit_hits_total{agent=\"lim\",limit=\"max_turns\"} 1")
}

// TestLimitTurnTimeout: an agent with limits: {turn_timeout: 2s} and
// TESTAGENT_MODE=sleep (first turn calls the sleep tool, which blocks for
// TESTAGENT_SLEEP_MS=60000) must have the stuck turn aborted by the per-turn
// deadline:
//
//   - terminal status "limit_exceeded" well within 15s (the sleep alone would
//     take 60s — proof the deadline, not the tool, ended the turn);
//   - the error event names turn_timeout;
//   - turn_count stays 0 (the timed-out turn never completed) and no partial
//     entries leaked (no "text" event was ever published).
//
// This is the first LIVE proof of the harness-v0.3.2 classification path:
// RunTurn returns nil error and the timeout is detected from
// TurnResult.StopReason ("aborted"/"error") + runCtx DeadlineExceeded.
func TestLimitTurnTimeout(t *testing.T) {
	db := lmOpenDB(t)
	cfg := "agents:\n" +
		"  - {id: lim2, name: Lim2, model: test/scripted, listen_addr: 127.0.0.1:8920, limits: {turn_timeout: 2s}}\n"
	base := lmBoot(t, db, cfg, "127.0.0.1:8901",
		"TESTAGENT_MODE=sleep", "TESTAGENT_SLEEP_MS=60000")
	waitURL(t, base+"/agents/lim2/healthz", 30*time.Second)

	sid := lmPostSession(t, base, "lim2")
	t.Logf("session id = %s", sid)

	start := time.Now()
	var finalStatus string
	if !asEventually(t, 15*time.Second, func() bool {
		s, _ := lmSessionRow(t, base, "lim2", sid)
		finalStatus = s
		return lmTerminal(s)
	}) {
		t.Fatalf("session not terminal within 15s (turn_timeout=2s did not fire); last status %q", finalStatus)
	}
	if finalStatus != "limit_exceeded" {
		t.Fatalf("status = %q, want %q", finalStatus, "limit_exceeded")
	}
	t.Logf("terminal in %s", time.Since(start).Round(time.Millisecond))

	evs := lmEvents(t, base, "lim2", sid)
	if len(evs) == 0 {
		t.Fatal("no stored events for limit-terminated session")
	}
	last := evs[len(evs)-1]
	const wantErr = "limit exceeded: turn_timeout (2000/2000)"
	if last.Type != "error" || last.Err != wantErr {
		t.Fatalf("last event = {type:%q err:%q}, want {type:\"error\" err:%q}\nall events: %+v",
			last.Type, last.Err, wantErr, evs)
	}

	// The timed-out turn must not have completed: turn_count stays 0 (the
	// workflow fails the limit BEFORE SetTurnCount runs for that turn) …
	_, tc := lmSessionRow(t, base, "lim2", sid)
	if tc != 0 {
		t.Fatalf("turn_count = %d, want 0 (the timed-out sleep turn must not count as completed)", tc)
	}
	// … and no partial entries leaked into the published stream: the turn's
	// entries are discarded on timeout, so no "text" event may exist.
	for _, ev := range evs {
		if ev.Type == "text" {
			t.Fatalf("partial turn leaked a text event: %+v\nall events: %+v", ev, evs)
		}
	}
	t.Logf("event OK: %q; turn_count=0; no text events", last.Err)
}

// TestLimitTokenBudgetSurvivesRecovery: an agent with limits: {max_tokens: 450}
// and TESTAGENT_MODE=loop (150 tokens per turn) accumulates 150/300/450 over
// three turns and trips entering the 4th iteration's budget check with
// observed == 450. Mid-session we SIGKILL the agentd process; the supervisor
// respawns it, DBOS recovers the workflow with the SAME executor id, and the
// budget is REBUILT from the checkpointed per-turn usage — so the session
// still terminates limit_exceeded with exactly (450/450). A crash-reset budget
// would either never trip or trip with a smaller observed value.
//
// TESTAGENT_LOOP_TURN_MS=1500 slows each turn (pure timing, not a decision
// input) so the kill deterministically lands mid-session instead of racing
// millisecond turns.
func TestLimitTokenBudgetSurvivesRecovery(t *testing.T) {
	db := lmOpenDB(t)
	cfg := "agents:\n" +
		"  - {id: lim3, name: Lim3, model: test/scripted, listen_addr: 127.0.0.1:8930, limits: {max_tokens: 450}}\n"
	base := lmBoot(t, db, cfg, "127.0.0.1:8902",
		"TESTAGENT_MODE=loop", "TESTAGENT_LOOP_TURN_MS=1500")
	waitURL(t, base+"/agents/lim3/healthz", 30*time.Second)

	sid := lmPostSession(t, base, "lim3")
	t.Logf("session id = %s", sid)

	// Wait for the FIRST turn to checkpoint (turn_count >= 1), then kill the
	// agentd replica mid-session (pid found by LISTEN port, as the
	// replica-pools kill gate does). If lsof can't find the pid we FAIL — the
	// whole point of this test is the crash.
	if !asEventually(t, 30*time.Second, func() bool {
		_, tc := lmSessionRow(t, base, "lim3", sid)
		return tc >= 1
	}) {
		t.Fatal("first turn never completed (turn_count < 1)")
	}
	pid, ok := rpListenPID(t, 8930)
	if !ok {
		t.Fatal("could not resolve agentd pid on :8930 via lsof; cannot run the kill phase")
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill agentd (pid %d): %v", pid, err)
	}
	t.Logf("killed agentd pid %d after first turn checkpointed", pid)

	// Supervisor respawns (backoff 1s); wait for the replica to serve again.
	if !asEventually(t, 30*time.Second, func() bool {
		return asGet200("http://127.0.0.1:8930/healthz")
	}) {
		t.Fatal("agentd never respawned on :8930 after SIGKILL")
	}
	t.Log("agentd respawned; awaiting DBOS recovery to finish the session")

	// DBOS recovery re-drives the workflow: completed turns replay from
	// checkpoints (rebuilding totalTokens), remaining turns run live, and the
	// budget trips at 450. Generous deadline for recovery + 3 slowed turns.
	var finalStatus string
	if !asEventually(t, 90*time.Second, func() bool {
		s, _ := lmSessionRow(t, base, "lim3", sid)
		finalStatus = s
		return lmTerminal(s)
	}) {
		t.Fatalf("session never terminal after recovery; last status %q", finalStatus)
	}
	if finalStatus != "limit_exceeded" {
		t.Fatalf("status = %q, want %q (budget must survive the crash)", finalStatus, "limit_exceeded")
	}

	evs := lmEvents(t, base, "lim3", sid)
	if len(evs) == 0 {
		t.Fatal("no stored events for limit-terminated session")
	}
	last := evs[len(evs)-1]
	const wantErr = "limit exceeded: max_tokens (450/450)"
	if last.Type != "error" || last.Err != wantErr {
		t.Fatalf("last event = {type:%q err:%q}, want {type:\"error\" err:%q} — observed MUST be the "+
			"full 450, proving the budget was rebuilt from checkpoints, not reset by the crash.\nall events: %+v",
			last.Type, last.Err, wantErr, evs)
	}
	t.Logf("event OK: %q — token budget survived the SIGKILL/recovery", last.Err)
}
