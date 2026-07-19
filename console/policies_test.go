package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sausheong/runtime/internal/policy"
)

const consoleValidPolicy = `forbid (principal, action, resource) when { resource.server == "sandbox" };`

// consoleWithPolicies wires a console whose onboarding deps include a real
// policy MemStore, so the Policies UI is active.
func consoleWithPolicies(t *testing.T) (http.Handler, *policy.MemStore) {
	t.Helper()
	ps := policy.NewMemStore()
	deps := &Onboarding{
		Upstreams: &fakeUpstreamStore2{},
		Mutator:   &fakeMut2{},
		Admin:     &fakeAdmin2{},
		Secrets:   &fakeSec2{},
		Policies:  ps,
	}
	return Handler(nil, nil, OIDCConfig{}, deps), ps
}

func TestPolicies_SectionRenders(t *testing.T) {
	h, ps := consoleWithPolicies(t)
	_ = ps.Insert(context.Background(), policy.Row{Tenant: "t1", Name: "no-sbx", CedarText: consoleValidPolicy})
	r := adminReq("GET", "/ui/onboarding", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	body := w.Body.String()
	if !strings.Contains(body, "Gateway policies") {
		t.Fatal("policies section missing")
	}
	if !strings.Contains(body, "/ui/onboarding/policies/no-sbx/delete") {
		t.Fatal("expected a delete action for the policy")
	}
}

func TestPolicies_AddRequiresCSRF(t *testing.T) {
	h, _ := consoleWithPolicies(t)
	form := url.Values{"name": {"p"}, "cedar_text": {consoleValidPolicy}} // no csrf
	r := adminReq("POST", "/ui/onboarding/policies", form)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("add without csrf: want 403 got %d", w.Code)
	}
}

func TestPolicies_AddValid(t *testing.T) {
	h, ps := consoleWithPolicies(t)
	token := issuedCSRF(t, h)
	form := url.Values{"csrf_token": {token}, "name": {"no-sbx"}, "cedar_text": {consoleValidPolicy}}
	r := adminReq("POST", "/ui/onboarding/policies", form)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("add: want 303 got %d (%s)", w.Code, w.Body.String())
	}
	if rows, _ := ps.List(context.Background(), "t1"); len(rows) != 1 {
		t.Fatalf("policy not stored: %+v", rows)
	}
}

func TestPolicies_AddInvalidCedarShowsError(t *testing.T) {
	h, ps := consoleWithPolicies(t)
	token := issuedCSRF(t, h)
	form := url.Values{"csrf_token": {token}, "name": {"bad"}, "cedar_text": {"not cedar"}}
	r := adminReq("POST", "/ui/onboarding/policies", form)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	// Invalid Cedar is redirected with a flash rather than persisted.
	if w.Code != http.StatusSeeOther {
		t.Fatalf("invalid cedar: want 303 redirect got %d", w.Code)
	}
	if rows, _ := ps.List(context.Background(), "t1"); len(rows) != 0 {
		t.Fatalf("invalid policy must not persist: %+v", rows)
	}
}

func TestPolicies_AddRequiresAdmin(t *testing.T) {
	h, _ := consoleWithPolicies(t)
	token := issuedCSRF(t, h)
	form := url.Values{"csrf_token": {token}, "name": {"p"}, "cedar_text": {consoleValidPolicy}}
	r := nonAdminReq("POST", "/ui/onboarding/policies", form)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-admin add: want 403 got %d", w.Code)
	}
}
