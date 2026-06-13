//go:build integration

package test

// Task 10 (v1.0-M1): end-to-end onboarding integration against real Postgres.
//
// This proves the onboarding slice wiring through the EXPORTED surface only:
// superuser-seeded tenant -> admin registers an upstream via the real
// controlplane.RegisterUpstreamAdmin HTTP handler -> row persisted via the real
// gateway.NewUpstreamStore -> gateway.Manager.Add was invoked (upstream appears
// in mgr.Status) -> cross-tenant isolation (a second tenant cannot list or
// delete it) -> owner delete removes it from BOTH the store and the manager.
//
// Credential injection is NOT re-proven here: it is already covered end-to-end by
// the in-package unit tests in internal/gateway/cred_test.go
// (TestCredentialInjection / TestCredentialMissingFailsClosed /
// TestCredentialSkippedWhenNotSingleTenant), which use the unexported WithDial
// dial seam to observe injected headers. That seam is not reachable from this
// external (package test) file. The manual live proof in the next phase covers
// credential injection against a real upstream.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/sausheong/runtime/controlplane"
	"github.com/sausheong/runtime/internal/gateway"
	"github.com/sausheong/runtime/internal/identity"
)

func TestOnboardingEndToEnd(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("pgx", gwStoreDSN) // gwStoreDSN: test/gateway_upstream_store_test.go
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	idStore, err := identity.NewStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	gwStore, err := gateway.NewUpstreamStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}

	// clean slate + two tenants (obe1 owns the upstream; obe2 is the outsider)
	for _, tn := range []string{"obe1", "obe2"} {
		_, _ = db.ExecContext(ctx, `DELETE FROM gateway_upstreams WHERE tenant_id=$1`, tn)
		_, _ = db.ExecContext(ctx, `DELETE FROM tenants WHERE id=$1`, tn)
		if err := idStore.CreateTenant(ctx, tn, tn); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, tn := range []string{"obe1", "obe2"} {
			_, _ = db.ExecContext(ctx, `DELETE FROM gateway_upstreams WHERE tenant_id=$1`, tn)
			_, _ = db.ExecContext(ctx, `DELETE FROM tenants WHERE id=$1`, tn)
		}
	})

	// Manager built WITHOUT a fake dialer: the default production dialer will try
	// to reach http://orders.local and fail, but the upstream still APPEARS in
	// Status (state "down"). We assert presence, not liveness — registration +
	// Add is what this test proves. WithBackoff(hour, hour) keeps the failing
	// dial from spinning hot. We never call Start: Add appends to the slice and
	// Status iterates that snapshot independently of Start.
	mgr := gateway.NewManager(nil, gateway.WithBackoff(time.Hour, time.Hour))

	mux := http.NewServeMux()
	controlplane.RegisterUpstreamAdmin(mux, idStore, gwStore, mgr)

	admin1 := identity.Principal{Role: identity.RoleAdmin, TenantID: "obe1"}
	admin2 := identity.Principal{Role: identity.RoleAdmin, TenantID: "obe2"}

	// register an upstream as obe1 via the real HTTP handler
	body, _ := json.Marshal(map[string]string{"name": "orders", "url": "http://orders.local"})
	r := withPrincipal(httptest.NewRequest("POST", "/admin/upstreams", bytes.NewReader(body)), admin1)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", w.Code, w.Body)
	}
	var created struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created: %v", err)
	}
	if created.ID == "" {
		t.Fatal("no id returned")
	}

	// manager saw the Add (present in the unscoped Status view)
	if got := mgr.Status(""); len(got) != 1 || got[0].Name != "orders" {
		t.Fatalf("manager Status after add: %+v", got)
	}

	// persisted + tenant-scoped in real Postgres
	rows, err := gwStore.ListUpstreams(ctx, "obe1")
	if err != nil || len(rows) != 1 {
		t.Fatalf("obe1 rows: %d err=%v", len(rows), err)
	}

	// cross-tenant isolation: obe2 GET sees nothing
	r2 := withPrincipal(httptest.NewRequest("GET", "/admin/upstreams", nil), admin2)
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, r2)
	if w2.Code != http.StatusOK {
		t.Fatalf("obe2 list: %d %s", w2.Code, w2.Body)
	}
	var list2 []gateway.UpstreamRow
	if err := json.Unmarshal(w2.Body.Bytes(), &list2); err != nil {
		t.Fatalf("decode obe2 list: %v", err)
	}
	if len(list2) != 0 {
		t.Fatalf("cross-tenant leak: %+v", list2)
	}

	// cross-tenant delete is a no-op (204, row survives)
	rd := withPrincipal(httptest.NewRequest("DELETE", "/admin/upstreams/"+created.ID, nil), admin2)
	wd := httptest.NewRecorder()
	mux.ServeHTTP(wd, rd)
	if wd.Code != http.StatusNoContent {
		t.Fatalf("cross-tenant delete: %d %s", wd.Code, wd.Body)
	}
	if rows, _ := gwStore.ListUpstreams(ctx, "obe1"); len(rows) != 1 {
		t.Fatal("cross-tenant delete must NOT remove the row")
	}

	// owner delete works: removed from store AND manager
	ro := withPrincipal(httptest.NewRequest("DELETE", "/admin/upstreams/"+created.ID, nil), admin1)
	wo := httptest.NewRecorder()
	mux.ServeHTTP(wo, ro)
	if wo.Code != http.StatusNoContent {
		t.Fatalf("owner delete: %d %s", wo.Code, wo.Body)
	}
	if rows, _ := gwStore.ListUpstreams(ctx, "obe1"); len(rows) != 0 {
		t.Fatal("owner delete must remove the row")
	}
	if got := mgr.Status(""); len(got) != 0 {
		t.Fatalf("manager Status after delete: %+v", got)
	}
}

// withPrincipal attaches a principal via the exported controlplane.WithPrincipal.
func withPrincipal(r *http.Request, p identity.Principal) *http.Request {
	return r.WithContext(controlplane.WithPrincipal(r.Context(), p))
}
