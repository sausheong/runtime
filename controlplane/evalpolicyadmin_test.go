package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sausheong/runtime/internal/config"
	"github.com/sausheong/runtime/internal/eval"
	"github.com/sausheong/runtime/internal/identity"
	"github.com/sausheong/runtime/internal/obs"
	"github.com/sausheong/runtime/internal/store"
)

// evalPolicyMux stands up /admin/evals/* backed by a PolicyMemStore and a
// MemStore seeded with online results, so the policy CRUD and online-results
// routes can be exercised without Postgres.
func evalPolicyMux(t *testing.T) (*http.ServeMux, *eval.PolicyMemStore, store.Store) {
	t.Helper()
	mux := http.NewServeMux()
	ps := eval.NewPolicyMemStore()
	ctl := store.NewMemStore()
	reg := NewRegistry(&config.Config{Agents: []config.AgentConfig{
		{ID: "a1", Name: "A1", Model: "m", ListenAddr: "127.0.0.1:8401", Tenant: "t1"},
	}}, "/bin/agentd", "dsn")
	RegisterEvalAdmin(context.Background(), mux, newFakeAdminStore(), eval.NewMemStore(),
		ps, ctl, fakeEvalInvoker{out: "hello world"}, nil, reg, obs.NewControlMetrics())
	return mux, ps, ctl
}

func goodCriteria() []map[string]any {
	return []map[string]any{{"name": "polite", "scorer": "judge", "rubric": "be polite"}}
}

func TestEvalPolicyAdminCRUDAndRBAC(t *testing.T) {
	mux, ps, _ := evalPolicyMux(t)
	t1 := identity.Principal{Role: identity.RoleAdmin, TenantID: "t1"}

	// Valid policy → 201, stored under caller's tenant (empty tenant defaults).
	if w := doJSON(t, mux, "POST", "/admin/evals/policy", t1, map[string]any{
		"agent": "a1", "sample_rate": 25, "criteria": goodCriteria(),
	}); w.Code != http.StatusCreated {
		t.Fatalf("valid policy: want 201 got %d (%s)", w.Code, w.Body)
	}
	if _, ok, _ := ps.GetPolicy(context.Background(), "t1", "a1"); !ok {
		t.Fatal("policy not stored under t1")
	}

	// Bad sample_rate (101) → 400.
	if w := doJSON(t, mux, "POST", "/admin/evals/policy", t1, map[string]any{
		"agent": "a2", "sample_rate": 101, "criteria": goodCriteria(),
	}); w.Code != http.StatusBadRequest {
		t.Fatalf("bad rate: want 400 got %d (%s)", w.Code, w.Body)
	}

	// exact scorer is not allowed online → 400.
	if w := doJSON(t, mux, "POST", "/admin/evals/policy", t1, map[string]any{
		"agent": "a3", "sample_rate": 10,
		"criteria": []map[string]any{{"name": "x", "scorer": "exact", "pattern": "hi"}},
	}); w.Code != http.StatusBadRequest {
		t.Fatalf("exact scorer: want 400 got %d (%s)", w.Code, w.Body)
	}

	// Non-superuser naming another tenant → 403.
	if w := doJSON(t, mux, "POST", "/admin/evals/policy", t1, map[string]any{
		"agent": "a1", "tenant": "t2", "sample_rate": 10, "criteria": goodCriteria(),
	}); w.Code != http.StatusForbidden {
		t.Fatalf("cross-tenant: want 403 got %d", w.Code)
	}
	// Non-superuser naming "*" → 403.
	if w := doJSON(t, mux, "POST", "/admin/evals/policy", t1, map[string]any{
		"agent": "a1", "tenant": "*", "sample_rate": 10, "criteria": goodCriteria(),
	}); w.Code != http.StatusForbidden {
		t.Fatalf("wildcard: want 403 got %d", w.Code)
	}

	// GET list is tenant-scoped.
	lw := doJSON(t, mux, "GET", "/admin/evals/policy", t1, nil)
	if lw.Code != http.StatusOK {
		t.Fatalf("list: %d", lw.Code)
	}
	var list []eval.Policy
	_ = json.Unmarshal(lw.Body.Bytes(), &list)
	if len(list) != 1 || list[0].AgentID != "a1" || list[0].Tenant != "t1" {
		t.Fatalf("list wrong: %+v", list)
	}

	// GET {agent} returns it.
	gw := doJSON(t, mux, "GET", "/admin/evals/policy/a1", t1, nil)
	if gw.Code != http.StatusOK {
		t.Fatalf("get: %d", gw.Code)
	}
	var got eval.Policy
	_ = json.Unmarshal(gw.Body.Bytes(), &got)
	if got.AgentID != "a1" || got.SampleRate != 25 {
		t.Fatalf("get wrong: %+v", got)
	}

	// GET a missing agent → 404.
	if w := doJSON(t, mux, "GET", "/admin/evals/policy/ghost", t1, nil); w.Code != http.StatusNotFound {
		t.Fatalf("missing get: want 404 got %d", w.Code)
	}

	// DELETE removes it → 204, then GET → 404.
	if w := doJSON(t, mux, "DELETE", "/admin/evals/policy/a1", t1, nil); w.Code != http.StatusNoContent {
		t.Fatalf("delete: want 204 got %d", w.Code)
	}
	if _, ok, _ := ps.GetPolicy(context.Background(), "t1", "a1"); ok {
		t.Fatal("policy still present after delete")
	}
}

