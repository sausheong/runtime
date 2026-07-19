package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sausheong/runtime/internal/config"
	"github.com/sausheong/runtime/internal/gateway"
	"github.com/sausheong/runtime/internal/identity"
)

type fakeUpstreamStore struct {
	rows map[string]gateway.UpstreamRow
}

func (f *fakeUpstreamStore) InsertUpstream(ctx context.Context, r gateway.UpstreamRow) error {
	if f.rows == nil {
		f.rows = map[string]gateway.UpstreamRow{}
	}
	// Simulate the (tenant_id, name) unique constraint the real Postgres store
	// enforces: a real violation surfaces "duplicate key value violates unique
	// constraint ...".
	for _, existing := range f.rows {
		if existing.TenantID == r.TenantID && existing.Name == r.Name {
			return errors.New("duplicate key value violates unique constraint \"gateway_upstreams_tenant_id_name_key\"")
		}
	}
	f.rows[r.ID] = r
	return nil
}
func (f *fakeUpstreamStore) ListUpstreams(ctx context.Context, tenant string) ([]gateway.UpstreamRow, error) {
	var out []gateway.UpstreamRow
	for _, r := range f.rows {
		if tenant == "" || r.TenantID == tenant {
			out = append(out, r)
		}
	}
	return out, nil
}
func (f *fakeUpstreamStore) GetUpstream(ctx context.Context, id string) (gateway.UpstreamRow, bool, error) {
	r, ok := f.rows[id]
	return r, ok, nil
}
func (f *fakeUpstreamStore) DeleteUpstream(ctx context.Context, tenant, id string) error {
	if r, ok := f.rows[id]; ok && r.TenantID == tenant {
		delete(f.rows, id)
	}
	return nil
}

type fakeMutator struct {
	added   []string
	removed []string
}

func (f *fakeMutator) Add(cfg config.GatewayServer) error {
	f.added = append(f.added, cfg.Name)
	return nil
}
func (f *fakeMutator) Remove(name string)                            { f.removed = append(f.removed, name) }
func (f *fakeMutator) Status(tenant string) []gateway.UpstreamStatus { return nil }

