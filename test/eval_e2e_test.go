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

// This file is the P3.1-M1 end-to-end proof that the golden-set evaluator runs
// against a REAL runtimed + agentd + Postgres stack: a tenant admin creates a
// set of mixed deterministic scorers, starts a run against a live testagent,
// and the run completes with the exact pass/fail tally the testagent's known
// output produces — with no judge/grader model in the loop.
//
// The testagent (TESTAGENT_MODE unset — the classic 2-turn script) is a pure
// function of its input: it calls the "marker" tool, then emits the final text
// "final answer" and a done event (see testagent/provider.go and the resume
// e2e). The eval invoker (controlplane/evalinvoker.go) concatenates the "text"
// events, so every case's agent output is EXACTLY "final answer". Scorer
// expectations below are chosen against that constant, so pass/fail is
// deterministic and needs no LLM judge.
//
// Boot scaffolding mirrors test/policy_test.go (buildBin, identity store with
// admin service keys, runtimed in its own process group with RUNTIME_* env);
// admin calls use the sibling helpers adminPost/adminPostJSON/authReq/getBody
// and asEventually/waitURL.
//
// Ports: ctl 8500, agent 8501 — no collision with any other integration test.

// evalResetDB drops the shared tables (durable + identity + eval) so this test
// starts from a blank slate and leaves the DB clean for siblings (leftover
// identity rows would flip a later open-mode runtimed into enforced mode).
func evalResetDB(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, q := range []string{
		`DROP TABLE IF EXISTS markers`,
		`DROP TABLE IF EXISTS session_events, sessions, agents CASCADE`,
		`DROP SCHEMA IF EXISTS dbos CASCADE`,
		`DROP TABLE IF EXISTS eval_results, eval_runs, eval_sets CASCADE`,
		`DROP TABLE IF EXISTS service_keys CASCADE`,
		`DROP TABLE IF EXISTS identity_users CASCADE`,
		`DROP TABLE IF EXISTS tenants CASCADE`,
	} {
		mustExec(t, db, q)
	}
}

