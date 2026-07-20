//go:build integration

package eval

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// freshPolicyStore opens the DB, drops + recreates eval_policies via
// NewPolicyStore, and returns a PolicyStore. t.Cleanup drops the table so
// sibling tests start clean. Reuses testDSN() from store_test.go.
func freshPolicyStore(t *testing.T) (*PolicyStore, *sql.DB) {
	t.Helper()
	db, err := sql.Open("pgx", testDSN())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PingContext(context.Background()); err != nil {
		t.Skipf("postgres not reachable: %v", err)
	}
	drop := func() { _, _ = db.Exec(`DROP TABLE IF EXISTS eval_policies CASCADE`) }
	drop()
	t.Cleanup(drop)
	st, err := NewPolicyStore(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	return st, db
}

func TestPolicyStoreRoundTrip(t *testing.T) {
	st, db := freshPolicyStore(t)
	defer db.Close()
	ctx := context.Background()

	// Bad policy rejected at write time (no agent_id, no criteria).
	if err := st.PutPolicy(ctx, Policy{Tenant: "t", AgentID: "", SampleRate: 10}); err == nil {
		t.Fatal("expected validation rejection for empty agent_id")
	}

	p := Policy{Tenant: "t", AgentID: "a1", SampleRate: 25, Criteria: []Criterion{
		{Name: "contains-ok", Scorer: ScorerContains, Pattern: "ok"},
		{Name: "regex-num", Scorer: ScorerRegex, Pattern: `\d+`},
		{Name: "judge-helpful", Scorer: ScorerJudge, Rubric: "is it helpful?"},
	}}
	if err := st.PutPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}

	got, ok, err := st.GetPolicy(ctx, "t", "a1")
	if err != nil || !ok {
		t.Fatalf("get policy: ok=%v err=%v", ok, err)
	}
	if got.Tenant != "t" || got.AgentID != "a1" || got.SampleRate != 25 {
		t.Fatalf("policy identity/rate wrong: %+v", got)
	}
	if got.CreatedAt.IsZero() {
		t.Fatal("created_at not populated from DB default")
	}
	if len(got.Criteria) != 3 {
		t.Fatalf("criteria did not round-trip through JSONB: got %d, want 3: %+v", len(got.Criteria), got.Criteria)
	}
	if got.Criteria[0].Name != "contains-ok" || got.Criteria[0].Scorer != ScorerContains || got.Criteria[0].Pattern != "ok" {
		t.Fatalf("criterion[0] round-trip mismatch: %+v", got.Criteria[0])
	}
	if got.Criteria[1].Scorer != ScorerRegex || got.Criteria[1].Pattern != `\d+` {
		t.Fatalf("criterion[1] round-trip mismatch: %+v", got.Criteria[1])
	}
	if got.Criteria[2].Scorer != ScorerJudge || got.Criteria[2].Rubric != "is it helpful?" {
		t.Fatalf("criterion[2] round-trip mismatch: %+v", got.Criteria[2])
	}

	// Missing policy: ok=false, no error.
	if _, ok, err := st.GetPolicy(ctx, "t", "nope"); err != nil || ok {
		t.Fatalf("missing policy: ok=%v err=%v", ok, err)
	}

	// ListPolicies(tenant) finds it.
	byTenant, err := st.ListPolicies(ctx, "t")
	if err != nil {
		t.Fatal(err)
	}
	if len(byTenant) != 1 || byTenant[0].AgentID != "a1" || len(byTenant[0].Criteria) != 3 {
		t.Fatalf("ListPolicies(t) wrong: %+v", byTenant)
	}
	// ListPolicies("") includes it.
	all, err := st.ListPolicies(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].AgentID != "a1" {
		t.Fatalf("ListPolicies(\"\") wrong: %+v", all)
	}

	// DeletePolicy returns true, then GetPolicy ok=false.
	deleted, err := st.DeletePolicy(ctx, "t", "a1")
	if err != nil || !deleted {
		t.Fatalf("delete policy: deleted=%v err=%v", deleted, err)
	}
	if _, ok, _ := st.GetPolicy(ctx, "t", "a1"); ok {
		t.Fatal("policy still present after delete")
	}
	// Deleting again returns false.
	if deleted, _ := st.DeletePolicy(ctx, "t", "a1"); deleted {
		t.Fatal("second delete should return false")
	}
}

