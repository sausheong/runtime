package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	RegisterUpstreamAdmin(mux, store, us, mut)

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
