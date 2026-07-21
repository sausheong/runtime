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
)

// This file is the P3.1-M3 end-to-end proof that failure classification runs
// against a REAL runtimed + agentd + Postgres stack. It extends the M2
// online-sampling e2e harness (test/eval_online_e2e_test.go) — the boot / drive
// / scrape scaffolding is reused verbatim; only the assertions differ.
//
// It proves the two live classification paths and their observability:
//
//	(a) INLINE (no policy): agent "fn1" has NO eval policy. A clean session is
//	    driven to completion; agentd classifies inline with qualityFailed=false ⇒
//	    category "none". GET /admin/evals/failures?agent=fn1 ⇒ {"none":1}.
//	(b) SCORER-TAIL (must-fail policy): agent "fn2" has a sample_rate=100 policy
//	    with one contains criterion the testagent's constant "final answer" output
//	    is GUARANTEED to fail ("banana"). A clean session is driven; the scoring
//	    goroutine writes the failing criterion result, then folds classification
//	    into its tail so classify's qualityFailed reads the just-written row ⇒
//	    category "quality_fail". The tail is ASYNC, so the failures breakdown is
//	    polled (bounded) until quality_fail reaches 1 ⇒ {"quality_fail":1}.
//	(c) METRIC: /metrics carries agent_eval_failures_total with the agent= label —
//	    category="none" for fn1 and category="quality_fail" for fn2 — surviving the
//	    fan-out. Scraped OUTSIDE the identity chain (no bearer).
//	(d) RBAC: a DIFFERENT-tenant admin (globex) reading acme's agent fn1 is
//	    rejected 400 (evalAgentVisible: the agent is invisible cross-tenant), and
//	    the owner still sees its breakdown — proving the 400 is isolation, not a
//	    generic failure. Mirrors the M2 online-results tenant-isolation assertion.
//
// AGENT TOPOLOGY — TWO agents, not one. The online-eval policy resolves at agent
// SPAWN time (agentd reads cfg.EvalPolicy once at construction; there is no live
// reload), and agents spawn at control-plane boot. So both policies must exist in
// eval_policies BEFORE runtimed starts. A single shared agent cannot be "clean
// then policied" across two sessions — its policy is fixed at boot. Using ONE
// policy-free agent (fn1) and ONE must-fail-policy agent (fn2), each seeded before
// boot, keeps the two categories on separate agents so each breakdown is a clean
// single-entry map (no cumulative "none"+"quality_fail" coupling on one agent),
// and lets the metric assertion (c) match per-agent by the agent= label.
//
// SUBJECT FORWARDING is ON: sessions are driven with the acme admin bearer, so
// the edge forwards X-Runtime-Tenant=acme and the classified rows / metric land
// under tenant "acme" — what the failures API filters on and the RBAC probe
// tests. Ports: ctl 8522, agents 8523/8524 — no collision with any sibling test.
//
// DEFERRAL: tool_error / timeout / limit_exceeded are covered at the UNIT level
// (agentruntime/classify precedence table, Task 3). The deterministic testagent's
// classic script always runs the marker tool cleanly and emits "final answer" +
// done, so it cannot produce a tool_result IsError, a per-turn deadline, or a
// budget breach without extra scaffolding; forcing those e2e is out of scope here
// and intentionally deferred to the unit coverage.