func TestPolicyStoreUpsert(t *testing.T) {
	st, db := freshPolicyStore(t)
	defer db.Close()
	ctx := context.Background()

	v1 := Policy{Tenant: "t", AgentID: "a", SampleRate: 10, Criteria: []Criterion{
		{Name: "c1", Scorer: ScorerContains, Pattern: "hi"},
	}}
	if err := st.PutPolicy(ctx, v1); err != nil {
		t.Fatal(err)
	}
	// Same (tenant,agent_id), new rate + criteria → upsert, no dup-key error.
	v2 := Policy{Tenant: "t", AgentID: "a", SampleRate: 90, Criteria: []Criterion{
		{Name: "c2", Scorer: ScorerRegex, Pattern: `^x`},
		{Name: "c3", Scorer: ScorerJudge, Rubric: "concise?"},
	}}
	if err := st.PutPolicy(ctx, v2); err != nil {
		t.Fatalf("upsert must not error on duplicate (tenant,agent_id): %v", err)
	}
	got, ok, err := st.GetPolicy(ctx, "t", "a")
	if err != nil || !ok {
		t.Fatalf("get after upsert: ok=%v err=%v", ok, err)
	}
	if got.SampleRate != 90 {
		t.Fatalf("upsert did not replace sample_rate: %d", got.SampleRate)
	}
	if len(got.Criteria) != 2 || got.Criteria[0].Name != "c2" || got.Criteria[1].Name != "c3" {
		t.Fatalf("upsert did not replace criteria: %+v", got.Criteria)
	}
	// Still exactly one row for (tenant,agent_id).
	if ps, _ := st.ListPolicies(ctx, "t"); len(ps) != 1 {
		t.Fatalf("upsert created a duplicate row: %+v", ps)
	}
}

func TestPolicyStoreCrossTenantIsolation(t *testing.T) {
	st, db := freshPolicyStore(t)
	defer db.Close()
	ctx := context.Background()

	crit := []Criterion{{Name: "c", Scorer: ScorerContains, Pattern: "ok"}}
	if err := st.PutPolicy(ctx, Policy{Tenant: "t1", AgentID: "a1", SampleRate: 5, Criteria: crit}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutPolicy(ctx, Policy{Tenant: "t2", AgentID: "a1", SampleRate: 50, Criteria: crit}); err != nil {
		t.Fatal(err)
	}

	// Same agent_id under a different tenant does not leak.
	got, ok, err := st.GetPolicy(ctx, "t1", "a1")
	if err != nil || !ok || got.SampleRate != 5 {
		t.Fatalf("t1/a1 wrong: ok=%v rate=%d err=%v", ok, got.SampleRate, err)
	}
	if got2, _, _ := st.GetPolicy(ctx, "t2", "a1"); got2.SampleRate != 50 {
		t.Fatalf("t2/a1 wrong rate: %d", got2.SampleRate)
	}

	// ListPolicies is tenant-scoped.
	t1, _ := st.ListPolicies(ctx, "t1")
	if len(t1) != 1 || t1[0].Tenant != "t1" {
		t.Fatalf("ListPolicies(t1) leaked cross-tenant: %+v", t1)
	}
	all, _ := st.ListPolicies(ctx, "")
	if len(all) != 2 {
		t.Fatalf("ListPolicies(\"\") want 2, got %d: %+v", len(all), all)
	}
	// ORDER BY tenant, agent_id.
	if all[0].Tenant != "t1" || all[1].Tenant != "t2" {
		t.Fatalf("ListPolicies(\"\") not ordered by tenant: %+v", all)
	}

	// Deleting t1's policy leaves t2's intact.
	if _, err := st.DeletePolicy(ctx, "t1", "a1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.GetPolicy(ctx, "t2", "a1"); !ok {
		t.Fatal("deleting t1 policy removed t2 policy")
	}
}