// TestEvalLifecycle drives one golden-set run end-to-end against a live agent
// and asserts every observable outcome:
//
//	(1) a set with mixed deterministic scorers (exact/contains/regex) is created;
//	(2) a run against the live testagent completes (bounded poll);
//	(3) the aggregate tally (total/passed/failed/score) matches the testagent's
//	    known "final answer" output scored against the set;
//	(4) results carry one row per case with the expected per-case pass/fail;
//	(5) /metrics carries incremented runtime_eval_runs_total and
//	    runtime_eval_cases_total;
//	(6) a second tenant admin cannot read the first tenant's run or set (404);
//	(7) a set with an invalid regex expectation is rejected 400.
func TestEvalLifecycle(t *testing.T) {
	ctx := context.Background()

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatalf("ping postgres (%s): %v", dsn, err)
	}
	evalResetDB(t, db)
	t.Cleanup(func() {
		cdb, err := sql.Open("pgx", dsn)
		if err != nil {
			return
		}
		defer cdb.Close()
		for _, q := range []string{
			`DROP TABLE IF EXISTS eval_results, eval_runs, eval_sets CASCADE`,
			`DROP TABLE IF EXISTS service_keys CASCADE`,
			`DROP TABLE IF EXISTS identity_users CASCADE`,
			`DROP TABLE IF EXISTS tenants CASCADE`,
		} {
			_, _ = cdb.Exec(q)
		}
	})

	// Identity: two tenants, each with an ADMIN service key. acme owns the eval
	// set/run + the target agent; globex is the isolation probe.
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

	tmp := t.TempDir()
	agentd := buildBin(t, tmp, "agentd")
	runtimed := buildBin(t, tmp, "runtimed")

	cfgPath := filepath.Join(tmp, "runtime.yaml")
	cfg := "agents:\n" +
		"  - {id: ev1, name: Ev1, model: test/scripted, listen_addr: 127.0.0.1:8501, tenant: acme}\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	ctlAddr := "127.0.0.1:8500"
	cmd := exec.Command(runtimed)
	cmd.Env = append(os.Environ(),
		"RUNTIME_PG_DSN="+dsn,
		"RUNTIME_CTL_ADDR="+ctlAddr,
		"RUNTIME_AGENTD_BIN="+agentd,
		"RUNTIME_CONFIG="+cfgPath,
	) // no TESTAGENT_MODE ⇒ classic script ⇒ output "final answer"
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); _, _ = cmd.Process.Wait() }()

	base := "http://" + ctlAddr
	waitURL(t, base+"/healthz", 20*time.Second)
	// The invoker resolves a live replica from the registry, so the agent must be
	// registered + healthy before the run starts.
	if !asEventually(t, 30*time.Second, func() bool {
		return agentHealthyWithBearer(t, ctlAddr, acmeAdmin.Plaintext, "ev1")
	}) {
		t.Fatal("agent ev1 never became healthy")
	}

	// (1) Create a set with mixed deterministic scorers. Every case scores the
	// testagent's constant output "final answer": three PASS (exact/contains/
	// regex all match) and two FAIL (exact + contains that cannot match) — a
	// definite 3/5 with score 0.6.
	cases := []eval.Case{
		{Input: "q-exact-hit", Scorer: eval.ScorerExact, Expected: "final answer"},   // PASS
		{Input: "q-contains-hit", Scorer: eval.ScorerContains, Expected: "final"},     // PASS
		{Input: "q-regex-hit", Scorer: eval.ScorerRegex, Expected: "answer$"},          // PASS
		{Input: "q-exact-miss", Scorer: eval.ScorerExact, Expected: "wrong answer"},    // FAIL
		{Input: "q-contains-miss", Scorer: eval.ScorerContains, Expected: "banana"},    // FAIL
	}
	wantPass := []bool{true, true, true, false, false}
	const wantTotal, wantPassed, wantFailed = 5, 3, 2

	adminPost(t, ctlAddr, acmeAdmin.Plaintext, "/admin/evals/sets",
		map[string]any{"name": "golden", "cases": cases}, http.StatusCreated)

	// (2) Start a run against the live agent; capture run_id (202).
	var rr struct {
		RunID string `json:"run_id"`
	}
	adminPostJSON(t, ctlAddr, acmeAdmin.Plaintext, "/admin/evals/runs",
		map[string]any{"set": "golden", "agent": "ev1"}, http.StatusAccepted, &rr)
	if rr.RunID == "" {
		t.Fatal("run create returned empty run_id")
	}
	t.Logf("run id = %s", rr.RunID)

	// Poll GET /admin/evals/runs/{id} to completed (bounded).
	var run eval.Run
	if !asEventually(t, 60*time.Second, func() bool {
		run = evalGetRun(t, ctlAddr, acmeAdmin.Plaintext, rr.RunID)
		return run.Status == eval.StatusCompleted || run.Status == eval.StatusError
	}) {
		t.Fatalf("run never reached a terminal state; last status %q", run.Status)
	}
	if run.Status != eval.StatusCompleted {
		t.Fatalf("run status = %q (err=%q), want %q", run.Status, run.Error, eval.StatusCompleted)
	}

	// (3) Aggregate tally matches the testagent's known output.
	if run.Total != wantTotal || run.Passed != wantPassed || run.Failed != wantFailed {
		t.Fatalf("aggregate = {total:%d passed:%d failed:%d}, want {total:%d passed:%d failed:%d}",
			run.Total, run.Passed, run.Failed, wantTotal, wantPassed, wantFailed)
	}
	if run.Score < 0.599 || run.Score > 0.601 { // 3/5 = 0.6
		t.Fatalf("score = %v, want 0.6", run.Score)
	}
	t.Logf("aggregate OK: total=%d passed=%d failed=%d score=%v",
		run.Total, run.Passed, run.Failed, run.Score)

	// (4) Results: one row per case, each with the expected pass/fail and the
	// agent's actual output.
	results := evalGetResults(t, ctlAddr, acmeAdmin.Plaintext, rr.RunID)
	if len(results) != wantTotal {
		t.Fatalf("results len = %d, want %d: %+v", len(results), wantTotal, results)
	}
	// Results are keyed by CaseIndex; index into wantPass by that, not by slice
	// position (ordering is not contractually position-stable).
	seen := make(map[int]bool, wantTotal)
	for _, res := range results {
		if res.CaseIndex < 0 || res.CaseIndex >= wantTotal {
			t.Fatalf("result has out-of-range case_index %d: %+v", res.CaseIndex, res)
		}
		if seen[res.CaseIndex] {
			t.Fatalf("duplicate result for case_index %d", res.CaseIndex)
		}
		seen[res.CaseIndex] = true
		if res.Passed != wantPass[res.CaseIndex] {
			t.Fatalf("case %d passed=%v, want %v (output=%q scorer=%q)",
				res.CaseIndex, res.Passed, wantPass[res.CaseIndex], res.Output, res.Scorer)
		}
		if res.Output != "final answer" {
			t.Fatalf("case %d output = %q, want %q (testagent constant)",
				res.CaseIndex, res.Output, "final answer")
		}
	}
	if len(seen) != wantTotal {
		t.Fatalf("results did not cover every case: got indices %v", seen)
	}
	t.Log("results OK: one row per case, per-case pass/fail as expected")

	// (5) /metrics carries the incremented eval counters (fan-out sub-scrape may
	// lag termination by a beat, so poll briefly). /metrics is served OUTSIDE the
	// identity chain, so it needs no bearer.
	if !asEventually(t, 15*time.Second, func() bool {
		body := getBody(t, base+"/metrics", nil, 200)
		return evalHasPositiveSeries(body, "runtime_eval_runs_total", "acme", `status="completed"`) &&
			evalHasPositiveSeries(body, "runtime_eval_cases_total", "acme", `result="pass"`) &&
			evalHasPositiveSeries(body, "runtime_eval_cases_total", "acme", `result="fail"`)
	}) {
		body := getBody(t, base+"/metrics", nil, 200)
		var got []string
		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(line, "runtime_eval_runs_total") ||
				strings.HasPrefix(line, "runtime_eval_cases_total") {
				got = append(got, line)
			}
		}
		t.Fatalf("/metrics missing incremented runtime_eval_runs_total / runtime_eval_cases_total "+
			"for tenant acme; eval lines present:\n%s", strings.Join(got, "\n"))
	}
	t.Log("metric OK: runtime_eval_runs_total{...} and runtime_eval_cases_total{...} incremented")

	// (6) Tenant isolation: globex admin cannot read acme's run nor set (404,
	// no cross-tenant oracle).
	if resp := authReq(t, "GET", base+"/admin/evals/runs/"+rr.RunID, globexAdmin.Plaintext, nil); resp.StatusCode != http.StatusNotFound {
		resp.Body.Close()
		t.Fatalf("cross-tenant run get: status = %d, want 404", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if resp := authReq(t, "GET", base+"/admin/evals/runs/"+rr.RunID+"/results", globexAdmin.Plaintext, nil); resp.StatusCode != http.StatusNotFound {
		resp.Body.Close()
		t.Fatalf("cross-tenant results get: status = %d, want 404", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if resp := authReq(t, "GET", base+"/admin/evals/sets/golden", globexAdmin.Plaintext, nil); resp.StatusCode != http.StatusNotFound {
		resp.Body.Close()
		t.Fatalf("cross-tenant set get: status = %d, want 404", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	// Sanity: acme itself CAN read its own run (proves the 404s above are
	// isolation, not a broken run id).
	if resp := authReq(t, "GET", base+"/admin/evals/runs/"+rr.RunID, acmeAdmin.Plaintext, nil); resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("owner run get: status = %d, want 200", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	t.Log("isolation OK: globex admin gets 404 on acme's run/results/set; acme owner gets 200")

	// (7) Bad input: a set with an uncompilable regex expectation is rejected 400
	// at write time.
	adminPost(t, ctlAddr, acmeAdmin.Plaintext, "/admin/evals/sets",
		map[string]any{
			"name":  "bad",
			"cases": []eval.Case{{Input: "x", Scorer: eval.ScorerRegex, Expected: "("}},
		}, http.StatusBadRequest)
	t.Log("bad-input OK: invalid regex set rejected 400")
}

// evalGetRun GETs /admin/evals/runs/{id} with a bearer, asserts 200, and decodes
// the run. Fails the test on any transport/status/decode error.
func evalGetRun(t *testing.T, ctlAddr, bearer, runID string) eval.Run {
	t.Helper()
	resp := authReq(t, "GET", "http://"+ctlAddr+"/admin/evals/runs/"+runID, bearer, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get run %s: status = %d, want 200", runID, resp.StatusCode)
	}
	var run eval.Run
	if err := json.NewDecoder(resp.Body).Decode(&run); err != nil {
		t.Fatalf("get run %s: decode: %v", runID, err)
	}
	return run
}

// evalGetResults GETs /admin/evals/runs/{id}/results with a bearer, asserts 200,
// and decodes the result rows.
func evalGetResults(t *testing.T, ctlAddr, bearer, runID string) []eval.Result {
	t.Helper()
	resp := authReq(t, "GET", "http://"+ctlAddr+"/admin/evals/runs/"+runID+"/results", bearer, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get results %s: status = %d, want 200", runID, resp.StatusCode)
	}
	var results []eval.Result
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		t.Fatalf("get results %s: decode: %v", runID, err)
	}
	return results
}

// evalHasPositiveSeries reports whether the Prometheus exposition carries a
// series with the given metric name that contains both tenant and the extra
// label fragment, with a strictly-positive sample value. The fan-out may inject
// extra labels server-side, so we match on label content, not an exact string.
func evalHasPositiveSeries(body, metric, tenant, labelFrag string) bool {
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, metric+"{") {
			continue
		}
		if strings.Contains(line, `tenant="`+tenant+`"`) &&
			strings.Contains(line, labelFrag) &&
			evalPositiveSample(line) {
			return true
		}
	}
	return false
}

// evalPositiveSample reports whether a text-exposition line's trailing sample
// value is a number > 0.
func evalPositiveSample(line string) bool {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return false
	}
	v := fields[len(fields)-1]
	if strings.HasPrefix(v, "-") || v == "0" || v == "0.0" {
		return false
	}
	for _, r := range v {
		if r >= '1' && r <= '9' {
			return true
		}
	}
	return false
}
