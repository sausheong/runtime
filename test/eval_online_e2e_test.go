//go:build integration

package test

import (
	"context"
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

	"github.com/sausheong/runtime/internal/eval"
	"github.com/sausheong/runtime/internal/identity"
	"github.com/sausheong/runtime/internal/store"
)

// This file is the P3.1-M2 end-to-end proof that online sampling + transcript
// capture run against a REAL runtimed + agentd + Postgres stack. A tenant admin
// authors a per-agent online-eval policy (sample_rate 100 + two contains
// criteria — one that MUST pass against the testagent's constant "final answer"
// output, one that MUST fail); a session is driven to completion through the
// control plane; and every observable outcome is asserted:
//
//	(1) the session's full turn transcript is captured in session_transcripts;
//	(2) two online_eval_results rows appear (polled), with the exact per-criterion
//	    pass/fail the constant output produces — no judge/grader model in the loop;
//	(3) /metrics carries the agentd-owned agent_eval_sessions_scored_total and
//	    agent_eval_criteria_total series (they survive the fan-out with agent=);
//	(4) tenant isolation: a second-tenant admin sees no results for the session;
//	(5) a policy with an out-of-range sample_rate (101) is rejected 400.
//
// TIMING (critical): the PolicyResolver resolves RUNTIME_EVAL_POLICY at agent
// SPAWN time, and agents spawn at control-plane BOOT. So the policy is seeded
// directly into the eval_policies table (via eval.PolicyStore) BEFORE runtimed
// starts — exactly as identity is seeded directly here. Authoring the policy over
// the /admin/evals/policy API after boot would not reach the already-spawned
// agent (that path is covered separately by asserting the rate-101 rejection).
//
// SUBJECT FORWARDING is ON (RUNTIME_SUBJECT_FORWARDING=1): the agent stamps the
// forwarded caller tenant onto the transcript + results rows. The session is
// driven with the acme admin bearer, so the control-plane edge forwards
// X-Runtime-Tenant=acme, and the results land under tenant "acme" — which is what
// the results API filters on and what the isolation assertion probes.
//
// Boot scaffolding mirrors test/eval_e2e_test.go (identity store with admin
// service keys, runtimed in its own process group). Ports: ctl 8520, agent 8521
// — no collision with any other integration test.

// evalOnlineResetDB drops every shared table (durable + identity + eval + online)
// so this test starts blank and leaves the DB clean for siblings (leftover
// identity rows would flip a later open-mode runtimed into enforced mode).
func evalOnlineResetDB(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, q := range []string{
		`DROP TABLE IF EXISTS markers`,
		`DROP TABLE IF EXISTS online_eval_results CASCADE`,
		`DROP TABLE IF EXISTS session_transcripts CASCADE`,
		`DROP TABLE IF EXISTS session_events, sessions, agents CASCADE`,
		`DROP SCHEMA IF EXISTS dbos CASCADE`,
		`DROP TABLE IF EXISTS eval_results, eval_runs, eval_sets CASCADE`,
		`DROP TABLE IF EXISTS eval_policies CASCADE`,
		`DROP TABLE IF EXISTS service_keys CASCADE`,
		`DROP TABLE IF EXISTS identity_users CASCADE`,
		`DROP TABLE IF EXISTS tenants CASCADE`,
	} {
		mustExec(t, db, q)
	}
}