func TestEvalPolicyAdminNotConfigured(t *testing.T) {
	// nil policy store ⇒ 503 on policy routes.
	mux := http.NewServeMux()
	reg := NewRegistry(&config.Config{}, "/bin/agentd", "dsn")
	RegisterEvalAdmin(context.Background(), mux, newFakeAdminStore(), eval.NewMemStore(),
		nil, store.NewMemStore(), fakeEvalInvoker{}, nil, reg, obs.NewControlMetrics())
	t1 := identity.Principal{Role: identity.RoleAdmin, TenantID: "t1"}
	if w := doJSON(t, mux, "GET", "/admin/evals/policy", t1, nil); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil policy store: want 503 got %d", w.Code)
	}
}

func TestEvalOnlineResultsTenantScoped(t *testing.T) {
	mux, _, ctl := evalPolicyMux(t)
	ctx := context.Background()
	// Seed results for two sessions across two tenants.
	if err := ctl.PutOnlineResult(ctx, "sess-t1", "polite", "t1", "alice", "judge", true, "ok"); err != nil {
		t.Fatalf("seed t1: %v", err)
	}
	if err := ctl.PutOnlineResult(ctx, "sess-t2", "polite", "t2", "bob", "judge", false, "no"); err != nil {
		t.Fatalf("seed t2: %v", err)
	}

	t1 := identity.Principal{Role: identity.RoleAdmin, TenantID: "t1"}

	// ?session= for the caller's own tenant returns the row.
	w := doJSON(t, mux, "GET", "/admin/evals/online-results?session=sess-t1", t1, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("session read: %d", w.Code)
	}
	var rows []store.OnlineResult
	_ = json.Unmarshal(w.Body.Bytes(), &rows)
	if len(rows) != 1 || rows[0].Tenant != "t1" {
		t.Fatalf("own-tenant session rows wrong: %+v", rows)
	}

	// A cross-tenant caller reading another tenant's session gets empty.
	cw := doJSON(t, mux, "GET", "/admin/evals/online-results?session=sess-t2", t1, nil)
	if cw.Code != http.StatusOK {
		t.Fatalf("cross-tenant session read: %d", cw.Code)
	}
	var crows []store.OnlineResult
	_ = json.Unmarshal(cw.Body.Bytes(), &crows)
	if len(crows) != 0 {
		t.Fatalf("cross-tenant caller should see empty, got %+v", crows)
	}

	// No ?session=: caller's whole tenant.
	tw := doJSON(t, mux, "GET", "/admin/evals/online-results", t1, nil)
	if tw.Code != http.StatusOK {
		t.Fatalf("tenant read: %d", tw.Code)
	}
	var trows []store.OnlineResult
	_ = json.Unmarshal(tw.Body.Bytes(), &trows)
	if len(trows) != 1 || trows[0].Tenant != "t1" {
		t.Fatalf("tenant rows wrong: %+v", trows)
	}
}

func TestPolicyResolverAdapter(t *testing.T) {
	ctx := context.Background()
	ps := eval.NewPolicyMemStore()
	if err := ps.PutPolicy(ctx, eval.Policy{
		Tenant: "t1", AgentID: "a1", SampleRate: 30,
		Criteria: []eval.Criterion{{Name: "polite", Scorer: eval.ScorerJudge, Rubric: "be polite"}},
	}); err != nil {
		t.Fatalf("seed policy: %v", err)
	}
	r := NewPolicyResolver(ps)

	// Stored policy ⇒ marshaled JSON that round-trips to the same policy.
	js, err := r.PolicyJSON(ctx, "t1", "a1")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if js == "" {
		t.Fatal("resolver returned empty for a stored policy")
	}
	var back eval.Policy
	if err := json.Unmarshal([]byte(js), &back); err != nil {
		t.Fatalf("unmarshal resolved policy: %v", err)
	}
	if back.AgentID != "a1" || back.SampleRate != 30 || len(back.Criteria) != 1 {
		t.Fatalf("resolved policy wrong: %+v", back)
	}

	// Absent policy ⇒ "".
	if js, err := r.PolicyJSON(ctx, "t1", "ghost"); err != nil || js != "" {
		t.Fatalf("absent policy: want (\"\",nil) got (%q,%v)", js, err)
	}
}
