package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sausheong/runtime/internal/identity"
	"github.com/sausheong/runtime/internal/policy"
)

const validPolicy = `forbid (principal, action, resource) when { resource.server == "sandbox" };`

func postPolicy(t *testing.T, h http.Handler, p identity.Principal, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	r := withPrincipal(httptest.NewRequest("POST", "/admin/policies", bytes.NewReader(b)), p)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func policyMux(t *testing.T) (*http.ServeMux, *policy.MemStore) {
	t.Helper()
	mux := http.NewServeMux()
	store := newFakeAdminStore()
	store.CreateTenant(context.Background(), "t1", "t1")
	store.CreateTenant(context.Background(), "t2", "t2")
	ps := policy.NewMemStore()
	RegisterPolicyAdmin(mux, store, ps)
	return mux, ps
}

func TestPolicyAPIRBACAndValidation(t *testing.T) {
	mux, ps := policyMux(t)
	admin := identity.Principal{Role: identity.RoleAdmin, TenantID: "t1"}
	viewer := identity.Principal{Role: identity.RoleViewer, TenantID: "t1"}
	operator := identity.Principal{Role: identity.RoleOperator, TenantID: "t1"}

	if w := postPolicy(t, mux, viewer, map[string]any{"name": "p", "cedar_text": validPolicy}); w.Code != http.StatusForbidden {
		t.Fatalf("viewer: want 403 got %d", w.Code)
	}
	if w := postPolicy(t, mux, operator, map[string]any{"name": "p", "cedar_text": validPolicy}); w.Code != http.StatusForbidden {
		t.Fatalf("operator: want 403 got %d", w.Code)
	}
	if w := postPolicy(t, mux, admin, map[string]any{"name": "", "cedar_text": validPolicy}); w.Code != http.StatusBadRequest {
		t.Fatalf("empty name: want 400 got %d", w.Code)
	}
	// Invalid Cedar ⇒ 400 with the parser message in the body.
	w := postPolicy(t, mux, admin, map[string]any{"name": "bad", "cedar_text": "not cedar"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad cedar: want 400 got %d", w.Code)
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("Cedar")) && !bytes.Contains(w.Body.Bytes(), []byte("invalid")) {
		t.Fatalf("bad-cedar body should carry parser error, got %q", w.Body.String())
	}
	// Valid ⇒ 201 and stored under t1.
	if w := postPolicy(t, mux, admin, map[string]any{"name": "no-sbx", "cedar_text": validPolicy}); w.Code != http.StatusCreated {
		t.Fatalf("create: want 201 got %d (%s)", w.Code, w.Body)
	}
	rows, _ := ps.List(context.Background(), "t1")
	if len(rows) != 1 || rows[0].Name != "no-sbx" {
		t.Fatalf("stored rows wrong: %+v", rows)
	}
	// Duplicate ⇒ 400.
	if w := postPolicy(t, mux, admin, map[string]any{"name": "no-sbx", "cedar_text": validPolicy}); w.Code != http.StatusBadRequest {
		t.Fatalf("duplicate: want 400 got %d", w.Code)
	}
}

func TestPolicyAPITenantScoping(t *testing.T) {
	ctx := context.Background()
	mux, ps := policyMux(t)
	t1admin := identity.Principal{Role: identity.RoleAdmin, TenantID: "t1"}
	t2admin := identity.Principal{Role: identity.RoleAdmin, TenantID: "t2"}
	su := identity.Principal{Role: identity.RoleAdmin, Superuser: true}

	// t1 admin creates a policy in t1.
	if w := postPolicy(t, mux, t1admin, map[string]any{"name": "p1", "cedar_text": validPolicy}); w.Code != http.StatusCreated {
		t.Fatalf("t1 create: %d", w.Code)
	}
	// t2 admin lists — must see none of t1's.
	r := withPrincipal(httptest.NewRequest("GET", "/admin/policies", nil), t2admin)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	var t2rows []policy.Row
	_ = json.Unmarshal(w.Body.Bytes(), &t2rows)
	if len(t2rows) != 0 {
		t.Fatalf("t2 must not see t1 policies: %+v", t2rows)
	}
	// Superuser targets t2 explicitly and creates there.
	if w := postPolicy(t, mux, su, map[string]any{"name": "sp", "cedar_text": validPolicy, "tenant": "t2"}); w.Code != http.StatusCreated {
		t.Fatalf("superuser cross-tenant create: %d (%s)", w.Code, w.Body)
	}
	if rows, _ := ps.List(ctx, "t2"); len(rows) != 1 {
		t.Fatalf("superuser policy not stored under t2: %+v", rows)
	}
	// Superuser without a tenant field ⇒ 400 (must specify).
	if w := postPolicy(t, mux, su, map[string]any{"name": "x", "cedar_text": validPolicy}); w.Code != http.StatusBadRequest {
		t.Fatalf("superuser no-tenant: want 400 got %d", w.Code)
	}
}

func TestPolicyDeleteNoOracle(t *testing.T) {
	ctx := context.Background()
	mux, ps := policyMux(t)
	_ = ps.Insert(ctx, policy.Row{Tenant: "t1", Name: "p1", CedarText: validPolicy})
	t2admin := identity.Principal{Role: identity.RoleAdmin, TenantID: "t2"}

	// t2 admin deletes t1's policy by name ⇒ 204 (no oracle), row survives.
	r := withPrincipal(httptest.NewRequest("DELETE", "/admin/policies/p1", nil), t2admin)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("cross-tenant delete: want 204 got %d", w.Code)
	}
	if rows, _ := ps.List(ctx, "t1"); len(rows) != 1 {
		t.Fatal("cross-tenant delete must NOT remove t1's policy")
	}
	// Deleting a missing policy in own tenant ⇒ 204 too.
	t1admin := identity.Principal{Role: identity.RoleAdmin, TenantID: "t1"}
	r = withPrincipal(httptest.NewRequest("DELETE", "/admin/policies/ghost", nil), t1admin)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("missing delete: want 204 got %d", w.Code)
	}
	// Real delete removes it.
	r = withPrincipal(httptest.NewRequest("DELETE", "/admin/policies/p1", nil), t1admin)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if rows, _ := ps.List(ctx, "t1"); len(rows) != 0 {
		t.Fatalf("own-tenant delete must remove the policy, got %+v", rows)
	}
}
