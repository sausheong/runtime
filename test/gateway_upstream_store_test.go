//go:build integration

package test

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/sausheong/runtime/internal/gateway"
	"github.com/sausheong/runtime/internal/identity"
)

const gwStoreDSN = "postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable"

func TestGatewayUpstreamStoreCRUD(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("pgx", gwStoreDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := identity.NewStore(ctx, db); err != nil {
		t.Fatal(err)
	}
	st, err := gateway.NewUpstreamStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = db.ExecContext(ctx, `DELETE FROM gateway_upstreams WHERE tenant_id='gwt'`)
	_, _ = db.ExecContext(ctx, `DELETE FROM tenants WHERE id='gwt'`)
	if _, err := db.ExecContext(ctx, `INSERT INTO tenants(id,name) VALUES('gwt','gwt')`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM gateway_upstreams WHERE tenant_id='gwt'`)
		_, _ = db.ExecContext(ctx, `DELETE FROM tenants WHERE id='gwt'`)
	})

	row := gateway.UpstreamRow{
		ID: "gwu-1", TenantID: "gwt", Name: "orders", Transport: "openapi",
		OpenAPI: "http://spec", BaseURL: "http://api", Operations: []string{"listOrders"},
		CredSecret: "ORDERS_KEY", CredHeader: "Authorization",
	}
	if err := st.InsertUpstream(ctx, row); err != nil {
		t.Fatal(err)
	}
	dup := row
	dup.ID = "gwu-2"
	if err := st.InsertUpstream(ctx, dup); err == nil {
		t.Fatal("expected duplicate (tenant,name) to fail")
	}
	got, err := st.ListUpstreams(ctx, "gwt")
	if err != nil || len(got) != 1 || got[0].Name != "orders" || got[0].CredSecret != "ORDERS_KEY" {
		t.Fatalf("list mismatch: %+v err=%v", got, err)
	}
	cfg := got[0].ToConfig()
	if len(cfg.Tenants) != 1 || cfg.Tenants[0] != "gwt" || cfg.OpenAPI != "http://spec" {
		t.Fatalf("toConfig mismatch: %+v", cfg)
	}
	all, err := st.ListUpstreams(ctx, "")
	if err != nil || len(all) < 1 {
		t.Fatalf("all-list mismatch: %+v err=%v", all, err)
	}
	one, ok, err := st.GetUpstream(ctx, "gwu-1")
	if err != nil || !ok || one.Name != "orders" {
		t.Fatalf("get mismatch: %+v ok=%v err=%v", one, ok, err)
	}
	if err := st.DeleteUpstream(ctx, "gwt", "gwu-1"); err != nil {
		t.Fatal(err)
	}
	got, _ = st.ListUpstreams(ctx, "gwt")
	if len(got) != 0 {
		t.Fatalf("expected empty after delete, got %+v", got)
	}
}

func TestGatewayUpstreamStoreNilOperations(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("pgx", gwStoreDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := identity.NewStore(ctx, db); err != nil {
		t.Fatal(err)
	}
	st, err := gateway.NewUpstreamStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = db.ExecContext(ctx, `DELETE FROM gateway_upstreams WHERE tenant_id='gwtnil'`)
	_, _ = db.ExecContext(ctx, `DELETE FROM tenants WHERE id='gwtnil'`)
	if _, err := db.ExecContext(ctx, `INSERT INTO tenants(id,name) VALUES('gwtnil','gwtnil')`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM gateway_upstreams WHERE tenant_id='gwtnil'`)
		_, _ = db.ExecContext(ctx, `DELETE FROM tenants WHERE id='gwtnil'`)
	})

	// http upstream with NO operations (nil) — must NOT violate the
	// operations TEXT[] NOT NULL constraint (regression guard for pq.Array(nil)→NULL).
	row := gateway.UpstreamRow{
		ID: "gwu-nil", TenantID: "gwtnil", Name: "noops", Transport: "http",
		URL: "http://x", Operations: nil,
	}
	if err := st.InsertUpstream(ctx, row); err != nil {
		t.Fatalf("insert with nil operations must succeed, got: %v", err)
	}
	got, err := st.ListUpstreams(ctx, "gwtnil")
	if err != nil || len(got) != 1 {
		t.Fatalf("list: %+v err=%v", got, err)
	}
	if len(got[0].Operations) != 0 {
		t.Fatalf("nil operations should round-trip to empty, got %+v", got[0].Operations)
	}
}