// evalFailuresResetDB drops every shared table so this test starts blank and
// leaves the DB clean for siblings (leftover identity rows would flip a later
// open-mode runtimed into enforced mode). Mirrors evalOnlineResetDB.
func evalFailuresResetDB(t *testing.T, db *sql.DB) {
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

// TestFailureClassificationLifecycle drives one policy-free session (fn1) and one
// must-fail-policy session (fn2) end-to-end and asserts the classified category
// via /admin/evals/failures, the agent_eval_failures_total metric, and RBAC.
func TestFailureClassificationLifecycle(t *testing.T) {
	ctx := context.Background()

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatalf("ping postgres (%s): %v", dsn, err)
	}
	evalFailuresResetDB(t, db)
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

	// Identity: two tenants, each with an ADMIN service key. acme owns both target
	// agents; globex is the cross-tenant RBAC probe.
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

	// Seed the must-fail policy for fn2 ONLY, DIRECTLY in the DB BEFORE boot (the
	// resolver reads it at spawn time). sample_rate 100 (this one session is always
	// sampled) with a single contains criterion the constant output "final answer"
	// can never match ⇒ the criterion FAILS ⇒ classify at the scorer tail returns
	// "quality_fail". fn1 gets NO policy ⇒ inline classify ⇒ "none".
	ps, err := eval.NewPolicyStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if err := ps.PutPolicy(ctx, eval.Policy{
		Tenant: "acme", AgentID: "fn2", SampleRate: 100,
		Criteria: []eval.Criterion{
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
		"  - {id: fn1, name: Fn1, model: test/scripted, listen_addr: 127.0.0.1:8523, tenant: acme}\n" +
		"  - {id: fn2, name: Fn2, model: test/scripted, listen_addr: 127.0.0.1:8524, tenant: acme}\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	ctlAddr := "127.0.0.1:8522"
	cmd := exec.Command(runtimed)
	cmd.Env = append(os.Environ(),
		"RUNTIME_PG_DSN="+dsn,
		"RUNTIME_CTL_ADDR="+ctlAddr,
		"RUNTIME_AGENTD_BIN="+agentd,
		"RUNTIME_CONFIG="+cfgPath,
		"RUNTIME_SUBJECT_FORWARDING=1", // edge forwards X-Runtime-Tenant ⇒ rows stamped tenant=acme
	) // no TESTAGENT_MODE ⇒ classic script ⇒ output "final answer"
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); _, _ = cmd.Process.Wait() }()

	base := "http://" + ctlAddr
	waitURL(t, base+"/healthz", 20*time.Second)
	// Both agents must be spawned + healthy before we drive sessions through them.
	for _, id := range []string{"fn1", "fn2"} {
		id := id
		if !asEventually(t, 40*time.Second, func() bool {
			return agentHealthyWithBearer(t, ctlAddr, acmeAdmin.Plaintext, id)
		}) {
			t.Fatalf("agent %s never became healthy", id)
		}
	}

	// (a) INLINE / no policy: drive one clean session on fn1 to completion, then
	// assert the failures breakdown is exactly {"none":1}.
	sid1 := driveSessionToCompletion(t, ctx, db, ctlAddr, acmeAdmin.Plaintext, "fn1")
	t.Logf("fn1 session %s completed", sid1)
	// The inline classify runs synchronously on the terminal turn, but the store
	// write + the driver's completion observation can interleave; poll briefly.
	var b1 map[string]int
	if !asEventually(t, 15*time.Second, func() bool {
		b1 = getFailures(t, ctlAddr, acmeAdmin.Plaintext, "fn1", http.StatusOK)
		return b1["none"] == 1
	}) {
		t.Fatalf("fn1 failures breakdown = %+v, want {none:1}", b1)
	}
	if len(b1) != 1 {
		t.Fatalf("fn1 failures breakdown = %+v, want exactly {none:1}", b1)
	}
	t.Logf("(a) OK: fn1 (no policy) classified none: %+v", b1)

	// (b) SCORER-TAIL / must-fail policy: drive one clean session on fn2. The
	// scoring goroutine writes the failing criterion then classifies at its tail —
	// ASYNC — so poll (bounded, ~25×200ms) until quality_fail reaches 1.
	sid2 := driveSessionToCompletion(t, ctx, db, ctlAddr, acmeAdmin.Plaintext, "fn2")
	t.Logf("fn2 session %s completed", sid2)
	var b2 map[string]int
	if !asEventually(t, 20*time.Second, func() bool {
		b2 = getFailures(t, ctlAddr, acmeAdmin.Plaintext, "fn2", http.StatusOK)
		return b2["quality_fail"] == 1
	}) {
		t.Fatalf("fn2 failures breakdown = %+v, want {quality_fail:1}", b2)
	}
	if len(b2) != 1 {
		t.Fatalf("fn2 failures breakdown = %+v, want exactly {quality_fail:1}", b2)
	}
	t.Logf("(b) OK: fn2 (must-fail policy) classified quality_fail at scorer tail: %+v", b2)

	// (c) METRIC: agent_eval_failures_total must carry the agent= label and survive
	// the fan-out — category="none" for fn1, category="quality_fail" for fn2, both
	// tenant=acme, strictly positive. /metrics is served OUTSIDE the identity chain.
	if !asEventually(t, 20*time.Second, func() bool {
		body := getBody(t, base+"/metrics", nil, 200)
		return evalHasPositiveSeries(body, "agent_eval_failures_total", "acme", `agent="fn1"`) &&
			evalHasPositiveSeries(body, "agent_eval_failures_total", "acme", `category="none"`) &&
			evalHasPositiveSeries(body, "agent_eval_failures_total", "acme", `agent="fn2"`) &&
			evalHasPositiveSeries(body, "agent_eval_failures_total", "acme", `category="quality_fail"`)
	}) {
		body := getBody(t, base+"/metrics", nil, 200)
		var got []string
		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(line, "agent_eval_failures_total") {
				got = append(got, line)
			}
		}
		t.Fatalf("/metrics missing agent_eval_failures_total{agent=fn1,category=none} / "+
			"{agent=fn2,category=quality_fail} for tenant acme; lines present:\n%s",
			strings.Join(got, "\n"))
	}
	t.Log("(c) OK: agent_eval_failures_total{agent=fn1,category=none} + {agent=fn2,category=quality_fail} survived fan-out")

	// (d) RBAC: the globex admin reading acme's agent fn1 is rejected 400 (the
	// agent is invisible cross-tenant). Sanity: the acme owner still sees its
	// breakdown, proving the 400 is isolation and not a generic failure.
	getFailures(t, ctlAddr, globexAdmin.Plaintext, "fn1", http.StatusBadRequest)
	if owner := getFailures(t, ctlAddr, acmeAdmin.Plaintext, "fn1", http.StatusOK); owner["none"] != 1 {
		t.Fatalf("owner failures breakdown = %+v, want {none:1} (proves the 400 above is isolation)", owner)
	}
	t.Log("(d) OK: globex admin rejected 400 on acme's fn1; acme owner still sees {none:1}")
}

