package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sausheong/runtime/internal/config"
	"github.com/sausheong/runtime/internal/eval"
	"github.com/sausheong/runtime/internal/identity"
	"github.com/sausheong/runtime/internal/obs"
)

// fakeEvalInvoker returns a fixed output for every input, so runs complete
// deterministically without a real agent.
type fakeEvalInvoker struct{ out string }

func (f fakeEvalInvoker) Invoke(context.Context, string, string) (string, error) {
	return f.out, nil
}

// evalMux stands up /admin/evals/* over a MemStore and a Registry seeding one
// agent a1 under tenant t1. The fake invoker returns "hello world".
func evalMux(t *testing.T) (*http.ServeMux, *eval.MemStore) {
	t.Helper()
	mux := http.NewServeMux()
	es := eval.NewMemStore()
	reg := NewRegistry(&config.Config{Agents: []config.AgentConfig{
		{ID: "a1", Name: "A1", Model: "m", ListenAddr: "127.0.0.1:8401", Tenant: "t1"},
	}}, "/bin/agentd", "dsn")
	RegisterEvalAdmin(context.Background(), mux, newFakeAdminStore(), es,
		nil, nil, fakeEvalInvoker{out: "hello world"}, nil, reg, obs.NewControlMetrics())
	return mux, es
}

func doJSON(t *testing.T, h http.Handler, method, path string, p identity.Principal, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	r := withPrincipal(httptest.NewRequest(method, path, rdr), p)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestEvalSetsRBACAndValidation(t *testing.T) {
	mux, es := evalMux(t)
	t1 := identity.Principal{Role: identity.RoleAdmin, TenantID: "t1"}

	goodCases := []eval.Case{{Input: "2+2?", Scorer: eval.ScorerContains, Expected: "4"}}

	// Valid set → 201, stored under caller's tenant (empty tenant defaults).
	if w := doJSON(t, mux, "POST", "/admin/evals/sets", t1,
		map[string]any{"name": "math", "cases": goodCases}); w.Code != http.StatusCreated {
		t.Fatalf("valid create: want 201 got %d (%s)", w.Code, w.Body)
	}
	if _, ok, _ := es.GetSet(context.Background(), "t1", "math"); !ok {
		t.Fatal("set not stored under t1")
	}

	// Bad regex → 400.
	badCases := []eval.Case{{Input: "x", Scorer: eval.ScorerRegex, Expected: "("}}
	if w := doJSON(t, mux, "POST", "/admin/evals/sets", t1,
		map[string]any{"name": "bad", "cases": badCases}); w.Code != http.StatusBadRequest {
		t.Fatalf("bad regex: want 400 got %d", w.Code)
	}

	// Non-superuser naming another tenant → 403.
	if w := doJSON(t, mux, "POST", "/admin/evals/sets", t1,
		map[string]any{"name": "x", "tenant": "t2", "cases": goodCases}); w.Code != http.StatusForbidden {
		t.Fatalf("cross-tenant: want 403 got %d", w.Code)
	}
	// Non-superuser naming "*" → 403.
	if w := doJSON(t, mux, "POST", "/admin/evals/sets", t1,
		map[string]any{"name": "x", "tenant": "*", "cases": goodCases}); w.Code != http.StatusForbidden {
		t.Fatalf("wildcard tenant: want 403 got %d", w.Code)
	}
}

func TestEvalRunLifecycle(t *testing.T) {
	mux, _ := evalMux(t)
	t1 := identity.Principal{Role: identity.RoleAdmin, TenantID: "t1"}

	// Seed a set with a contains scorer the fake invoker's "hello world" passes.
	cases := []eval.Case{{Input: "hi", Scorer: eval.ScorerContains, Expected: "hello"}}
	if w := doJSON(t, mux, "POST", "/admin/evals/sets", t1,
		map[string]any{"name": "greet", "cases": cases}); w.Code != http.StatusCreated {
		t.Fatalf("seed set: %d (%s)", w.Code, w.Body)
	}

	// Run against the visible agent a1 → 202 with a run_id.
	w := doJSON(t, mux, "POST", "/admin/evals/runs", t1, map[string]any{"set": "greet", "agent": "a1"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("run create: want 202 got %d (%s)", w.Code, w.Body)
	}
	var rr struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rr); err != nil || rr.RunID == "" {
		t.Fatalf("run_id missing: %s (err %v)", w.Body, err)
	}

	// Poll the run to completion (the goroutine uses the fake invoker).
	var run eval.Run
	deadline := time.Now().Add(2 * time.Second)
	for {
		gw := doJSON(t, mux, "GET", "/admin/evals/runs/"+rr.RunID, t1, nil)
		if gw.Code != http.StatusOK {
			t.Fatalf("get run: %d", gw.Code)
		}
		_ = json.Unmarshal(gw.Body.Bytes(), &run)
		if run.Status == eval.StatusCompleted || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if run.Status != eval.StatusCompleted {
		t.Fatalf("run did not complete: status=%q", run.Status)
	}
	if run.Total != 1 || run.Passed != 1 {
		t.Fatalf("run tally: total=%d passed=%d, want 1/1", run.Total, run.Passed)
	}

	// Results present.
	resW := doJSON(t, mux, "GET", "/admin/evals/runs/"+rr.RunID+"/results", t1, nil)
	if resW.Code != http.StatusOK {
		t.Fatalf("results: %d", resW.Code)
	}
	var results []eval.Result
	_ = json.Unmarshal(resW.Body.Bytes(), &results)
	if len(results) != 1 || !results[0].Passed {
		t.Fatalf("results wrong: %+v", results)
	}
}

func TestEvalRunUnknownAgent(t *testing.T) {
	mux, es := evalMux(t)
	t1 := identity.Principal{Role: identity.RoleAdmin, TenantID: "t1"}
	cases := []eval.Case{{Input: "hi", Scorer: eval.ScorerContains, Expected: "hello"}}
	_ = es.PutSet(context.Background(), eval.Set{Tenant: "t1", Name: "greet", Cases: cases})

	// Unknown agent → 400, no run created.
	if w := doJSON(t, mux, "POST", "/admin/evals/runs", t1,
		map[string]any{"set": "greet", "agent": "ghost"}); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown agent: want 400 got %d", w.Code)
	}
	if runs, _ := es.ListRuns(context.Background(), "t1"); len(runs) != 0 {
		t.Fatalf("no run should be created for unknown agent, got %d", len(runs))
	}
}

func TestEvalRunTenantIsolation(t *testing.T) {
	mux, es := evalMux(t)
	// A t1 run exists.
	_ = es.CreateRun(context.Background(), eval.Run{
		RunID: "r-t1", Tenant: "t1", SetName: "greet", AgentID: "a1", Status: eval.StatusCompleted,
	})
	t2 := identity.Principal{Role: identity.RoleAdmin, TenantID: "t2"}

	// t2 admin GETting t1's run → 404 (no cross-tenant oracle).
	if w := doJSON(t, mux, "GET", "/admin/evals/runs/r-t1", t2, nil); w.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant run get: want 404 got %d", w.Code)
	}
	// And its results → 404 too.
	if w := doJSON(t, mux, "GET", "/admin/evals/runs/r-t1/results", t2, nil); w.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant results get: want 404 got %d", w.Code)
	}
}
