package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sausheong/runtime/controlplane"
	"github.com/sausheong/runtime/internal/config"
	"github.com/sausheong/runtime/internal/eval"
)

// fakeInvoker is a hermetic eval.Invoker returning a fixed output and counting
// calls, so a launch test can prove the async runner reached the agent.
type fakeInvoker struct {
	output string
	mu     sync.Mutex
	calls  int
}

func (f *fakeInvoker) Invoke(_ context.Context, _ string, _ string) (string, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return f.output, nil
}

func (f *fakeInvoker) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// evalRunsReg builds a registry with agent "support" in the caller's tenant
// (t1, matching adminReq) and "t2agent" in a foreign tenant (t2) to exercise
// the cross-tenant invisibility guard.
func evalRunsReg(t *testing.T) *controlplane.Registry {
	t.Helper()
	cfg := &config.Config{Agents: []config.AgentConfig{
		{ID: "support", Name: "Support", Model: "m", ListenAddr: "127.0.0.1:9101", Tenant: "t1"},
		{ID: "t2agent", Name: "Other", Model: "m", ListenAddr: "127.0.0.1:9102", Tenant: "t2"},
	}}
	return controlplane.NewRegistry(cfg, "./agentd", "dsn")
}

// consoleWithEvalRuns wires a console whose onboarding deps include a real
// in-memory eval store, a fake invoker, a nil judge (contains scorer needs
// none), and the signal ctx — so the observability Eval-runs UI is active.
func consoleWithEvalRuns(t *testing.T) (http.Handler, *eval.MemStore, *fakeInvoker) {
	t.Helper()
	es := eval.NewMemStore()
	inv := &fakeInvoker{output: "hello there"}
	deps := &Onboarding{
		Upstreams:     &fakeUpstreamStore2{},
		Mutator:       &fakeMut2{},
		Admin:         &fakeAdmin2{},
		Secrets:       &fakeSec2{},
		EvalStore:     es,
		EvalInvoker:   inv,
		EvalJudge:     nil,
		EvalSignalCtx: context.Background(),
	}
	return Handler(evalRunsReg(t), nil, OIDCConfig{}, deps), es, inv
}

// seedSet stores a one-case "contains" set the fake invoker's output satisfies.
func seedSet(t *testing.T, es *eval.MemStore, tenant, name string) {
	t.Helper()
	if err := es.PutSet(context.Background(), eval.Set{
		Tenant: tenant, Name: name,
		Cases: []eval.Case{{Input: "hi", Scorer: eval.ScorerContains, Expected: "hello"}},
	}); err != nil {
		t.Fatalf("seed set: %v", err)
	}
}

func TestEvalRuns_SectionRenders(t *testing.T) {
	h, es, _ := consoleWithEvalRuns(t)
	seedSet(t, es, "t1", "greetings")
	_ = es.CreateRun(context.Background(), eval.Run{
		RunID: "run-abc", Tenant: "t1", SetName: "greetings", AgentID: "support", Status: eval.StatusCompleted,
	})
	r := adminReq("GET", "/ui/observability", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("GET observability: want 200 got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Eval runs") {
		t.Fatal("eval runs section missing")
	}
	if !strings.Contains(body, `action="/ui/observability/eval-runs"`) {
		t.Fatal("launch form missing")
	}
	if !strings.Contains(body, "greetings") {
		t.Fatal("launch form missing the seeded set in its dropdown")
	}
	if !strings.Contains(body, "support") {
		t.Fatal("launch form missing the visible agent in its dropdown")
	}
	if !strings.Contains(body, "run-abc") {
		t.Fatal("runs table missing the seeded run")
	}
}

func TestEvalRuns_HiddenWhenNil(t *testing.T) {
	// EvalStore nil ⇒ no Eval-runs section, even with identity on.
	deps := &Onboarding{Upstreams: &fakeUpstreamStore2{}, Mutator: &fakeMut2{}, Admin: &fakeAdmin2{}}
	h := Handler(evalRunsReg(t), nil, OIDCConfig{}, deps)
	r := adminReq("GET", "/ui/observability", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if strings.Contains(w.Body.String(), "Eval runs") {
		t.Fatal("eval runs section must be hidden when the store is nil")
	}
}

func TestEvalRuns_LaunchValid(t *testing.T) {
	h, es, inv := consoleWithEvalRuns(t)
	seedSet(t, es, "t1", "greetings")
	token := issuedObsCSRF(t, h)
	form := url.Values{"csrf_token": {token}, "set": {"greetings"}, "agent": {"support"}}
	r := adminReq("POST", "/ui/observability/eval-runs", form)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("launch: want 303 got %d (%s)", w.Code, w.Body.String())
	}
	// A run must have been created under the caller's tenant.
	runs, _ := es.ListRuns(context.Background(), "t1")
	if len(runs) != 1 || runs[0].SetName != "greetings" || runs[0].AgentID != "support" {
		t.Fatalf("run not created as (t1, greetings, support): %+v", runs)
	}
	// The async runner (on the signal ctx) reaches the agent and completes.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, _, _ := es.GetRun(context.Background(), runs[0].RunID)
		if got.Status == eval.StatusCompleted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	got, _, _ := es.GetRun(context.Background(), runs[0].RunID)
	if got.Status != eval.StatusCompleted {
		t.Fatalf("run did not complete: status=%q", got.Status)
	}
	if inv.callCount() == 0 {
		t.Fatal("invoker was never called by the async runner")
	}
	if got.Passed != 1 {
		t.Fatalf("expected 1 passed case, got %+v", got)
	}
}

func TestEvalRuns_LaunchRequiresCSRF(t *testing.T) {
	h, es, _ := consoleWithEvalRuns(t)
	seedSet(t, es, "t1", "greetings")
	form := url.Values{"set": {"greetings"}, "agent": {"support"}} // no csrf
	r := adminReq("POST", "/ui/observability/eval-runs", form)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("launch without csrf: want 403 got %d", w.Code)
	}
	if runs, _ := es.ListRuns(context.Background(), "t1"); len(runs) != 0 {
		t.Fatalf("no run must be created without csrf: %+v", runs)
	}
}

