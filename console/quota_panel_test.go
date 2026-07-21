package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sausheong/runtime/internal/quota"
)

// fakeQuotaStore is an in-memory controlplane.QuotaStore for the console tests.
type fakeQuotaStore struct {
	rules []quota.Rule
}

func (f *fakeQuotaStore) Insert(_ context.Context, r quota.Rule) error {
	for i := range f.rules {
		if f.rules[i].Tenant == r.Tenant && f.rules[i].Upstream == r.Upstream {
			f.rules[i] = r
			return nil
		}
	}
	f.rules = append(f.rules, r)
	return nil
}

func (f *fakeQuotaStore) List(_ context.Context, tenant string) ([]quota.Rule, error) {
	var out []quota.Rule
	for _, r := range f.rules {
		if r.Tenant == tenant {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeQuotaStore) Delete(_ context.Context, tenant, upstream string) (bool, error) {
	for i := range f.rules {
		if f.rules[i].Tenant == tenant && f.rules[i].Upstream == upstream {
			f.rules = append(f.rules[:i], f.rules[i+1:]...)
			return true, nil
		}
	}
	return false, nil
}

// consoleWithQuotas wires a console whose onboarding deps include a fake quota
// store, so the Quotas UI is active.
func consoleWithQuotas(t *testing.T) (http.Handler, *fakeQuotaStore) {
	t.Helper()
	qs := &fakeQuotaStore{}
	deps := &Onboarding{
		Upstreams: &fakeUpstreamStore2{},
		Mutator:   &fakeMut2{},
		Admin:     &fakeAdmin2{},
		Secrets:   &fakeSec2{},
		Quotas:    qs,
	}
	return Handler(nil, nil, OIDCConfig{}, deps), qs
}

func TestQuotas_SectionRenders(t *testing.T) {
	h, qs := consoleWithQuotas(t)
	_ = qs.Insert(context.Background(), quota.Rule{Tenant: "t1", Upstream: "srv1", RatePerMin: 100})
	r := adminReq("GET", "/ui/onboarding", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	body := w.Body.String()
	if !strings.Contains(body, "Gateway quotas") {
		t.Fatal("quotas section missing")
	}
	if !strings.Contains(body, "srv1") {
		t.Fatal("expected seeded upstream in the quotas table")
	}
}

func TestQuotas_HiddenWhenNil(t *testing.T) {
	h, _ := newTestConsole() // no Quotas dep
	r := adminReq("GET", "/ui/onboarding", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if strings.Contains(w.Body.String(), "Gateway quotas") {
		t.Fatal("quotas section must be hidden when the dep is nil")
	}
}

func TestQuotas_AddRequiresCSRF(t *testing.T) {
	h, _ := consoleWithQuotas(t)
	form := url.Values{"upstream": {"srv1"}, "rate": {"100"}} // no csrf
	r := adminReq("POST", "/ui/onboarding/quotas", form)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("add without csrf: want 403 got %d", w.Code)
	}
}

func TestQuotas_AddValid(t *testing.T) {
	h, qs := consoleWithQuotas(t)
	token := issuedCSRF(t, h)
	form := url.Values{"csrf_token": {token}, "upstream": {"srv1"}, "rate": {"100"}}
	r := adminReq("POST", "/ui/onboarding/quotas", form)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("add: want 303 got %d (%s)", w.Code, w.Body.String())
	}
	rows, _ := qs.List(context.Background(), "t1")
	if len(rows) != 1 || rows[0].Upstream != "srv1" || rows[0].RatePerMin != 100 || rows[0].Tenant != "t1" {
		t.Fatalf("quota not stored as (t1, srv1, 100): %+v", rows)
	}
}

func TestQuotas_AddRejectsBadRate(t *testing.T) {
	h, qs := consoleWithQuotas(t)
	token := issuedCSRF(t, h)
	form := url.Values{"csrf_token": {token}, "upstream": {"srv1"}, "rate": {"nope"}}
	r := adminReq("POST", "/ui/onboarding/quotas", form)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad rate: want 400 got %d", w.Code)
	}
	if rows, _ := qs.List(context.Background(), "t1"); len(rows) != 0 {
		t.Fatalf("bad-rate quota must not persist: %+v", rows)
	}
}

func TestQuotas_Delete(t *testing.T) {
	h, qs := consoleWithQuotas(t)
	_ = qs.Insert(context.Background(), quota.Rule{Tenant: "t1", Upstream: "srv1", RatePerMin: 100})
	token := issuedCSRF(t, h)
	form := url.Values{"csrf_token": {token}, "upstream": {"srv1"}}
	r := adminReq("POST", "/ui/onboarding/quotas/delete", form)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("delete: want 303 got %d (%s)", w.Code, w.Body.String())
	}
	if rows, _ := qs.List(context.Background(), "t1"); len(rows) != 0 {
		t.Fatalf("quota not deleted: %+v", rows)
	}
}

func TestQuotas_DeleteRequiresCSRF(t *testing.T) {
	h, _ := consoleWithQuotas(t)
	form := url.Values{"upstream": {"srv1"}} // no csrf
	r := adminReq("POST", "/ui/onboarding/quotas/delete", form)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("delete without csrf: want 403 got %d", w.Code)
	}
}

func TestQuotas_AddRequiresAdmin(t *testing.T) {
	h, _ := consoleWithQuotas(t)
	token := issuedCSRF(t, h)
	form := url.Values{"csrf_token": {token}, "upstream": {"srv1"}, "rate": {"100"}}
	r := nonAdminReq("POST", "/ui/onboarding/quotas", form)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-admin add: want 403 got %d", w.Code)
	}
}