func postUpstream(t *testing.T, h http.Handler, p identity.Principal, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	r := withPrincipal(httptest.NewRequest("POST", "/admin/upstreams", bytes.NewReader(b)), p)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestUpstreamAPIRBACAndValidation(t *testing.T) {
	mux := http.NewServeMux()
	us := &fakeUpstreamStore{}
	mut := &fakeMutator{}
	store := newFakeAdminStore()
	store.CreateTenant(context.Background(), "t1", "t1")
	RegisterUpstreamAdmin(mux, store, us, mut, nil)

	admin := identity.Principal{Role: identity.RoleAdmin, TenantID: "t1"}
	viewer := identity.Principal{Role: identity.RoleViewer, TenantID: "t1"}

	if w := postUpstream(t, mux, viewer, map[string]any{"name": "o", "url": "http://x"}); w.Code != http.StatusForbidden {
		t.Fatalf("viewer: want 403 got %d", w.Code)
	}
	if w := postUpstream(t, mux, admin, map[string]any{"name": "o", "command": "sh"}); w.Code != http.StatusBadRequest {
		t.Fatalf("stdio: want 400 got %d", w.Code)
	}
	if w := postUpstream(t, mux, admin, map[string]any{"name": "o"}); w.Code != http.StatusBadRequest {
		t.Fatalf("no transport: want 400 got %d", w.Code)
	}
	w := postUpstream(t, mux, admin, map[string]any{"name": "orders", "url": "http://x"})
	if w.Code != http.StatusCreated {
		t.Fatalf("create: want 201 got %d (%s)", w.Code, w.Body)
	}
	if len(mut.added) != 1 || mut.added[0] != "orders" {
		t.Fatalf("manager not called: %+v", mut.added)
	}
	rows, _ := us.ListUpstreams(context.Background(), "t1")
	if len(rows) != 1 || rows[0].TenantID != "t1" {
		t.Fatalf("row tenant scoping wrong: %+v", rows)
	}
}

func TestUpstreamCredBothOrNeither(t *testing.T) {
	mux := http.NewServeMux()
	store := newFakeAdminStore()
	store.CreateTenant(context.Background(), "t1", "t1")
	RegisterUpstreamAdmin(mux, store, &fakeUpstreamStore{}, &fakeMutator{}, nil)
	admin := identity.Principal{Role: identity.RoleAdmin, TenantID: "t1"}
	// cred_secret without cred_header → 400
	w := postUpstream(t, mux, admin, map[string]any{"name": "o", "url": "http://x", "cred_secret": "K"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("cred_secret alone: want 400 got %d", w.Code)
	}
}

func TestUpstreamRollbackOnManagerError(t *testing.T) {
	mux := http.NewServeMux()
	store := newFakeAdminStore()
	store.CreateTenant(context.Background(), "t1", "t1")
	us := &fakeUpstreamStore{}
	mut := &errMutator{} // Add always errors
	RegisterUpstreamAdmin(mux, store, us, mut, nil)
	admin := identity.Principal{Role: identity.RoleAdmin, TenantID: "t1"}
	w := postUpstream(t, mux, admin, map[string]any{"name": "orders", "url": "http://x"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("manager add error: want 400 got %d (%s)", w.Code, w.Body)
	}
	// row must have been rolled back
	rows, _ := us.ListUpstreams(context.Background(), "t1")
	if len(rows) != 0 {
		t.Fatalf("expected rollback to leave 0 rows, got %d", len(rows))
	}
}

func TestUpstreamDeleteCrossTenantNoOp(t *testing.T) {
	ctx := context.Background()
	mux := http.NewServeMux()
	store := newFakeAdminStore()
	store.CreateTenant(ctx, "t1", "t1")
	store.CreateTenant(ctx, "t2", "t2")
	us := &fakeUpstreamStore{rows: map[string]gateway.UpstreamRow{
		"gwu-x": {ID: "gwu-x", TenantID: "t1", Name: "orders", Transport: "http", URL: "http://x"},
	}}
	mut := &fakeMutator{}
	RegisterUpstreamAdmin(mux, store, us, mut, nil)
	// admin of t2 tries to delete t1's upstream by id
	admin2 := identity.Principal{Role: identity.RoleAdmin, TenantID: "t2"}
	r := withPrincipal(httptest.NewRequest("DELETE", "/admin/upstreams/gwu-x", nil), admin2)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("cross-tenant delete: want 204 (no oracle) got %d", w.Code)
	}
	// row must survive; manager.Remove must NOT have been called
	if _, ok, _ := us.GetUpstream(ctx, "gwu-x"); !ok {
		t.Fatal("cross-tenant delete must NOT remove the row")
	}
	if len(mut.removed) != 0 {
		t.Fatalf("manager.Remove must not be called cross-tenant, got %v", mut.removed)
	}
}

func TestUpstreamDuplicateNameFriendlyError(t *testing.T) {
	mux := http.NewServeMux()
	store := newFakeAdminStore()
	store.CreateTenant(context.Background(), "t1", "t1")
	us := &fakeUpstreamStore{}
	mut := &fakeMutator{}
	RegisterUpstreamAdmin(mux, store, us, mut, nil)
	admin := identity.Principal{Role: identity.RoleAdmin, TenantID: "t1"}

	if w := postUpstream(t, mux, admin, map[string]any{"name": "orders", "url": "http://x"}); w.Code != http.StatusCreated {
		t.Fatalf("first register: want 201 got %d (%s)", w.Code, w.Body)
	}
	w := postUpstream(t, mux, admin, map[string]any{"name": "orders", "url": "http://y"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("duplicate register: want 400 got %d (%s)", w.Code, w.Body)
	}
	body := w.Body.String()
	if !strings.Contains(body, "orders") {
		t.Fatalf("duplicate error should name the upstream: %q", body)
	}
	if strings.Contains(body, "constraint") || strings.Contains(body, "gateway_upstreams") || strings.Contains(body, "duplicate key") {
		t.Fatalf("duplicate error leaks schema internals: %q", body)
	}
}

// errMutator's Add always fails (to exercise rollback).
type errMutator struct{ fakeMutator }

func (e *errMutator) Add(cfg config.GatewayServer) error { return errors.New("boom") }