// driveSessionToCompletion POSTs a session to /agents/{id}/sessions with a bearer,
// asserts the session id, and waits (bounded) for the DB row to reach
// status=completed. Returns the session id. Mirrors the M2 drive + terminal poll.
func driveSessionToCompletion(t *testing.T, ctx context.Context, db *sql.DB, ctlAddr, bearer, agentID string) string {
	t.Helper()
	var sc struct {
		SessionID string `json:"session_id"`
	}
	adminPostJSON(t, ctlAddr, bearer, "/agents/"+agentID+"/sessions",
		map[string]string{"message": "classify me"}, http.StatusOK, &sc)
	if sc.SessionID == "" {
		t.Fatalf("agent %s: session create returned empty session_id", agentID)
	}
	sid := sc.SessionID
	if !asEventually(t, 60*time.Second, func() bool {
		var status string
		if err := db.QueryRowContext(ctx, `SELECT status FROM sessions WHERE id=$1`, sid).Scan(&status); err != nil {
			return false
		}
		return status == "completed"
	}) {
		var status string
		_ = db.QueryRowContext(ctx, `SELECT status FROM sessions WHERE id=$1`, sid).Scan(&status)
		t.Fatalf("agent %s: session %s never completed; last status %q", agentID, sid, status)
	}
	return sid
}

// getFailures GETs /admin/evals/failures?agent=<id> with a bearer, asserts the
// status code, and (on 200) decodes the category→count breakdown map.
func getFailures(t *testing.T, ctlAddr, bearer, agentID string, wantStatus int) map[string]int {
	t.Helper()
	resp := authReq(t, "GET",
		"http://"+ctlAddr+"/admin/evals/failures?agent="+agentID, bearer, nil)
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		t.Fatalf("get failures %s: status = %d, want %d", agentID, resp.StatusCode, wantStatus)
	}
	if wantStatus != http.StatusOK {
		return nil
	}
	var out map[string]int
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("get failures %s: decode: %v", agentID, err)
	}
	return out
}
