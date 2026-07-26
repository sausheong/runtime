//go:build integration

package gateway

import (
	"context"
	"database/sql"
	"github.com/sausheong/runtime/internal/pgtest"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// gatewayStoreTestDSN resolves from RUNTIME_TEST_PG_DSN / RUNTIME_PG_DSN. These tests DROP
// tables, so the override must be real. See internal/pgtest.
func gatewayStoreTestDSN() string { return pgtest.DSN() }

func TestUpstreamStoreRepairsMissingTenantForeignKeyAndRejectsOrphans(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("pgx", gatewayStoreTestDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		t.Skipf("postgres not reachable: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS tenants (
		    id TEXT PRIMARY KEY,
		    name TEXT NOT NULL DEFAULT ''
		)`); err != nil {
		t.Fatal(err)
	}
	if _, err := NewUpstreamStore(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`ALTER TABLE gateway_upstreams
		 DROP CONSTRAINT IF EXISTS gateway_upstreams_tenant_id_fkey`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO gateway_upstreams (id,tenant_id,name,transport)
		VALUES ('restore-orphan','missing-tenant','restore-orphan','http')`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(),
			`DELETE FROM gateway_upstreams WHERE id='restore-orphan'`)
		_, _ = NewUpstreamStore(context.Background(), db)
	})
	if _, err := NewUpstreamStore(ctx, db); err == nil {
		t.Fatal("schema repair accepted an orphaned gateway upstream")
	}
	var retained bool
	if err := db.QueryRowContext(ctx,
		`SELECT EXISTS (
		    SELECT 1 FROM gateway_upstreams WHERE id='restore-orphan'
		)`).Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if !retained {
		t.Fatal("failed schema repair destructively removed the orphan")
	}
	if _, err := db.ExecContext(ctx,
		`DELETE FROM gateway_upstreams WHERE id='restore-orphan'`); err != nil {
		t.Fatal(err)
	}
	if _, err := NewUpstreamStore(ctx, db); err != nil {
		t.Fatalf("repair missing gateway tenant foreign key: %v", err)
	}
	var validFK bool
	if err := db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_constraint
			 WHERE conrelid='gateway_upstreams'::regclass
			   AND confrelid='tenants'::regclass
			   AND contype='f' AND confdeltype='c'
		)`).Scan(&validFK); err != nil {
		t.Fatal(err)
	}
	if !validFK {
		t.Fatal("gateway_upstreams tenant foreign key was not repaired")
	}
}
