//go:build integration

package store_test

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/sausheong/runtime/internal/identity"
	"github.com/sausheong/runtime/internal/store"
)

const ownerTestDSN = "postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable"

func TestRestrictedAgentRoleCannotReadOtherTenantOrIdentityTables(t *testing.T) {
	agentDSN := os.Getenv("RUNTIME_AGENT_PG_DSN")
	if agentDSN == "" {
		t.Skip("RUNTIME_AGENT_PG_DSN is not configured")
	}
	u, err := url.Parse(agentDSN)
	if err != nil || u.User == nil || u.User.Username() == "" {
		t.Fatalf("invalid RUNTIME_AGENT_PG_DSN: %v", err)
	}
	role := u.User.Username()
	ctx := context.Background()
	ownerDB, err := sql.Open("pgx", ownerTestDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer ownerDB.Close()
	var ownerRole string
	if err := ownerDB.QueryRowContext(ctx, `SELECT current_user`).Scan(&ownerRole); err != nil {
		t.Fatal(err)
	}
	if _, err := identity.NewStore(ctx, ownerDB); err != nil {
		t.Fatal(err)
	}
	ownerStore, err := store.NewPGStore(ctx, ownerTestDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer ownerStore.Close()
	if err := store.ProvisionAgentRole(ctx, ownerDB, ownerRole, "alpha", "agent-a", false); err == nil {
		t.Fatal("control-plane database role was accepted as an agent role")
	}
	if err := store.ProvisionAgentRole(ctx, ownerDB, role, "alpha", "agent-a", true); err != nil {
		t.Fatal(err)
	}
	if err := store.ConfigureAgentRole(ctx, ownerDB, role, "alpha", "agent-b", false); err == nil {
		t.Fatal("production role binding allowed a same-tenant agent reassignment")
	}
	alphaID, err := ownerStore.CreateSessionForIdentity(ctx, "alpha", "agent-a", "generation-a", 0)
	if err != nil {
		t.Fatal(err)
	}
	sameTenantOtherAgentID, err := ownerStore.CreateSessionForIdentity(ctx, "alpha", "agent-b", "generation-b", 0)
	if err != nil {
		t.Fatal(err)
	}
	betaID, err := ownerStore.CreateSessionForIdentity(ctx, "beta", "agent-a", "generation-c", 0)
	if err != nil {
		t.Fatal(err)
	}
	otherSchema := store.AgentDBOSSchema("alpha", "agent-b")
	if _, err := ownerDB.ExecContext(ctx,
		`CREATE SCHEMA IF NOT EXISTS "`+otherSchema+`";
		 CREATE TABLE IF NOT EXISTS "`+otherSchema+`".workflow_probe (id INT)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = ownerDB.Exec(
			`DELETE FROM sessions WHERE id IN ($1,$2,$3)`,
			alphaID, sameTenantOtherAgentID, betaID)
		_, _ = ownerDB.Exec(`DROP SCHEMA IF EXISTS "` + otherSchema + `" CASCADE`)
	})

	agentDB, err := sql.Open("pgx", agentDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer agentDB.Close()
	var visible []string
	rows, err := agentDB.QueryContext(
		ctx,
		`SELECT id FROM sessions WHERE id IN ($1,$2,$3) ORDER BY id`,
		alphaID, sameTenantOtherAgentID, betaID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		visible = append(visible, id)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if len(visible) != 1 || visible[0] != alphaID {
		t.Fatalf("restricted role saw sessions %v, want only alpha session %q", visible, alphaID)
	}
	if _, err := agentDB.ExecContext(ctx,
		`UPDATE sessions SET turn_count=999 WHERE id=$1`, sameTenantOtherAgentID); err != nil {
		t.Fatal(err)
	} else {
		var turns int
		if err := ownerDB.QueryRowContext(ctx,
			`SELECT turn_count FROM sessions WHERE id=$1`, sameTenantOtherAgentID).Scan(&turns); err != nil {
			t.Fatal(err)
		}
		if turns == 999 {
			t.Fatal("restricted role mutated another same-tenant agent's session")
		}
	}
	if _, err := agentDB.ExecContext(ctx,
		`SELECT 1 FROM "`+otherSchema+`".workflow_probe LIMIT 1`); err == nil {
		t.Fatal("restricted role read another same-tenant agent's DBOS schema")
	}
	for _, table := range []string{"identity_users", "service_keys", "secrets"} {
		if _, err := agentDB.ExecContext(ctx, `SELECT 1 FROM "`+table+`" LIMIT 1`); err == nil {
			t.Errorf("restricted role unexpectedly read %s", table)
		}
	}
	if _, err := agentDB.ExecContext(ctx, `SELECT 1 FROM agents LIMIT 1`); err == nil {
		t.Error("restricted role unexpectedly read global agents metadata")
	}
	if _, err := agentDB.ExecContext(ctx, `CREATE TABLE runtime_agent_must_not_create (id INT)`); err == nil {
		t.Error("restricted role unexpectedly created an object in public")
	}
	if _, err := agentDB.ExecContext(ctx,
		`INSERT INTO session_transcripts
		    (session_id, turn_index, tenant, actor_id, entries)
		 VALUES ($1, 999, 'beta', 'mallory', '[]'::jsonb)`,
		alphaID); err == nil {
		t.Error("restricted role stored a child row with a tenant different from its parent session")
	}
}

func TestProvisionAgentRoleRejectsElevatedCatalogRole(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("pgx", ownerTestDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var privilegedRole string
	if err := db.QueryRowContext(ctx, `
		SELECT rolname
		  FROM pg_roles
		 WHERE rolsuper OR rolcreaterole OR rolcreatedb OR rolbypassrls
		 ORDER BY rolname
		 LIMIT 1`).Scan(&privilegedRole); err != nil {
		t.Fatal(err)
	}
	if err := store.ProvisionAgentRole(ctx, db, privilegedRole, "alpha", "agent-a", false); err == nil {
		t.Fatalf("elevated catalog role %q was accepted as an agent role", privilegedRole)
	}
	var memberRole string
	if err := db.QueryRowContext(ctx, `
		SELECT member.rolname
		  FROM pg_auth_members membership
		  JOIN pg_roles member ON member.oid = membership.member
		 ORDER BY member.rolname
		 LIMIT 1`).Scan(&memberRole); err != nil {
		t.Fatal(err)
	}
	if err := store.ProvisionAgentRole(ctx, db, memberRole, "alpha", "agent-a", false); err == nil {
		t.Fatalf("role %q with inherited membership was accepted as an agent role", memberRole)
	}
}