// TestOnlineSamplingLifecycle drives one online-sampled session end-to-end and
// asserts transcript capture, per-criterion online results, the agent-owned
// metrics, tenant isolation, and bad-policy rejection.
func TestOnlineSamplingLifecycle(t *testing.T) {
	ctx := context.Background()

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatalf("ping postgres (%s): %v", dsn, err)
	}
	evalOnlineResetDB(t, db)
	t.Cleanup(func() {
		cdb, err := sql.Open("pgx", dsn)
		if err != nil {
			return
		}
		defer cdb.Close()
		for _, q := range []string{
			`DROP TABLE IF EXISTS online_eval_results CASCADE`,
			`DROP TABLE IF EXISTS session_transcripts CASCADE`,
			`DROP TABLE IF EXISTS eval_policies CASCADE`,
			`DROP TABLE IF EXISTS eval_results, eval_runs, eval_sets CASCADE`,
			`DROP TABLE IF EXISTS service_keys CASCADE`,
			`DROP TABLE IF EXISTS identity_users CASCADE`,
			`DROP TABLE IF EXISTS tenants CASCADE`,
		} {
			_, _ = cdb.Exec(q)
		}
	})

	// Identity: two tenants, each with an ADMIN service key. acme owns the target
	// agent + authors the policy; globex is the isolation probe.
	st, err := identity.NewStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTenant(ctx, "acme", "Acme"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTenant(ctx, "globex", "Globex"); err != nil {
		t.Fatal(err)
	}
	acmeAdmin, _ := identity.MintServiceKey()
	if err := st.InsertServiceKey(ctx, acmeAdmin.ID, "acme", acmeAdmin.Hash, identity.RoleAdmin, "acme-admin"); err != nil {
		t.Fatal(err)
	}
	globexAdmin, _ := identity.MintServiceKey()
	if err := st.InsertServiceKey(ctx, globexAdmin.ID, "globex", globexAdmin.Hash, identity.RoleAdmin, "globex-admin"); err != nil {
		t.Fatal(err)
	}

	// Seed the online-eval policy DIRECTLY in the DB BEFORE boot: the resolver
	// reads it at spawn time, and the agent spawns at control-plane boot. sample
	// 100% (so this one session is always sampled) with two contains criteria:
	//   - "pass-final": contains "final answer" ⇒ PASS against the testagent's
	//     constant output "final answer".
	//   - "fail-banana": contains "banana" ⇒ FAIL (the output never contains it).
	ps, err := eval.NewPolicyStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if err := ps.PutPolicy(ctx, eval.Policy{
		Tenant: "acme", AgentID: "ev1", SampleRate: 100,
		Criteria: []eval.Criterion{
			{Name: "pass-final", Scorer: eval.ScorerContains, Pattern: "final answer"},
			{Name: "fail-banana", Scorer: eval.ScorerContains, Pattern: "banana"},
		},
	}); err != nil {
		t.Fatalf("seed policy: %v", err)
	}

	tmp := t.TempDir()
	agentd := buildBin(t, tmp, "agentd")
	runtimed := buildBin(t, tmp, "runtimed")

	cfgPath := filepath.Join(tmp, "runtime.yaml")
	cfg := "agents:\n" +
		"  - {id: ev1, name: Ev1, model: test/scripted, listen_addr: 127.0.0.1:8521, tenant: acme}\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	ctlAddr := "127.0.0.1:8520"
	cmd := exec.Command(runtimed)
	cmd.Env = append(os.Environ(),
		"RUNTIME_PG_DSN="+dsn,
		"RUNTIME_CTL_ADDR="+ctlAddr,
		"RUNTIME_AGENTD_BIN="+agentd,
		"RUNTIME_CONFIG="+cfgPath,
		"RUNTIME_SUBJECT_FORWARDING=1", // edge forwards X-Runtime-Tenant ⇒ results stamped tenant=acme
	) // no TESTAGENT_MODE ⇒ classic script ⇒ output "final answer"
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); _, _ = cmd.Process.Wait() }()

	base := "http://" + ctlAddr
	waitURL(t, base+"/healthz", 20*time.Second)
	// The agent must be spawned + healthy before we drive a session through it.
	if !asEventually(t, 40*time.Second, func() bool {
		return agentHealthyWithBearer(t, ctlAddr, acmeAdmin.Plaintext, "ev1")
	}) {
		t.Fatal("agent ev1 never became healthy")
	}

	// (1) Drive one session to completion through the control plane, authenticated
	// as the acme admin so the edge forwards X-Runtime-Tenant=acme. POST is
	// ActionInvoke (admin ⇒ allowed).
	var sc struct {
		SessionID string `json:"session_id"`
	}
	adminPostJSON(t, ctlAddr, acmeAdmin.Plaintext, "/agents/ev1/sessions",
		map[string]string{"message": "score me"}, http.StatusOK, &sc)
	if sc.SessionID == "" {
		t.Fatal("session create returned empty session_id")
	}
	sid := sc.SessionID
	t.Logf("session id = %s", sid)

	// Wait for the session to reach a terminal state (the testagent runs the
	// marker tool, then emits "final answer" + done ⇒ status "completed").
	if !asEventually(t, 60*time.Second, func() bool {
		var status string
		if err := db.QueryRow(`SELECT status FROM sessions WHERE id=$1`, sid).Scan(&status); err != nil {
			return false
		}
		return status == "completed"
	}) {
		var status string
		_ = db.QueryRow(`SELECT status FROM sessions WHERE id=$1`, sid).Scan(&status)
		t.Fatalf("session never completed; last status %q", status)
	}
	t.Log("session OK: reached status=completed")

	// (2) Transcript: session_transcripts carries the captured turn entries. No
	// read API exists, so query the DB directly (like the store integration
	// tests). The classic 2-turn script produces two turn rows (turn 0 = marker
	// tool call/result, turn 1 = final answer); assert >=1 row, tenant=acme, and
	// that the final answer text is present in the captured JSONB entries.
	if !asEventually(t, 15*time.Second, func() bool {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM session_transcripts WHERE session_id=$1`, sid).Scan(&n); err != nil {
			return false
		}
		return n >= 1
	}) {
		t.Fatal("no session_transcripts rows captured for the session")
	}
	var (
		turnRows    int
		tenantSeen  string
		entriesText string
	)
	if err := db.QueryRow(
		`SELECT count(*), max(tenant), string_agg(entries::text, ' ')
		   FROM session_transcripts WHERE session_id=$1`, sid).
		Scan(&turnRows, &tenantSeen, &entriesText); err != nil {
		t.Fatalf("read session_transcripts: %v", err)
	}
	if turnRows < 1 {
		t.Fatalf("session_transcripts turn rows = %d, want >=1", turnRows)
	}
	if tenantSeen != "acme" {
		t.Fatalf("session_transcripts tenant = %q, want acme (subject forwarding on)", tenantSeen)
	}
	if !strings.Contains(entriesText, "final answer") {
		t.Fatalf("captured transcript entries do not contain %q: %s", "final answer", entriesText)
	}
	t.Logf("transcript OK: %d turn row(s), tenant=acme, entries carry the final answer", turnRows)

	// (3) Online results: poll GET /admin/evals/online-results?session=<sid> until
	// the two criteria appear, then assert the exact per-criterion pass/fail.
	var results []store.OnlineResult
	if !asEventually(t, 30*time.Second, func() bool {
		results = getOnlineResults(t, ctlAddr, acmeAdmin.Plaintext, sid)
		return len(results) == 2
	}) {
		t.Fatalf("expected 2 online results for session %s, got %d: %+v", sid, len(results), results)
	}
	byCriterion := map[string]store.OnlineResult{}
	for _, r := range results {
		byCriterion[r.Criterion] = r
	}
	if pass, ok := byCriterion["pass-final"]; !ok || !pass.Passed {
		t.Fatalf("criterion pass-final: got %+v, want passed=true", pass)
	}
	if fail, ok := byCriterion["fail-banana"]; !ok || fail.Passed {
		t.Fatalf("criterion fail-banana: got %+v, want passed=false", fail)
	}
	for _, r := range results {
		if r.Tenant != "acme" {
			t.Fatalf("online result %q tenant = %q, want acme", r.Criterion, r.Tenant)
		}
		if r.Scorer != string(eval.ScorerContains) {
			t.Fatalf("online result %q scorer = %q, want contains", r.Criterion, r.Scorer)
		}
	}
	t.Log("online results OK: pass-final=pass, fail-banana=fail, both tenant=acme")

	// (4) Metrics: the agentd-owned eval counters must survive the fan-out with an
	// agent=ev1 label. The sub-scrape may lag the background scoring by a beat, so
	// poll briefly. /metrics is served OUTSIDE the identity chain (no bearer).
	if !asEventually(t, 20*time.Second, func() bool {
		body := getBody(t, base+"/metrics", nil, 200)
		return evalHasPositiveSeries(body, "agent_eval_sessions_scored_total", "acme", `agent="ev1"`) &&
			evalHasPositiveSeries(body, "agent_eval_criteria_total", "acme", `result="pass"`) &&
			evalHasPositiveSeries(body, "agent_eval_criteria_total", "acme", `result="fail"`)
	}) {
		body := getBody(t, base+"/metrics", nil, 200)
		var got []string
		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(line, "agent_eval_sessions_scored_total") ||
				strings.HasPrefix(line, "agent_eval_criteria_total") {
				got = append(got, line)
			}
		}
		t.Fatalf("/metrics missing incremented agent_eval_sessions_scored_total / "+
			"agent_eval_criteria_total for agent ev1 tenant acme; eval lines present:\n%s",
			strings.Join(got, "\n"))
	}
	t.Log("metric OK: agent_eval_sessions_scored_total{agent=ev1} + agent_eval_criteria_total{pass,fail} survived fan-out")

	// (5) Tenant isolation: the globex admin reads the SAME session's online
	// results and sees nothing (results are tenant-filtered; acme's rows are
	// invisible cross-tenant). Sanity: acme itself still sees its two rows.
	if got := getOnlineResults(t, ctlAddr, globexAdmin.Plaintext, sid); len(got) != 0 {
		t.Fatalf("cross-tenant online results: globex admin saw %d rows, want 0: %+v", len(got), got)
	}
	if got := getOnlineResults(t, ctlAddr, acmeAdmin.Plaintext, sid); len(got) != 2 {
		t.Fatalf("owner online results: acme admin saw %d rows, want 2 (proves the 0 above is isolation)", len(got))
	}
	t.Log("isolation OK: globex admin sees 0 rows for acme's session; acme owner sees 2")

	// (6) Bad policy: an out-of-range sample_rate (101) is rejected 400 at write
	// time by the /admin/evals/policy API.
	adminPost(t, ctlAddr, acmeAdmin.Plaintext, "/admin/evals/policy",
		map[string]any{
			"agent":       "ev1",
			"sample_rate": 101,
			"criteria": []map[string]any{
				{"name": "c", "scorer": "contains", "pattern": "x"},
			},
		}, http.StatusBadRequest)
	t.Log("bad-policy OK: sample_rate 101 rejected 400")
}

// getOnlineResults GETs /admin/evals/online-results?session=<sid> with a bearer,
// asserts 200, and decodes the result rows.
func getOnlineResults(t *testing.T, ctlAddr, bearer, sessionID string) []store.OnlineResult {
	t.Helper()
	resp := authReq(t, "GET",
		"http://"+ctlAddr+"/admin/evals/online-results?session="+sessionID, bearer, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get online-results %s: status = %d, want 200", sessionID, resp.StatusCode)
	}
	var out []store.OnlineResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("get online-results %s: decode: %v", sessionID, err)
	}
	return out
}
