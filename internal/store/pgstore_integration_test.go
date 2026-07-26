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

func TestPGReplaySafeTerminalClassificationAndOnlineMetricsState(t *testing.T) {
	st := newPGTestStore(t)
	ctx := context.Background()
	id, err := st.CreateSessionForIdentity(
		ctx, "alpha", "pg-replay-safe", "generation-a", 0)
	if err != nil {
		t.Fatal(err)
	}
	p := st.(*pgStore)
	t.Cleanup(func() {
		_, _ = p.db.ExecContext(context.Background(), `DELETE FROM sessions WHERE id=$1`, id)
	})
	changed, err := st.SetInitialFailureCategory(ctx, id, "none")
	if err != nil || !changed {
		t.Fatalf("initial category changed=%v err=%v", changed, err)
	}
	if err := st.SetFailureCategory(ctx, id, "quality_fail"); err != nil {
		t.Fatal(err)
	}
	changed, err = st.SetInitialFailureCategory(ctx, id, "none")
	if err != nil || changed {
		t.Fatalf("replay category changed=%v err=%v", changed, err)
	}
	row, err := st.GetSession(ctx, id)
	if err != nil || row.FailureCategory != "quality_fail" {
		t.Fatalf("replay category=%q err=%v", row.FailureCategory, err)
	}
	refined, err := st.RefineFailureCategory(ctx, id, "quality_fail", "tool_error")
	if err != nil || !refined {
		t.Fatalf("refine category changed=%v err=%v", refined, err)
	}
	refined, err = st.RefineFailureCategory(ctx, id, "quality_fail", "none")
	if err != nil || refined {
		t.Fatalf("replayed/wrong-source refinement changed=%v err=%v", refined, err)
	}
	inserted, authoritative, err := st.PutOnlineResultIfNew(
		ctx, id, "quality", "alpha", "actor", "contains", true, "")
	if err != nil || !inserted || !authoritative {
		t.Fatalf("first result inserted=%v authoritative=%v err=%v", inserted, authoritative, err)
	}
	inserted, authoritative, err = st.PutOnlineResultIfNew(
		ctx, id, "quality", "alpha", "actor", "contains", false, "changed")
	if err != nil || inserted || !authoritative {
		t.Fatalf("replayed result inserted=%v authoritative=%v err=%v", inserted, authoritative, err)
	}
	results, err := st.ListOnlineResults(ctx, id)
	if err != nil || len(results) != 1 || !results[0].Passed || results[0].Detail != "" {
		t.Fatalf("immutable PostgreSQL result=%+v err=%v", results, err)
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

func TestPGSessionRetentionSerializesConcurrentTouchWithoutPartialHistory(t *testing.T) {
	st := newPGTestStore(t)
	ctx := context.Background()
	p := st.(*pgStore)
	sid, err := st.CreateSessionForIdentity(
		ctx, "alpha", "retention-race", "generation-a", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSessionStatus(ctx, sid, "completed"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendEvent(ctx, sid, "done", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := p.db.ExecContext(ctx,
		`UPDATE sessions SET last_active_at=now()-interval '2 hours' WHERE id=$1`,
		sid); err != nil {
		t.Fatal(err)
	}

	const advisoryKey int64 = 7626031401
	if _, err := p.db.ExecContext(ctx, `
		CREATE OR REPLACE FUNCTION runtime_test_pause_reap()
		RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			PERFORM pg_advisory_xact_lock(7626031401);
			RETURN OLD;
		END $$;
		DROP TRIGGER IF EXISTS runtime_test_pause_reap ON session_events;
		CREATE TRIGGER runtime_test_pause_reap
		BEFORE DELETE ON session_events
		FOR EACH ROW EXECUTE FUNCTION runtime_test_pause_reap()`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = p.db.Exec(`
			DROP TRIGGER IF EXISTS runtime_test_pause_reap ON session_events;
			DROP FUNCTION IF EXISTS runtime_test_pause_reap();
			DELETE FROM sessions WHERE id=$1`, sid)
	})

	lockConn, err := p.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lockConn.Close()
	if _, err := lockConn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, advisoryKey); err != nil {
		t.Fatal(err)
	}

	reapDone := make(chan error, 1)
	go func() {
		_, reapErr := st.ReapSessions(
			context.Background(), time.Now().Add(-time.Hour), 1, false)
		reapDone <- reapErr
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		if err := p.db.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity
				 WHERE wait_event='advisory'
				   AND query ILIKE '%DELETE FROM session_events%'
			)`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("retention did not reach the locked child-delete trigger")
		}
		time.Sleep(10 * time.Millisecond)
	}

	touchDone := make(chan error, 1)
	go func() {
		touchDone <- st.TouchSession(context.Background(), sid)
	}()
	select {
	case err := <-touchDone:
		t.Fatalf("touch did not serialize behind retention lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if _, err := lockConn.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, advisoryKey); err != nil {
		t.Fatal(err)
	}
	if err := <-reapDone; err != nil {
		t.Fatal(err)
	}
	if err := <-touchDone; !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("serialized touch after retention = %v, want ErrSessionNotFound", err)
	}
	if _, err := st.GetSession(ctx, sid); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("parent survived retention after child deletion: %v", err)
	}
	var events int
	if err := p.db.QueryRowContext(ctx,
		`SELECT count(*) FROM session_events WHERE session_id=$1`, sid).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 0 {
		t.Fatalf("orphaned session events=%d", events)
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
	if err := CheckSchemaMigrations(ctx, db, component, 1, 2, migrations); err != nil {
		t.Fatalf("complete migration preflight: %v", err)
	}
	var versions int
	if err := db.QueryRow(`SELECT count(*) FROM runtime_schema_migrations WHERE component=$1`, component).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != 2 {
		t.Fatalf("ledger versions=%d, want 2", versions)
	}
	if _, err := db.Exec(`DELETE FROM runtime_schema_migrations WHERE component=$1 AND version=1`, component); err != nil {
		t.Fatal(err)
	}
	if err := CheckSchemaMigrations(ctx, db, component, 1, 2, migrations); err == nil {
		t.Fatal("migration ledger gap accepted")
	}
	if _, err := db.Exec(`
		INSERT INTO runtime_schema_migrations(component,version,name,checksum)
		VALUES ($1,1,$2,$3)`, component, migrations[0].Name, migrationChecksum(migrations[0])); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE runtime_schema_migrations SET checksum='corrupt' WHERE component=$1 AND version=2`, component); err != nil {
		t.Fatal(err)
	}
	if err := CheckSchemaMigrations(ctx, db, component, 1, 2, migrations); err == nil {
		t.Fatal("migration checksum corruption accepted")
	}
	if _, err := db.Exec(`UPDATE runtime_schema_migrations SET checksum=$2 WHERE component=$1 AND version=2`, component, migrationChecksum(migrations[1])); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO runtime_schema_migrations(component,version,name,checksum) VALUES ($1,3,'future','future')`, component); err != nil {
		t.Fatal(err)
	}
	if err := CheckSchemaVersion(ctx, db, component, 1, 2); err == nil {
		t.Fatal("newer unsupported schema accepted")
	}
}

func TestCoreSchemaRestrictedPreflightRejectsMissingSecurityObjects(t *testing.T) {
	ctx := context.Background()
	repair := func(t *testing.T) {
		t.Helper()
		st, err := NewPGStore(ctx, pgTestDSN)
		if err != nil {
			t.Fatalf("repair core schema: %v", err)
		}
		_ = st.Close()
	}
	repair(t)
	db, err := sql.Open("pgx", pgTestDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	t.Cleanup(func() { repair(t) })

	// A preflight that is too strict breaks legitimate agent startup, so the
	// unmutated schema the control plane just migrated must be accepted.
	if err := CheckCoreSchema(ctx, db); err != nil {
		t.Fatalf("preflight rejected a freshly migrated schema: %v", err)
	}

	cases := []struct {
		name   string
		mutate string
	}{
		{"table", `DROP TABLE session_events CASCADE`},
		{"row security", `ALTER TABLE sessions DISABLE ROW LEVEL SECURITY`},
		{"policy", `DROP POLICY runtime_agent_tenant_sessions ON sessions`},
		{"trigger", `DROP TRIGGER runtime_session_transcript_tenant ON session_transcripts`},
		{"foreign key", `ALTER TABLE online_eval_results DROP CONSTRAINT online_eval_results_session_id_fkey`},
		// The cases below all PRESERVE the object name and only weaken its
		// semantics, so a name-counting preflight would accept them.
		{"policy predicate weakened", `ALTER POLICY runtime_agent_tenant_sessions ON sessions USING (true) WITH CHECK (true)`},
		{"policy with-check weakened", `ALTER POLICY runtime_agent_tenant_transcripts ON session_transcripts WITH CHECK (true)`},
		{"policy narrowed to select", `DROP POLICY runtime_agent_tenant_events ON session_events;
			CREATE POLICY runtime_agent_tenant_events ON session_events FOR SELECT
			USING (runtime_agent_can_access_session(session_id))`},
		{"trigger redirected to permissive function", `CREATE OR REPLACE FUNCTION runtime_test_permissive_child()
			RETURNS TRIGGER LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$;
			DROP TRIGGER runtime_online_eval_tenant ON online_eval_results;
			CREATE TRIGGER runtime_online_eval_tenant BEFORE INSERT OR UPDATE OF session_id, tenant
			ON online_eval_results FOR EACH ROW EXECUTE FUNCTION runtime_test_permissive_child()`},
		{"trigger timing changed to after", `DROP TRIGGER runtime_session_transcript_tenant ON session_transcripts;
			CREATE TRIGGER runtime_session_transcript_tenant AFTER INSERT OR UPDATE OF session_id, tenant
			ON session_transcripts FOR EACH ROW EXECUTE FUNCTION runtime_enforce_session_child_tenant()`},
		{"trigger disabled", `ALTER TABLE session_transcripts DISABLE TRIGGER runtime_session_transcript_tenant`},
		{"foreign key cascade weakened", `ALTER TABLE session_events DROP CONSTRAINT session_events_session_id_fkey;
			ALTER TABLE session_events ADD CONSTRAINT session_events_session_id_fkey
			FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE NO ACTION`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repair(t)
			if _, err := db.ExecContext(ctx, tc.mutate); err != nil {
				t.Fatal(err)
			}
			if err := CheckCoreSchema(ctx, db); err == nil {
				t.Fatalf("preflight accepted missing %s", tc.name)
			}
		})
	}
}

func TestSchemaMigrationsRepairBaselineBeforeDependentMigration(t *testing.T) {
	db, err := sql.Open("pgx", pgTestDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	component := fmt.Sprintf("test-partial-restore-%d", time.Now().UnixNano())
	table := fmt.Sprintf("migration_restore_%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM runtime_schema_migrations WHERE component=$1`, component)
		_, _ = db.Exec(`DROP TABLE IF EXISTS ` + table)
	})
	migrations := []Migration{
		{Version: 1, Name: "baseline", SQL: `CREATE TABLE IF NOT EXISTS ` + table + ` (id INT PRIMARY KEY)`},
		{Version: 2, Name: "dependent", SQL: `ALTER TABLE ` + table + ` ADD COLUMN durable TEXT`},
	}
	if err := ApplyMigrationsLocked(ctx, db, component, 1, 1, migrations); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE ` + table); err != nil {
		t.Fatal(err)
	}
	if err := ApplyMigrationsLocked(ctx, db, component, 1, 2, migrations); err != nil {
		t.Fatalf("partial restore was not repaired before dependent migration: %v", err)
	}
	var durableColumn bool
	if err := db.QueryRow(`
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			 WHERE table_schema=current_schema() AND table_name=$1
			   AND column_name='durable'
		)`, table).Scan(&durableColumn); err != nil {
		t.Fatal(err)
	}
	if !durableColumn {
		t.Fatal("repaired table is missing the dependent migration column")
	}
}

func TestSchemaMigrationsDoNotRepairBeforeLedgerValidation(t *testing.T) {
	db, err := sql.Open("pgx", pgTestDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	component := fmt.Sprintf("test-fail-closed-restore-%d", time.Now().UnixNano())
	table := fmt.Sprintf("migration_fail_closed_%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM runtime_schema_migrations WHERE component=$1`, component)
		_, _ = db.Exec(`DROP TABLE IF EXISTS ` + table)
	})
	migrations := []Migration{
		{Version: 1, Name: "baseline", SQL: `CREATE TABLE IF NOT EXISTS ` + table + ` (id INT PRIMARY KEY)`},
		{Version: 2, Name: "dependent", SQL: `ALTER TABLE ` + table + ` ADD COLUMN durable TEXT`},
	}
	if err := ApplyMigrationsLocked(ctx, db, component, 1, 1, migrations); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE ` + table); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		UPDATE runtime_schema_migrations SET checksum='corrupt'
		 WHERE component=$1 AND version=1`, component); err != nil {
		t.Fatal(err)
	}
	if err := ApplyMigrationsLocked(ctx, db, component, 1, 2, migrations); err == nil {
		t.Fatal("corrupt migration ledger was accepted")
	}
	var exists bool
	if err := db.QueryRow(`SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("baseline was repaired before the corrupt ledger failed closed")
	}
}

func TestCoreSchemaRecoversMissingBaselineAtCurrentLedger(t *testing.T) {
	ctx := context.Background()
	st := newPGTestStore(t)
	p := st.(*pgStore)
	for _, table := range []string{
		"online_eval_results", "session_transcripts", "session_events", "sessions",
	} {
		if _, err := p.db.ExecContext(ctx, `DROP TABLE IF EXISTS `+table+` CASCADE`); err != nil {
			t.Fatal(err)
		}
	}
	recovered, err := NewPGStore(ctx, pgTestDSN)
	if err != nil {
		t.Fatalf("recover core schema with current ledger: %v", err)
	}
	defer recovered.Close()
	db := recovered.(*pgStore).db
	for _, table := range []string{
		"sessions", "session_events", "session_transcripts", "online_eval_results",
	} {
		var exists bool
		if err := db.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Errorf("baseline table %s was not recovered", table)
		}
	}
	var generationColumn bool
	if err := db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			 WHERE table_schema=current_schema() AND table_name='sessions'
			   AND column_name='agent_generation'
		)`).Scan(&generationColumn); err != nil {
		t.Fatal(err)
	}
	if !generationColumn {
		t.Fatal("recovered sessions table is missing agent_generation")
	}
	for _, child := range []string{"session_events", "session_transcripts", "online_eval_results"} {
		var validFK bool
		if err := db.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_constraint
				 WHERE conrelid=$1::regclass AND confrelid='sessions'::regclass
				   AND contype='f' AND confdeltype='c'
			)`, child).Scan(&validFK); err != nil {
			t.Fatal(err)
		}
		if !validFK {
			t.Errorf("recovered table %s lacks cascading session foreign key", child)
		}
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
