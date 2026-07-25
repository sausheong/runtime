//go:build integration

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"
)

// Matches the DSN convention used by the integration tests in test/.
const pgTestDSN = "postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable"

func newPGTestStore(t *testing.T) Store {
	t.Helper()
	ctx := context.Background()
	st, err := NewPGStore(ctx, pgTestDSN)
	if err != nil {
		t.Fatalf("NewPGStore (is postgres running at %s?): %v", pgTestDSN, err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestPGLimitExceededIsTerminalForActiveCount(t *testing.T) {
	// A limit_exceeded session must NOT count as active load — otherwise the
	// autoscaler can never drain a replica that hosted a breached session.
	st := newPGTestStore(t)
	ctx := context.Background()
	const agentID = "pg-limit-terminal-test"
	id, err := st.CreateSession(ctx, agentID, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		p := st.(*pgStore)
		_, _ = p.db.ExecContext(context.Background(), `DELETE FROM sessions WHERE id=$1`, id)
	})
	if err := st.SetSessionStatus(ctx, id, "limit_exceeded"); err != nil {
		t.Fatal(err)
	}
	otherTenantID, err := st.CreateSessionForTenant(ctx, "other-tenant", agentID, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		p := st.(*pgStore)
		_, _ = p.db.ExecContext(context.Background(), `DELETE FROM sessions WHERE id=$1`, otherTenantID)
	})
	m, err := st.ActiveSessionsByReplica(ctx, "default", agentID)
	if err != nil {
		t.Fatal(err)
	}
	if m[0] != 0 {
		t.Errorf("limit_exceeded counted as active: %v", m)
	}
}

func TestPGFailureCategory(t *testing.T) {
	st := newPGTestStore(t)
	ctx := context.Background()
	const agentID = "pg-failcat-test"
	// Clean any prior rows for a repeatable run.
	p := st.(*pgStore)
	t.Cleanup(func() {
		_, _ = p.db.ExecContext(context.Background(), `DELETE FROM sessions WHERE agent_id=$1`, agentID)
	})
	_, _ = p.db.ExecContext(ctx, `DELETE FROM sessions WHERE agent_id=$1`, agentID)

	mk := func(cat string) string {
		id, err := st.CreateSession(ctx, agentID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if cat != "" {
			if err := st.SetFailureCategory(ctx, id, cat); err != nil {
				t.Fatal(err)
			}
			// Idempotent re-set (replay).
			if err := st.SetFailureCategory(ctx, id, cat); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	id1 := mk("tool_error")
	mk("tool_error")
	mk("none")
	mk("") // unclassified — must be omitted from the breakdown
	otherTenantID, err := st.CreateSessionForTenant(ctx, "other-tenant", agentID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetFailureCategory(ctx, otherTenantID, "tool_error"); err != nil {
		t.Fatal(err)
	}

	// GetSession round-trips the column.
	row, err := st.GetSession(ctx, id1)
	if err != nil {
		t.Fatal(err)
	}
	if row.FailureCategory != "tool_error" {
		t.Fatalf("GetSession category=%q, want tool_error", row.FailureCategory)
	}

	// Breakdown groups and omits ''.
	got, err := st.FailureBreakdownByAgent(ctx, "default", agentID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if got["tool_error"] != 2 || got["none"] != 1 || len(got) != 2 {
		t.Fatalf("breakdown=%v, want {tool_error:2, none:1}", got)
	}

	// since in the future ⇒ empty (all rows are older than a far-future cutoff).
	future := time.Now().Add(24 * time.Hour)
	got2, err := st.FailureBreakdownByAgent(ctx, "default", agentID, future)
	if err != nil {
		t.Fatal(err)
	}
	if len(got2) != 0 {
		t.Fatalf("future-since breakdown=%v, want empty", got2)
	}
}

func TestPGTenantOwnershipAndSessionRetention(t *testing.T) {
	st := newPGTestStore(t)
	ctx := context.Background()
	p := st.(*pgStore)

	oldTerminal, err := st.CreateSessionForTenant(ctx, "alpha", "pg-retention-test", 0)
	if err != nil {
		t.Fatal(err)
	}
	newTerminal, err := st.CreateSessionForTenant(ctx, "beta", "pg-retention-test", 0)
	if err != nil {
		t.Fatal(err)
	}
	active, err := st.CreateSessionForTenant(ctx, "alpha", "pg-retention-test", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = p.db.ExecContext(context.Background(),
			`DELETE FROM sessions WHERE id IN ($1,$2,$3)`,
			oldTerminal, newTerminal, active)
	})

	row, err := st.GetSession(ctx, oldTerminal)
	if err != nil || row.TenantID != "alpha" {
		t.Fatalf("PostgreSQL tenant round-trip: row=%+v err=%v", row, err)
	}
	if err := st.SetSessionStatus(ctx, oldTerminal, "completed"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSessionStatus(ctx, newTerminal, "error"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.db.ExecContext(ctx,
		`UPDATE sessions
		    SET last_active_at = CASE id
		        WHEN $1 THEN now() - interval '2 hours'
		        WHEN $2 THEN now() - interval '30 minutes'
		        ELSE now() - interval '3 hours'
		    END
		  WHERE id IN ($1,$2,$3)`,
		oldTerminal, newTerminal, active); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendEvent(ctx, oldTerminal, "done", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendTranscript(ctx, oldTerminal, 0, "alpha", "user", []byte(`[]`), "completed", "completed"); err != nil {
		t.Fatal(err)
	}
	if err := st.PutOnlineResult(ctx, oldTerminal, "quality", "alpha", "user", "contains", true, "ok"); err != nil {
		t.Fatal(err)
	}

	cutoff := time.Now().Add(-time.Hour)
	if n, err := st.ReapSessions(ctx, cutoff, 1, true); err != nil || n != 1 {
		t.Fatalf("PostgreSQL retention dry run n=%d err=%v", n, err)
	}
	if _, err := st.GetSession(ctx, oldTerminal); err != nil {
		t.Fatalf("dry run deleted terminal session: %v", err)
	}
	if n, err := st.ReapSessions(ctx, cutoff, 1, false); err != nil || n != 1 {
		t.Fatalf("PostgreSQL retention reap n=%d err=%v", n, err)
	}
	if _, err := st.GetSession(ctx, oldTerminal); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("old terminal session still present: %v", err)
	}
	for table, key := range map[string]string{
		"session_events":      "session_id",
		"session_transcripts": "session_id",
		"online_eval_results": "session_id",
	} {
		var count int
		if err := p.db.QueryRowContext(ctx,
			`SELECT count(*) FROM `+table+` WHERE `+key+`=$1`, oldTerminal).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Errorf("%s retained %d child rows", table, count)
		}
	}
	if _, err := st.GetSession(ctx, newTerminal); err != nil {
		t.Fatalf("newer terminal session deleted: %v", err)
	}
	if _, err := st.GetSession(ctx, active); err != nil {
		t.Fatalf("active session deleted: %v", err)
	}
}

func TestSchemaMigrationsOrderedIdempotentAndVersionChecked(t *testing.T) {
	db, err := sql.Open("pgx", pgTestDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	component := fmt.Sprintf("test-migrations-%d", time.Now().UnixNano())
	table := fmt.Sprintf("migration_test_%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM runtime_schema_migrations WHERE component=$1`, component)
		_, _ = db.Exec(`DROP TABLE IF EXISTS ` + table)
	})
	migrations := []Migration{
		{Version: 1, Name: "create", SQL: `CREATE TABLE ` + table + ` (id INT PRIMARY KEY)`},
		{Version: 2, Name: "add-name", SQL: `ALTER TABLE ` + table + ` ADD COLUMN name TEXT`},
	}
	if err := ApplyMigrationsLocked(ctx, db, component, 1, 2, migrations); err != nil {
		t.Fatal(err)
	}
	if err := ApplyMigrationsLocked(ctx, db, component, 1, 2, migrations); err != nil {
		t.Fatalf("idempotent apply: %v", err)
	}
	if err := CheckSchemaVersion(ctx, db, component, 1, 2); err != nil {
		t.Fatal(err)
	}
	var versions int
	if err := db.QueryRow(`SELECT count(*) FROM runtime_schema_migrations WHERE component=$1`, component).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != 2 {
		t.Fatalf("ledger versions=%d, want 2", versions)
	}
	if _, err := db.Exec(`INSERT INTO runtime_schema_migrations(component,version,name,checksum) VALUES ($1,3,'future','future')`, component); err != nil {
		t.Fatal(err)
	}
	if err := CheckSchemaVersion(ctx, db, component, 1, 2); err == nil {
		t.Fatal("newer unsupported schema accepted")
	}
}

func TestFailedMigrationRollsBackLedgerAndDDL(t *testing.T) {
	db, err := sql.Open("pgx", pgTestDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	component := fmt.Sprintf("test-rollback-%d", time.Now().UnixNano())
	table := fmt.Sprintf("migration_rollback_%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM runtime_schema_migrations WHERE component=$1`, component)
		_, _ = db.Exec(`DROP TABLE IF EXISTS ` + table)
	})
	err = ApplyMigrationsLocked(ctx, db, component, 1, 2, []Migration{
		{Version: 1, Name: "create", SQL: `CREATE TABLE ` + table + ` (id INT)`},
		{Version: 2, Name: "fail", SQL: `ALTER TABLE definitely_missing_runtime_table ADD COLUMN broken INT`},
	})
	if err == nil {
		t.Fatal("broken migration succeeded")
	}
	var exists bool
	if err := db.QueryRow(`SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("failed migration left DDL behind")
	}
	var versions int
	if err := db.QueryRow(`SELECT count(*) FROM runtime_schema_migrations WHERE component=$1`, component).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != 0 {
		t.Fatalf("failed migration advanced ledger to %d", versions)
	}
}

func TestLegacySessionQuarantineOnlyTouchesPreLedgerDefaultRows(t *testing.T) {
	db, err := sql.Open("pgx", pgTestDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, statement := range []string{
		`CREATE TEMP TABLE sessions (id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL)`,
		`CREATE TEMP TABLE runtime_schema_migrations (component TEXT, version INT, applied_at TIMESTAMPTZ)`,
		`INSERT INTO runtime_schema_migrations VALUES ('core',1,now())`,
		`INSERT INTO sessions VALUES
			('legacy-default','default',now()-interval '1 day'),
			('new-default','default',now()+interval '1 day'),
			('legacy-alpha','alpha',now()-interval '1 day')`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.ExecContext(ctx, quarantineLegacySessionsSQL); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, tenant_id FROM sessions ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var id, tenant string
		if err := rows.Scan(&id, &tenant); err != nil {
			t.Fatal(err)
		}
		got[id] = tenant
	}
	if got["legacy-default"] != "__legacy_unowned__" ||
		got["new-default"] != "default" ||
		got["legacy-alpha"] != "alpha" {
		t.Fatalf("quarantine result=%v", got)
	}
}
