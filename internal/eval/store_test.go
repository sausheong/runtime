//go:build integration

package eval

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func testDSN() string {
	if v := os.Getenv("RUNTIME_TEST_PG_DSN"); v != "" {
		return v
	}
	return "postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable"
}

// freshStore opens the DB, drops + recreates the eval tables via NewStore, and
// returns a Store. t.Cleanup drops the tables so sibling tests start clean.
func freshStore(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	db, err := sql.Open("pgx", testDSN())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PingContext(context.Background()); err != nil {
		t.Skipf("postgres not reachable: %v", err)
	}
	drop := func() {
		_, _ = db.Exec(`DROP TABLE IF EXISTS eval_results CASCADE`)
		_, _ = db.Exec(`DROP TABLE IF EXISTS eval_runs CASCADE`)
		_, _ = db.Exec(`DROP TABLE IF EXISTS eval_sets CASCADE`)
	}
	drop()
	t.Cleanup(drop)
	st, err := NewStore(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	return st, db
}

func TestStoreSetsRoundTrip(t *testing.T) {
	st, db := freshStore(t)
	defer db.Close()
	ctx := context.Background()

	// Bad set rejected at write time.
	if err := st.PutSet(ctx, Set{Tenant: "t", Name: "", Cases: nil}); err == nil {
		t.Fatal("expected validation rejection for empty name")
	}

	set := Set{Tenant: "t", Name: "greet", Cases: []Case{
		{Input: "hi", Scorer: ScorerExact, Expected: "hello"},
		{Input: "bye", Scorer: ScorerContains, Expected: "later"},
		{Input: "num", Scorer: ScorerRegex, Expected: `\d+`},
	}}
	if err := st.PutSet(ctx, set); err != nil {
		t.Fatal(err)
	}

	got, ok, err := st.GetSet(ctx, "t", "greet")
	if err != nil || !ok {
		t.Fatalf("get set: ok=%v err=%v", ok, err)
	}
	if got.Tenant != "t" || got.Name != "greet" {
		t.Fatalf("set identity wrong: %+v", got)
	}
	if got.CreatedAt.IsZero() {
		t.Fatal("created_at not populated from DB default")
	}
	if len(got.Cases) != 3 {
		t.Fatalf("cases did not round-trip through JSONB: got %d, want 3: %+v", len(got.Cases), got.Cases)
	}
	if got.Cases[0].Input != "hi" || got.Cases[0].Scorer != ScorerExact || got.Cases[0].Expected != "hello" {
		t.Fatalf("case[0] round-trip mismatch: %+v", got.Cases[0])
	}
	if got.Cases[2].Scorer != ScorerRegex || got.Cases[2].Expected != `\d+` {
		t.Fatalf("case[2] round-trip mismatch: %+v", got.Cases[2])
	}

	// Missing set: ok=false, no error.
	if _, ok, err := st.GetSet(ctx, "t", "nope"); err != nil || ok {
		t.Fatalf("missing set: ok=%v err=%v", ok, err)
	}

	// ListSets(tenant) finds it.
	byTenant, err := st.ListSets(ctx, "t")
	if err != nil {
		t.Fatal(err)
	}
	if len(byTenant) != 1 || byTenant[0].Name != "greet" || len(byTenant[0].Cases) != 3 {
		t.Fatalf("ListSets(t) wrong: %+v", byTenant)
	}
	// ListSets("") includes it.
	all, err := st.ListSets(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Name != "greet" {
		t.Fatalf("ListSets(\"\") wrong: %+v", all)
	}
	// Cross-tenant is empty.
	if other, _ := st.ListSets(ctx, "other"); len(other) != 0 {
		t.Fatalf("cross-tenant ListSets leaked: %+v", other)
	}

	// DeleteSet returns true, then GetSet ok=false.
	deleted, err := st.DeleteSet(ctx, "t", "greet")
	if err != nil || !deleted {
		t.Fatalf("delete set: deleted=%v err=%v", deleted, err)
	}
	if _, ok, _ := st.GetSet(ctx, "t", "greet"); ok {
		t.Fatal("set still present after delete")
	}
	// Deleting again returns false.
	if deleted, _ := st.DeleteSet(ctx, "t", "greet"); deleted {
		t.Fatal("second delete should return false")
	}
}

func TestStoreSetUpsert(t *testing.T) {
	st, db := freshStore(t)
	defer db.Close()
	ctx := context.Background()

	v1 := Set{Tenant: "t", Name: "s", Cases: []Case{{Input: "a", Scorer: ScorerExact, Expected: "1"}}}
	if err := st.PutSet(ctx, v1); err != nil {
		t.Fatal(err)
	}
	// Same (tenant,name), different cases → upsert, no dup-key error.
	v2 := Set{Tenant: "t", Name: "s", Cases: []Case{
		{Input: "b", Scorer: ScorerExact, Expected: "2"},
		{Input: "c", Scorer: ScorerContains, Expected: "3"},
	}}
	if err := st.PutSet(ctx, v2); err != nil {
		t.Fatalf("upsert must not error on duplicate (tenant,name): %v", err)
	}
	got, ok, err := st.GetSet(ctx, "t", "s")
	if err != nil || !ok {
		t.Fatalf("get after upsert: ok=%v err=%v", ok, err)
	}
	if len(got.Cases) != 2 || got.Cases[0].Input != "b" || got.Cases[1].Input != "c" {
		t.Fatalf("upsert did not replace cases: %+v", got.Cases)
	}
	// Still exactly one row for (tenant,name).
	if sets, _ := st.ListSets(ctx, "t"); len(sets) != 1 {
		t.Fatalf("upsert created a duplicate row: %+v", sets)
	}
}

func TestStoreRunsAndResults(t *testing.T) {
	st, db := freshStore(t)
	defer db.Close()
	ctx := context.Background()

	// Create a pending run, get it back.
	run := Run{RunID: "r1", Tenant: "t", SetName: "greet", AgentID: "a1", Status: StatusPending}
	if err := st.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	gr, ok, err := st.GetRun(ctx, "r1")
	if err != nil || !ok {
		t.Fatalf("get run: ok=%v err=%v", ok, err)
	}
	if gr.Status != StatusPending || gr.SetName != "greet" || gr.AgentID != "a1" {
		t.Fatalf("run fields wrong: %+v", gr)
	}
	if gr.CreatedAt.IsZero() {
		t.Fatal("run created_at not populated")
	}
	if gr.FinishedAt != nil {
		t.Fatalf("pending run must have nil FinishedAt: %v", gr.FinishedAt)
	}
	// Missing run: ok=false, no error.
	if _, ok, err := st.GetRun(ctx, "nope"); err != nil || ok {
		t.Fatalf("missing run: ok=%v err=%v", ok, err)
	}

	// SetRunStatus.
	if err := st.SetRunStatus(ctx, "r1", StatusRunning); err != nil {
		t.Fatal(err)
	}
	if gr, _, _ := st.GetRun(ctx, "r1"); gr.Status != StatusRunning {
		t.Fatalf("set status: %+v", gr)
	}

	// PutResult ×2 (out of order) → ListResults ascending by case_index.
	if err := st.PutResult(ctx, "r1", Result{CaseIndex: 1, Input: "bye", Output: "later", Scorer: "contains", Passed: false, Detail: "miss"}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutResult(ctx, "r1", Result{CaseIndex: 0, Input: "hi", Output: "hello", Scorer: "exact", Passed: true}); err != nil {
		t.Fatal(err)
	}
	results, err := st.ListResults(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("want 2 results, got %d: %+v", len(results), results)
	}
	if results[0].CaseIndex != 0 || results[1].CaseIndex != 1 {
		t.Fatalf("results not ascending by case_index: %+v", results)
	}
	if !results[0].Passed || results[1].Passed {
		t.Fatalf("result passed flags wrong: %+v", results)
	}
	if results[0].Output != "hello" || results[1].Detail != "miss" {
		t.Fatalf("result fields did not round-trip: %+v", results)
	}

	// PutResult idempotent upsert on (run_id,case_index): re-put case 0 with new output.
	if err := st.PutResult(ctx, "r1", Result{CaseIndex: 0, Input: "hi", Output: "HELLO", Scorer: "exact", Passed: true}); err != nil {
		t.Fatalf("re-put same case must upsert, not error: %v", err)
	}
	results, _ = st.ListResults(ctx, "r1")
	if len(results) != 2 {
		t.Fatalf("upsert must not add a row, got %d: %+v", len(results), results)
	}
	if results[0].Output != "HELLO" {
		t.Fatalf("upsert did not replace output: %+v", results[0])
	}

	// FinishRun → counts/score/FinishedAt.
	if err := st.FinishRun(ctx, "r1", StatusCompleted, 2, 1, 1, 0.5, ""); err != nil {
		t.Fatal(err)
	}
	fr, ok, err := st.GetRun(ctx, "r1")
	if err != nil || !ok {
		t.Fatalf("get after finish: ok=%v err=%v", ok, err)
	}
	if fr.Status != StatusCompleted || fr.Total != 2 || fr.Passed != 1 || fr.Failed != 1 || fr.Score != 0.5 {
		t.Fatalf("finish counts/score wrong: %+v", fr)
	}
	if fr.FinishedAt == nil {
		t.Fatal("FinishedAt must be set after FinishRun")
	}
	if fr.FinishedAt.Before(fr.CreatedAt) {
		t.Fatalf("FinishedAt before CreatedAt: created=%v finished=%v", fr.CreatedAt, *fr.FinishedAt)
	}
}

func TestStoreListRunsOrdering(t *testing.T) {
	st, db := freshStore(t)
	defer db.Close()
	ctx := context.Background()

	if err := st.CreateRun(ctx, Run{RunID: "old", Tenant: "t", SetName: "s", AgentID: "a", Status: StatusPending}); err != nil {
		t.Fatal(err)
	}
	// Force a distinct, later created_at so DESC ordering is unambiguous.
	if _, err := db.Exec(`UPDATE eval_runs SET created_at = now() - interval '1 hour' WHERE run_id='old'`); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateRun(ctx, Run{RunID: "new", Tenant: "t", SetName: "s", AgentID: "a", Status: StatusPending}); err != nil {
		t.Fatal(err)
	}

	runs, err := st.ListRuns(ctx, "t")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("want 2 runs, got %d: %+v", len(runs), runs)
	}
	if runs[0].RunID != "new" || runs[1].RunID != "old" {
		t.Fatalf("ListRuns not recent-first: %+v", runs)
	}
	// Cross-tenant is empty.
	if other, _ := st.ListRuns(ctx, "other"); len(other) != 0 {
		t.Fatalf("cross-tenant ListRuns leaked: %+v", other)
	}
	// ListRuns("") returns all.
	if all, _ := st.ListRuns(ctx, ""); len(all) != 2 {
		t.Fatalf("ListRuns(\"\") want 2, got %d", len(all))
	}
}

func TestStoreResultsCascadeOnRunDelete(t *testing.T) {
	st, db := freshStore(t)
	defer db.Close()
	ctx := context.Background()

	if err := st.CreateRun(ctx, Run{RunID: "r1", Tenant: "t", SetName: "s", AgentID: "a", Status: StatusPending}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutResult(ctx, "r1", Result{CaseIndex: 0, Input: "i", Output: "o", Scorer: "exact", Passed: true}); err != nil {
		t.Fatal(err)
	}
	if res, _ := st.ListResults(ctx, "r1"); len(res) != 1 {
		t.Fatalf("precondition: want 1 result, got %d", len(res))
	}

	// Deleting the run cascades to its results (FK ON DELETE CASCADE).
	if _, err := db.Exec(`DELETE FROM eval_runs WHERE run_id=$1`, "r1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.GetRun(ctx, "r1"); ok {
		t.Fatal("run should be gone after delete")
	}
	res, err := st.ListResults(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 0 {
		t.Fatalf("FK cascade failed: results survived run delete: %+v", res)
	}
}