func TestEvalRuns_LaunchRequiresAdmin(t *testing.T) {
	h, es, _ := consoleWithEvalRuns(t)
	seedSet(t, es, "t1", "greetings")
	token := issuedObsCSRF(t, h)
	form := url.Values{"csrf_token": {token}, "set": {"greetings"}, "agent": {"support"}}
	r := nonAdminReq("POST", "/ui/observability/eval-runs", form)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-admin launch: want 403 got %d", w.Code)
	}
}

func TestEvalRuns_LaunchUnknownSet(t *testing.T) {
	h, _, _ := consoleWithEvalRuns(t)
	token := issuedObsCSRF(t, h)
	form := url.Values{"csrf_token": {token}, "set": {"nope"}, "agent": {"support"}}
	r := adminReq("POST", "/ui/observability/eval-runs", form)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown set: want 400 got %d", w.Code)
	}
}

func TestEvalRuns_LaunchMissingFields(t *testing.T) {
	h, es, _ := consoleWithEvalRuns(t)
	seedSet(t, es, "t1", "greetings")
	token := issuedObsCSRF(t, h)
	form := url.Values{"csrf_token": {token}, "set": {"greetings"}} // no agent
	r := adminReq("POST", "/ui/observability/eval-runs", form)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing agent: want 400 got %d", w.Code)
	}
}

func TestEvalRuns_LaunchInvisibleAgent(t *testing.T) {
	h, es, _ := consoleWithEvalRuns(t)
	seedSet(t, es, "t1", "greetings")
	token := issuedObsCSRF(t, h)
	// t2agent exists but belongs to tenant t2 ⇒ invisible to the t1 caller.
	form := url.Values{"csrf_token": {token}, "set": {"greetings"}, "agent": {"t2agent"}}
	r := adminReq("POST", "/ui/observability/eval-runs", form)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invisible agent: want 400 got %d", w.Code)
	}
	if runs, _ := es.ListRuns(context.Background(), "t1"); len(runs) != 0 {
		t.Fatalf("no run must be created for an invisible agent: %+v", runs)
	}
}

func TestEvalRuns_ResultsView(t *testing.T) {
	h, es, _ := consoleWithEvalRuns(t)
	_ = es.CreateRun(context.Background(), eval.Run{
		RunID: "run-res", Tenant: "t1", SetName: "greetings", AgentID: "support", Status: eval.StatusCompleted,
	})
	_ = es.PutResult(context.Background(), "run-res", eval.Result{
		CaseIndex: 0, Input: "hi", Output: "hello there", Scorer: "contains", Passed: true, Detail: "",
	})
	r := adminReq("GET", "/ui/observability/eval-runs/run-res", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("results view: want 200 got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "run-res") {
		t.Fatal("results view missing run id")
	}
	if !strings.Contains(body, "hello there") {
		t.Fatal("results view missing per-case output")
	}
}

func TestEvalRuns_ResultsCrossTenant404(t *testing.T) {
	h, es, _ := consoleWithEvalRuns(t)
	// Run owned by t2 ⇒ a t1 caller must get a 404 (no oracle).
	_ = es.CreateRun(context.Background(), eval.Run{
		RunID: "run-foreign", Tenant: "t2", SetName: "greetings", AgentID: "t2agent", Status: eval.StatusCompleted,
	})
	r := adminReq("GET", "/ui/observability/eval-runs/run-foreign", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant results: want 404 got %d", w.Code)
	}
}

// issuedObsCSRF fetches the observability page as the test admin and extracts the
// CSRF token the launch form carries (HMAC of the "sess-1" session in adminReq).
func issuedObsCSRF(t *testing.T, h http.Handler) string {
	t.Helper()
	r := adminReq("GET", "/ui/observability", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	body := w.Body.String()
	const marker = `name="csrf_token" value="`
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatal("no csrf_token field found on the observability page")
	}
	rest := body[i+len(marker):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		t.Fatal("malformed csrf_token field")
	}
	return rest[:j]
}
