package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sausheong/runtime/internal/eval"
)

// consoleWithEval wires a console whose onboarding deps include a real in-memory
// eval store (MemStore implements eval.EvalStore), so the Eval sets UI is active.
func consoleWithEval(t *testing.T) (http.Handler, *eval.MemStore) {
	t.Helper()
	es := eval.NewMemStore()
	deps := &Onboarding{
		Upstreams: &fakeUpstreamStore2{},
		Mutator:   &fakeMut2{},
		Admin:     &fakeAdmin2{},
		Secrets:   &fakeSec2{},
		EvalStore: es,
	}
	return Handler(nil, nil, OIDCConfig{}, deps), es
}

func TestEvalSets_SectionRenders(t *testing.T) {
	h, es := consoleWithEval(t)
	_ = es.PutSet(context.Background(), eval.Set{
		Tenant: "t1", Name: "greetings",
		Cases: []eval.Case{{Input: "hi", Scorer: eval.ScorerContains, Expected: "hello"}},
	})
	r := adminReq("GET", "/ui/onboarding", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	body := w.Body.String()
	if !strings.Contains(body, "Eval sets") {
		t.Fatal("eval sets section missing")
	}
	if !strings.Contains(body, "greetings") {
		t.Fatal("expected seeded set name in the eval sets table")
	}
}

func TestEvalSets_HiddenWhenNil(t *testing.T) {
	h, _ := newTestConsole() // no EvalStore dep
	r := adminReq("GET", "/ui/onboarding", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if strings.Contains(w.Body.String(), "Eval sets") {
		t.Fatal("eval sets section must be hidden when the dep is nil")
	}
}

func TestEvalSets_AddRequiresCSRF(t *testing.T) {
	h, _ := consoleWithEval(t)
	form := url.Values{"name": {"greetings"}, "cases": {`[{"input":"hi","scorer":"contains","expected":"hello"}]`}} // no csrf
	r := adminReq("POST", "/ui/onboarding/eval-sets", form)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("add without csrf: want 403 got %d", w.Code)
	}
}

func TestEvalSets_AddValid(t *testing.T) {
	h, es := consoleWithEval(t)
	token := issuedCSRF(t, h)
	form := url.Values{
		"csrf_token": {token}, "name": {"greetings"},
		"cases": {`[{"input":"hi","scorer":"contains","expected":"hello"}]`},
	}
	r := adminReq("POST", "/ui/onboarding/eval-sets", form)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("add: want 303 got %d (%s)", w.Code, w.Body.String())
	}
	sets, _ := es.ListSets(context.Background(), "t1")
	if len(sets) != 1 || sets[0].Name != "greetings" || sets[0].Tenant != "t1" || len(sets[0].Cases) != 1 {
		t.Fatalf("set not stored as (t1, greetings, 1 case): %+v", sets)
	}
	if sets[0].Cases[0].Scorer != eval.ScorerContains || sets[0].Cases[0].Expected != "hello" {
		t.Fatalf("case not parsed from JSON textarea: %+v", sets[0].Cases)
	}
}

func TestEvalSets_AddRejectsMalformedJSON(t *testing.T) {
	h, es := consoleWithEval(t)
	token := issuedCSRF(t, h)
	form := url.Values{"csrf_token": {token}, "name": {"greetings"}, "cases": {"not json"}}
	r := adminReq("POST", "/ui/onboarding/eval-sets", form)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("malformed cases: want 400 got %d", w.Code)
	}
	if sets, _ := es.ListSets(context.Background(), "t1"); len(sets) != 0 {
		t.Fatalf("malformed-cases set must not persist: %+v", sets)
	}
}

func TestEvalSets_AddRejectsInvalidSet(t *testing.T) {
	h, es := consoleWithEval(t)
	token := issuedCSRF(t, h)
	// Valid JSON but empty case list ⇒ ValidateSet fails ⇒ 400.
	form := url.Values{"csrf_token": {token}, "name": {"greetings"}, "cases": {"[]"}}
	r := adminReq("POST", "/ui/onboarding/eval-sets", form)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid set: want 400 got %d", w.Code)
	}
	if sets, _ := es.ListSets(context.Background(), "t1"); len(sets) != 0 {
		t.Fatalf("invalid set must not persist: %+v", sets)
	}
}

