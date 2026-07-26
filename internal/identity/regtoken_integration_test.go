//go:build integration

package identity

import (
	"context"
	"database/sql"
	"github.com/sausheong/runtime/internal/pgtest"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	runtimestore "github.com/sausheong/runtime/internal/store"
)

// regDSN resolves from RUNTIME_TEST_PG_DSN / RUNTIME_PG_DSN. These tests DROP
// tables, so the override must be real. See internal/pgtest.
func regDSN() string { return pgtest.DSN() }

func TestRegistrationTokenLegacyMigrationFailsClosed(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("pgx", regDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, _ = db.ExecContext(ctx, `DELETE FROM runtime_schema_migrations WHERE component='identity'`)
	_, _ = db.ExecContext(ctx, `DROP TABLE IF EXISTS registration_tokens`)

	if err := runtimestore.ApplyMigrationsLocked(ctx, db, "identity", 1, 1,
		[]runtimestore.Migration{{Version: 1, Name: "baseline", SQL: schemaSQL}}); err != nil {
		t.Fatalf("install identity v1: %v", err)
	}
	legacy, _ := MintServiceKey()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO registration_tokens (token_id,agent_id,hash)
		 VALUES ($1,'legacy-agent',$2)`, legacy.ID, legacy.Hash); err != nil {
		t.Fatal(err)
	}

	st, err := NewStore(ctx, db)
	if err != nil {
		t.Fatalf("migrate identity v1 to v2: %v", err)
	}
	if _, err := st.ActiveRegTokenByID(ctx, legacy.ID); err != ErrNoRegToken {
		t.Fatalf("migrated legacy token error=%v want ErrNoRegToken", err)
	}
	var tenant, generation string
	if err := db.QueryRowContext(ctx,
		`SELECT tenant_id,agent_generation FROM registration_tokens WHERE token_id=$1`,
		legacy.ID).Scan(&tenant, &generation); err != nil {
		t.Fatal(err)
	}
	if tenant != "" || generation != "" {
		t.Fatalf("legacy token was unsafely inferred: tenant=%q generation=%q",
			tenant, generation)
	}
}

func TestRegistrationTokenCRUD(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("pgx", regDSN())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	// Self-clean.
	_, _ = db.ExecContext(ctx, `DELETE FROM runtime_schema_migrations WHERE component='identity'`)
	_, _ = db.ExecContext(ctx, `DROP TABLE IF EXISTS registration_tokens`)

	st, err := NewStore(ctx, db)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	mk, err := MintServiceKey()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if err := st.InsertRegistrationToken(ctx, mk.ID, "agent-x", "tenant-x", "generation-x", mk.Hash); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// Active lookup resolves the immutable binding + hash.
	credential, err := st.ActiveRegTokenByID(ctx, mk.ID)
	if err != nil || credential.AgentID != "agent-x" ||
		credential.TenantID != "tenant-x" ||
		credential.AgentGeneration != "generation-x" ||
		credential.Hash != mk.Hash {
		t.Fatalf("active lookup: credential=%+v err=%v", credential, err)
	}
	// List shows it, never the secret.
	rows, err := st.ListRegistrationTokens(ctx)
	if err != nil || len(rows) != 1 || rows[0].AgentID != "agent-x" ||
		rows[0].TenantID != "tenant-x" ||
		rows[0].AgentGeneration != "generation-x" || rows[0].Revoked {
		t.Fatalf("list: %+v err=%v", rows, err)
	}
	// Revoke → active lookup fails closed.
	if err := st.RevokeRegistrationToken(ctx, mk.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := st.ActiveRegTokenByID(ctx, mk.ID); err != ErrNoRegToken {
		t.Fatalf("want ErrNoRegToken after revoke, got %v", err)
	}
	rows, _ = st.ListRegistrationTokens(ctx)
	if len(rows) != 1 || !rows[0].Revoked {
		t.Fatalf("list after revoke: %+v", rows)
	}

	// Legacy version-1 rows have no trustworthy tenant/generation binding and
	// must fail closed after migration.
	legacy, _ := MintServiceKey()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO registration_tokens
		    (token_id,agent_id,tenant_id,agent_generation,hash)
		 VALUES ($1,'agent-x','','',$2)`, legacy.ID, legacy.Hash); err != nil {
		t.Fatalf("insert legacy fixture: %v", err)
	}
	if _, err := st.ActiveRegTokenByID(ctx, legacy.ID); err != ErrNoRegToken {
		t.Fatalf("legacy unbound token error=%v want ErrNoRegToken", err)
	}

	instance, _ := MintServiceKey()
	if err := st.InsertRegistrationToken(ctx, instance.ID, "agent-y",
		"tenant-y", "generation-y", instance.Hash); err != nil {
		t.Fatal(err)
	}
	if err := st.RevokeRegistrationTokensForAgent(ctx, "tenant-y",
		"agent-y", "generation-y"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ActiveRegTokenByID(ctx, instance.ID); err != ErrNoRegToken {
		t.Fatalf("generation revocation error=%v want ErrNoRegToken", err)
	}
}
