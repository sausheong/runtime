//go:build integration

package agentstore

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	runtimestore "github.com/sausheong/runtime/internal/store"
)

func agentStoreTestDSN() string {
	if dsn := os.Getenv("RUNTIME_PG_DSN"); dsn != "" {
		return dsn
	}
	return "postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable"
}

func TestRegistrationGenerationMigrationBackfillsLegacyRows(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("pgx", agentStoreTestDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		t.Skipf("postgres not reachable: %v", err)
	}
	_, _ = db.ExecContext(ctx, `DELETE FROM runtime_schema_migrations WHERE component='managed-agents'`)
	_, _ = db.ExecContext(ctx, `DROP TABLE IF EXISTS managed_agents`)
	if _, err := db.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS tenants (
		    id TEXT PRIMARY KEY,
		    name TEXT NOT NULL DEFAULT ''
		)`); err != nil {
		t.Fatal(err)
	}
	_, _ = db.ExecContext(ctx,
		`INSERT INTO tenants(id,name) VALUES('legacy-tenant','legacy')
		 ON CONFLICT (id) DO NOTHING`)
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DROP TABLE IF EXISTS managed_agents`)
		_, _ = db.ExecContext(ctx, `DELETE FROM tenants WHERE id='legacy-tenant'`)
	})

	if err := runtimestore.ApplyMigrationsLocked(ctx, db, "managed-agents", 1, 1,
		[]runtimestore.Migration{{Version: 1, Name: "baseline", SQL: schemaSQL}}); err != nil {
		t.Fatalf("install managed-agents v1: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO managed_agents(id,tenant_id,name,url)
		 VALUES('legacy-agent','legacy-tenant','legacy','https://example.com')`); err != nil {
		t.Fatal(err)
	}
	st, err := New(ctx, db)
	if err != nil {
		t.Fatalf("migrate managed-agents v1 to v2: %v", err)
	}
	row, ok, err := st.Get(ctx, "legacy-agent")
	if err != nil || !ok {
		t.Fatalf("get migrated agent ok=%v err=%v", ok, err)
	}
	if row.RegistrationGeneration == "" {
		t.Fatal("legacy managed agent has empty registration generation")
	}
}

func TestStoreRepairsMissingTenantForeignKeyAtCurrentLedger(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("pgx", agentStoreTestDSN())
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
	if _, err := New(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`ALTER TABLE managed_agents DROP CONSTRAINT IF EXISTS managed_agents_tenant_id_fkey`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO managed_agents
		    (id,tenant_id,name,url,registration_generation)
		VALUES
		    ('restore-orphan','missing-tenant','restore-orphan',
		     'https://example.com','restore-generation')`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(),
			`DELETE FROM managed_agents WHERE id='restore-orphan'`)
		_, _ = New(context.Background(), db)
	})
	if _, err := New(ctx, db); err == nil {
		t.Fatal("schema repair accepted an orphaned managed agent")
	}
	var retained bool
	if err := db.QueryRowContext(ctx,
		`SELECT EXISTS (
		    SELECT 1 FROM managed_agents WHERE id='restore-orphan'
		)`).Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if !retained {
		t.Fatal("failed schema repair destructively removed the orphan")
	}
	if _, err := db.ExecContext(ctx,
		`DELETE FROM managed_agents WHERE id='restore-orphan'`); err != nil {
		t.Fatal(err)
	}
	if _, err := New(ctx, db); err != nil {
		t.Fatalf("repair managed-agent schema with current ledger: %v", err)
	}
	var validFK bool
	if err := db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_constraint
			 WHERE conrelid='managed_agents'::regclass
			   AND confrelid='tenants'::regclass
			   AND contype='f' AND confdeltype='c'
		)`).Scan(&validFK); err != nil {
		t.Fatal(err)
	}
	if !validFK {
		t.Fatal("managed_agents tenant foreign key was not repaired")
	}
}