func TestEvalSets_Delete(t *testing.T) {
	h, es := consoleWithEval(t)
	_ = es.PutSet(context.Background(), eval.Set{
		Tenant: "t1", Name: "greetings",
		Cases: []eval.Case{{Input: "hi", Scorer: eval.ScorerContains, Expected: "hello"}},
	})
	token := issuedCSRF(t, h)
	form := url.Values{"csrf_token": {token}}
	r := adminReq("POST", "/ui/onboarding/eval-sets/greetings/delete", form)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("delete: want 303 got %d (%s)", w.Code, w.Body.String())
	}
	if sets, _ := es.ListSets(context.Background(), "t1"); len(sets) != 0 {
		t.Fatalf("set not deleted: %+v", sets)
	}
}

func TestEvalSets_DeleteRequiresCSRF(t *testing.T) {
	h, _ := consoleWithEval(t)
	r := adminReq("POST", "/ui/onboarding/eval-sets/greetings/delete", url.Values{}) // no csrf
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("delete without csrf: want 403 got %d", w.Code)
	}
}

func TestEvalSets_AddRequiresAdmin(t *testing.T) {
	h, _ := consoleWithEval(t)
	token := issuedCSRF(t, h)
	form := url.Values{
		"csrf_token": {token}, "name": {"greetings"},
		"cases": {`[{"input":"hi","scorer":"contains","expected":"hello"}]`},
	}
	r := nonAdminReq("POST", "/ui/onboarding/eval-sets", form)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-admin add: want 403 got %d", w.Code)
	}
}

// consoleWithEvalPolicies wires a console whose onboarding deps include a real
// in-memory policy store (PolicyMemStore implements eval.PolicyStoreAPI), so the
// online eval policies UI is active.
func consoleWithEvalPolicies(t *testing.T) (http.Handler, *eval.PolicyMemStore) {
	t.Helper()
	ps := eval.NewPolicyMemStore()
	deps := &Onboarding{
		Upstreams:    &fakeUpstreamStore2{},
		Mutator:      &fakeMut2{},
		Admin:        &fakeAdmin2{},
		Secrets:      &fakeSec2{},
		EvalPolicies: ps,
	}
	return Handler(nil, nil, OIDCConfig{}, deps), ps
}

func TestEvalPolicies_SectionRenders(t *testing.T) {
	h, ps := consoleWithEvalPolicies(t)
	_ = ps.PutPolicy(context.Background(), eval.Policy{
		Tenant: "t1", AgentID: "support", SampleRate: 10,
		Criteria: []eval.Criterion{{Name: "polite", Scorer: eval.ScorerJudge, Rubric: "Is the reply polite?"}},
	})
	r := adminReq("GET", "/ui/onboarding", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	body := w.Body.String()
	if !strings.Contains(body, "Online eval policies") {
		t.Fatal("online eval policies section missing")
	}
	if !strings.Contains(body, "support") {
		t.Fatal("expected seeded agent id in the policies table")
	}
}

func TestEvalPolicies_HiddenWhenNil(t *testing.T) {
	h, _ := newTestConsole() // no EvalPolicies dep
	r := adminReq("GET", "/ui/onboarding", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if strings.Contains(w.Body.String(), "Online eval policies") {
		t.Fatal("online eval policies section must be hidden when the dep is nil")
	}
}

func TestEvalPolicies_AddRequiresCSRF(t *testing.T) {
	h, _ := consoleWithEvalPolicies(t)
	form := url.Values{"agent": {"support"}, "rate": {"10"}, "criteria": {`[{"name":"polite","scorer":"judge","rubric":"Is the reply polite?"}]`}} // no csrf
	r := adminReq("POST", "/ui/onboarding/eval-policies", form)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("add without csrf: want 403 got %d", w.Code)
	}
}

func TestEvalPolicies_AddValid(t *testing.T) {
	h, ps := consoleWithEvalPolicies(t)
	token := issuedCSRF(t, h)
	form := url.Values{
		"csrf_token": {token}, "agent": {"support"}, "rate": {"10"},
		"criteria": {`[{"name":"polite","scorer":"judge","rubric":"Is the reply polite?"}]`},
	}
	r := adminReq("POST", "/ui/onboarding/eval-policies", form)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("add: want 303 got %d (%s)", w.Code, w.Body.String())
	}
	pols, _ := ps.ListPolicies(context.Background(), "t1")
	if len(pols) != 1 || pols[0].AgentID != "support" || pols[0].Tenant != "t1" || pols[0].SampleRate != 10 || len(pols[0].Criteria) != 1 {
		t.Fatalf("policy not stored as (t1, support, rate 10, 1 criterion): %+v", pols)
	}
	if pols[0].Criteria[0].Scorer != eval.ScorerJudge || pols[0].Criteria[0].Rubric != "Is the reply polite?" {
		t.Fatalf("criterion not parsed from JSON textarea: %+v", pols[0].Criteria)
	}
}

func TestEvalPolicies_AddRejectsMalformedJSON(t *testing.T) {
	h, ps := consoleWithEvalPolicies(t)
	token := issuedCSRF(t, h)
	form := url.Values{"csrf_token": {token}, "agent": {"support"}, "rate": {"10"}, "criteria": {"not json"}}
	r := adminReq("POST", "/ui/onboarding/eval-policies", form)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("malformed criteria: want 400 got %d", w.Code)
	}
	if pols, _ := ps.ListPolicies(context.Background(), "t1"); len(pols) != 0 {
		t.Fatalf("malformed-criteria policy must not persist: %+v", pols)
	}
}

func TestEvalPolicies_AddRejectsBadRate(t *testing.T) {
	h, ps := consoleWithEvalPolicies(t)
	token := issuedCSRF(t, h)
	// Non-numeric rate ⇒ strconv.Atoi fails ⇒ 400.
	form := url.Values{"csrf_token": {token}, "agent": {"support"}, "rate": {"abc"},
		"criteria": {`[{"name":"polite","scorer":"judge","rubric":"Is the reply polite?"}]`}}
	r := adminReq("POST", "/ui/onboarding/eval-policies", form)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad rate: want 400 got %d", w.Code)
	}
	if pols, _ := ps.ListPolicies(context.Background(), "t1"); len(pols) != 0 {
		t.Fatalf("bad-rate policy must not persist: %+v", pols)
	}
}

func TestEvalPolicies_AddRejectsInvalidPolicy(t *testing.T) {
	h, ps := consoleWithEvalPolicies(t)
	token := issuedCSRF(t, h)
	// Valid JSON, numeric rate, but rate 101 ⇒ ValidatePolicy fails ⇒ 400.
	form := url.Values{"csrf_token": {token}, "agent": {"support"}, "rate": {"101"},
		"criteria": {`[{"name":"polite","scorer":"judge","rubric":"Is the reply polite?"}]`}}
	r := adminReq("POST", "/ui/onboarding/eval-policies", form)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid policy (rate 101): want 400 got %d", w.Code)
	}
	if pols, _ := ps.ListPolicies(context.Background(), "t1"); len(pols) != 0 {
		t.Fatalf("invalid policy must not persist: %+v", pols)
	}
}

func TestEvalPolicies_Delete(t *testing.T) {
	h, ps := consoleWithEvalPolicies(t)
	_ = ps.PutPolicy(context.Background(), eval.Policy{
		Tenant: "t1", AgentID: "support", SampleRate: 10,
		Criteria: []eval.Criterion{{Name: "polite", Scorer: eval.ScorerJudge, Rubric: "Is the reply polite?"}},
	})
	token := issuedCSRF(t, h)
	form := url.Values{"csrf_token": {token}}
	r := adminReq("POST", "/ui/onboarding/eval-policies/support/delete", form)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("delete: want 303 got %d (%s)", w.Code, w.Body.String())
	}
	if pols, _ := ps.ListPolicies(context.Background(), "t1"); len(pols) != 0 {
		t.Fatalf("policy not deleted: %+v", pols)
	}
}

func TestEvalPolicies_DeleteRequiresCSRF(t *testing.T) {
	h, _ := consoleWithEvalPolicies(t)
	r := adminReq("POST", "/ui/onboarding/eval-policies/support/delete", url.Values{}) // no csrf
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("delete without csrf: want 403 got %d", w.Code)
	}
}

func TestEvalPolicies_AddRequiresAdmin(t *testing.T) {
	h, _ := consoleWithEvalPolicies(t)
	token := issuedCSRF(t, h)
	form := url.Values{
		"csrf_token": {token}, "agent": {"support"}, "rate": {"10"},
		"criteria": {`[{"name":"polite","scorer":"judge","rubric":"Is the reply polite?"}]`},
	}
	r := nonAdminReq("POST", "/ui/onboarding/eval-policies", form)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-admin add: want 403 got %d", w.Code)
	}
}
