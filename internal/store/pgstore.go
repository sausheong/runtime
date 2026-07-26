package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

//go:embed schema.sql
var schemaSQL string

//go:embed tenant_rls.sql
var tenantRLSSQL string

//go:embed tenant_rls_session_user.sql
var tenantRLSSessionUserSQL string

//go:embed quarantine_legacy_sessions.sql
var quarantineLegacySessionsSQL string

//go:embed child_tenant_integrity.sql
var childTenantIntegritySQL string

//go:embed test_role_wildcard.sql
var testRoleWildcardSQL string

//go:embed session_agent_generation.sql
var sessionAgentGenerationSQL string

//go:embed agent_role_scope.sql
var agentRoleScopeSQL string

//go:embed referential_integrity.sql
var referentialIntegritySQL string

type pgStore struct{ db *sql.DB }

const (
	coreSchemaComponent = "core"
	coreSchemaVersion   = 9
)

type Migration struct {
	Version int
	Name    string
	SQL     string
}

var coreMigrations = []Migration{
	{Version: 1, Name: "baseline", SQL: schemaSQL},
	{Version: 2, Name: "agent-tenant-row-security", SQL: tenantRLSSQL},
	{Version: 3, Name: "row-security-session-user", SQL: tenantRLSSessionUserSQL},
	{Version: 4, Name: "quarantine-legacy-unowned-sessions", SQL: quarantineLegacySessionsSQL},
	{Version: 5, Name: "enforce-child-tenant-integrity", SQL: childTenantIntegritySQL},
	{Version: 6, Name: "test-role-wildcard-support", SQL: testRoleWildcardSQL},
	{Version: 7, Name: "bind-sessions-to-agent-generation", SQL: sessionAgentGenerationSQL},
	{Version: 8, Name: "scope-agent-roles-to-agent-identity", SQL: agentRoleScopeSQL},
	{Version: 9, Name: "repair-referential-integrity", SQL: referentialIntegritySQL},
}

func (p *pgStore) Ping(ctx context.Context) error { return p.db.PingContext(ctx) }

func NewPGStore(ctx context.Context, dsn string) (Store, error) {
	return newPGStore(ctx, dsn, true)
}

// NewPGStoreExisting opens a store whose schema has already been prepared by
// the control plane. Restricted agent roles use this path so they never need
// ownership of control-plane tables merely to execute idempotent ALTER DDL.
func NewPGStoreExisting(ctx context.Context, dsn string) (Store, error) {
	return newPGStore(ctx, dsn, false)
}

func newPGStore(ctx context.Context, dsn string, applyDDL bool) (Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	if applyDDL {
		if err := ApplyMigrationsLocked(ctx, db, coreSchemaComponent, 1, coreSchemaVersion, coreMigrations); err != nil {
			db.Close()
			return nil, err
		}
		if err := ApplyDDLLocked(ctx, db, schemaSQL+"\n"+tenantRLSSQL+"\n"+
			tenantRLSSessionUserSQL+"\n"+quarantineLegacySessionsSQL+"\n"+
			childTenantIntegritySQL+"\n"+testRoleWildcardSQL+"\n"+
			sessionAgentGenerationSQL+"\n"+agentRoleScopeSQL); err != nil {
			db.Close()
			return nil, err
		}
		if err := ApplyDDLLocked(ctx, db, referentialIntegritySQL); err != nil {
			db.Close()
			return nil, err
		}
	} else if err := CheckCoreSchema(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return &pgStore{db: db}, nil
}

// ConfigureAgentRole binds a restricted login to exactly one tenant/agent trust
// domain for the row-level policies installed by the core schema.
func ConfigureAgentRole(ctx context.Context, db *sql.DB, role, tenant, agentID string, allowRebind bool) error {
	if role == "" || tenant == "" || agentID == "" {
		return errors.New("agent database role, tenant, and agent id are required")
	}
	var currentTenant, currentAgent string
	err := db.QueryRowContext(ctx,
		`SELECT tenant_id, agent_id FROM runtime_agent_tenant_roles WHERE role_name=$1`,
		role).Scan(&currentTenant, &currentAgent)
	if err == nil && (currentTenant != tenant || currentAgent != agentID) && !allowRebind {
		return fmt.Errorf(
			"agent database role %q is already bound to %q/%q, not %q/%q; provision a distinct role",
			role, currentTenant, currentAgent, tenant, agentID)
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read agent database role %q tenant mapping: %w", role, err)
	}
	if _, err := db.ExecContext(ctx, `
			INSERT INTO runtime_agent_tenant_roles (role_name, tenant_id, agent_id)
			VALUES ($1, $2, $3)
			ON CONFLICT (role_name) DO UPDATE
			    SET tenant_id = EXCLUDED.tenant_id,
			        agent_id = EXCLUDED.agent_id`,
		role, tenant, agentID); err != nil {
		return fmt.Errorf("bind agent database role %q to %q/%q: %w", role, tenant, agentID, err)
	}
	return nil
}

// ProvisionAgentRole grants the minimum shared-store privileges required by an
// agent process and binds that login to tenant-scoped session row policies. The
// login itself must be created by database bootstrap with no elevated
// attributes.
func ProvisionAgentRole(ctx context.Context, db *sql.DB, role, tenant, agentID string, allowRebind bool) error {
	for _, r := range role {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return fmt.Errorf("agent database role %q is not a safe SQL identifier", role)
		}
	}
	if role == "" {
		return errors.New("agent database role is required")
	}
	var (
		currentRole     string
		superuser       bool
		createRole      bool
		createDB        bool
		bypassRLS       bool
		inheritsControl bool
		inheritsRole    bool
	)
	err := db.QueryRowContext(ctx, `
		SELECT current_user, r.rolsuper, r.rolcreaterole, r.rolcreatedb, r.rolbypassrls,
		       pg_has_role(r.rolname::text, current_user::text, 'MEMBER'),
		       EXISTS (
		           SELECT 1
		             FROM pg_roles parent
		            WHERE parent.rolname <> r.rolname
		              AND pg_has_role(r.rolname::text, parent.rolname::text, 'MEMBER')
		       )
		  FROM pg_roles r
		 WHERE r.rolname = $1`,
		role).Scan(&currentRole, &superuser, &createRole, &createDB, &bypassRLS,
		&inheritsControl, &inheritsRole)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("agent database role %q does not exist; provision it with the deployment database bootstrap", role)
	}
	if err != nil {
		return fmt.Errorf("check agent database role %q: %w", role, err)
	}
	if role == currentRole || inheritsControl {
		return fmt.Errorf("agent database role %q is, or inherits from, the control-plane role %q", role, currentRole)
	}
	if superuser || createRole || createDB || bypassRLS || inheritsRole {
		return fmt.Errorf("agent database role %q has elevated attributes or inherits another role", role)
	}
	var ownsProtected bool
	if err := db.QueryRowContext(ctx, `
		SELECT EXISTS (
		    SELECT 1
		      FROM pg_class c
		      JOIN pg_namespace n ON n.oid = c.relnamespace
		      JOIN pg_roles r ON r.oid = c.relowner
		     WHERE r.rolname = $1
		       AND n.nspname = 'public'
		       AND (c.relrowsecurity OR c.relname IN (
		           'agents', 'runtime_agent_tenant_roles', 'runtime_schema_migrations',
		           'identity_users', 'service_keys', 'secrets'
		       ))
		)`, role).Scan(&ownsProtected); err != nil {
		return fmt.Errorf("check agent database role %q ownership: %w", role, err)
	}
	if ownsProtected {
		return fmt.Errorf("agent database role %q owns a protected table and could bypass its row policy", role)
	}
	quotedRole := `"` + strings.ReplaceAll(role, `"`, `""`) + `"`
	dbosSchema := AgentDBOSSchema(tenant, agentID)
	if agentID == "*" {
		// Test-only integration binaries deliberately share one disposable role
		// and legacy DBOS schema. Production identity selection never returns
		// the wildcard agent.
		dbosSchema = "dbos"
	}
	quotedDBOSSchema := `"` + strings.ReplaceAll(dbosSchema, `"`, `""`) + `"`
	statements := []string{
		`CREATE SCHEMA IF NOT EXISTS ` + quotedDBOSSchema,
		`REVOKE ALL ON SCHEMA ` + quotedDBOSSchema + ` FROM PUBLIC`,
		`REVOKE CREATE ON SCHEMA public FROM ` + quotedRole,
		`GRANT USAGE ON SCHEMA public TO ` + quotedRole,
		`GRANT USAGE, CREATE ON SCHEMA ` + quotedDBOSSchema + ` TO ` + quotedRole,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE
			sessions, session_events, session_transcripts, online_eval_results TO ` + quotedRole,
		`REVOKE ALL PRIVILEGES ON TABLE agents FROM ` + quotedRole,
		`GRANT SELECT ON TABLE runtime_schema_migrations TO ` + quotedRole,
		`GRANT EXECUTE ON FUNCTION runtime_agent_tenant() TO ` + quotedRole,
		`GRANT EXECUTE ON FUNCTION runtime_agent_id() TO ` + quotedRole,
		`GRANT EXECUTE ON FUNCTION runtime_agent_can_access_session(TEXT) TO ` + quotedRole,
	}
	for _, statement := range statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("grant agent role %q: %w", role, err)
		}
	}
	for _, table := range []string{"identity_users", "service_keys", "secrets"} {
		var exists bool
		if err := db.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists); err != nil {
			return fmt.Errorf("check sensitive table %q: %w", table, err)
		}
		if exists {
			if _, err := db.ExecContext(ctx, `REVOKE ALL PRIVILEGES ON TABLE "`+table+`" FROM `+quotedRole); err != nil {
				return fmt.Errorf("revoke agent role %q access to %q: %w", role, table, err)
			}
		}
	}
	// The built-in testagent uses a harness-owned marker table. Production
	// agents must provision their own tool tables explicitly; agents never get
	// general CREATE authority on public.
	var markerExists bool
	if err := db.QueryRowContext(ctx, `SELECT to_regclass('public.markers') IS NOT NULL`).Scan(&markerExists); err != nil {
		return err
	}
	if markerExists {
		if _, err := db.ExecContext(ctx,
			`GRANT SELECT, INSERT ON TABLE markers TO `+quotedRole+`;
			 GRANT USAGE, SELECT ON SEQUENCE markers_id_seq TO `+quotedRole); err != nil {
			return fmt.Errorf("grant test marker access to agent role %q: %w", role, err)
		}
	}
	return ConfigureAgentRole(ctx, db, role, tenant, agentID, allowRebind)
}

// ApplySchemaMigrations records a component's immutable baseline migration.
// Future schema changes append Migration entries through ApplyMigrationsLocked;
// existing entries must never be edited because their checksums are verified.
func ApplySchemaMigrations(ctx context.Context, db *sql.DB, component string, current int, baseline string) error {
	if err := ApplyMigrationsLocked(ctx, db, component, 1, current, []Migration{
		{Version: 1, Name: "baseline", SQL: baseline},
	}); err != nil {
		return err
	}
	// Single-version component baselines are idempotent and act as their own
	// current-schema reconciler after the ledger has been validated.
	return ApplyDDLLocked(ctx, db, baseline)
}

// ApplyMigrationsLocked applies ordered component migrations and advances the
// ledger in the same transaction. Any failure rolls back both DDL and ledger.
func ApplyMigrationsLocked(ctx context.Context, db *sql.DB, component string, minSupported, current int, migrations []Migration) error {
	if component == "" || minSupported < 1 || current < minSupported {
		return fmt.Errorf("invalid schema range for component %q", component)
	}
	for i, migration := range migrations {
		if migration.Version != i+1 || migration.Name == "" || migration.SQL == "" {
			return fmt.Errorf("component %q migrations must be contiguous from version 1", component)
		}
	}
	if len(migrations) < current {
		return fmt.Errorf("component %q current version %d has no migration", component, current)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration tx: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, schemaLockKey); err != nil {
		return fmt.Errorf("acquire schema lock: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS runtime_schema_migrations (
			component  TEXT NOT NULL,
			version    INT NOT NULL,
			name       TEXT NOT NULL,
			checksum   TEXT NOT NULL,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			PRIMARY KEY (component, version)
		)`); err != nil {
		return fmt.Errorf("create migration ledger: %w", err)
	}

	rows, err := tx.QueryContext(ctx,
		`SELECT version, checksum FROM public.runtime_schema_migrations WHERE component=$1 ORDER BY version`,
		component)
	if err != nil {
		return err
	}
	applied := 0
	for rows.Next() {
		var version int
		var checksum string
		if err := rows.Scan(&version, &checksum); err != nil {
			rows.Close()
			return err
		}
		if version != applied+1 || version > len(migrations) {
			rows.Close()
			return fmt.Errorf("component %q has unsupported migration version %d", component, version)
		}
		want := migrationChecksum(migrations[version-1])
		if checksum != want {
			rows.Close()
			return fmt.Errorf("component %q migration %d checksum mismatch", component, version)
		}
		applied = version
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if applied > current {
		return fmt.Errorf("component %q schema version %d is newer than supported %d", component, applied, current)
	}
	if applied > 0 && applied < minSupported {
		return fmt.Errorf("component %q schema version %d is older than supported %d", component, applied, minSupported)
	}
	// A partial restore can retain the ledger but lose a baseline relation.
	// Reconcile the immutable, idempotent baseline inside this same locked
	// transaction after validating checksums and compatibility, and before any
	// dependent migration runs. On a fresh database migration 1 below performs
	// the initial baseline apply.
	if applied > 0 && applied < current {
		if _, err := tx.ExecContext(ctx, migrations[0].SQL); err != nil {
			return fmt.Errorf("reconcile %s baseline: %w", component, err)
		}
	}
	for _, migration := range migrations[applied:current] {
		if _, err := tx.ExecContext(ctx, migration.SQL); err != nil {
			return fmt.Errorf("apply %s migration %d (%s): %w", component, migration.Version, migration.Name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO public.runtime_schema_migrations (component, version, name, checksum) VALUES ($1,$2,$3,$4)`,
			component, migration.Version, migration.Name, migrationChecksum(migration)); err != nil {
			return fmt.Errorf("record %s migration %d: %w", component, migration.Version, err)
		}
	}
	return tx.Commit()
}

func migrationChecksum(m Migration) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s\x00%s", m.Version, m.Name, m.SQL))))
}

// CheckSchemaVersion is the restricted-binary preflight: it never applies DDL.
func CheckSchemaVersion(ctx context.Context, db *sql.DB, component string, minSupported, maxSupported int) error {
	var version int
	err := db.QueryRowContext(ctx,
		`SELECT COALESCE(max(version),0) FROM public.runtime_schema_migrations WHERE component=$1`,
		component).Scan(&version)
	if err != nil {
		return fmt.Errorf("check %s schema version: %w", component, err)
	}
	if version < minSupported || version > maxSupported {
		return fmt.Errorf("component %q schema version %d outside supported range %d..%d",
			component, version, minSupported, maxSupported)
	}
	return nil
}

// CheckSchemaMigrations validates the complete immutable ledger without
// applying DDL. It is suitable for restricted binaries that can read the
// migration table but must never own or repair schema objects.
func CheckSchemaMigrations(ctx context.Context, db *sql.DB, component string, minSupported, maxSupported int, expected []Migration) error {
	if component == "" || minSupported < 1 || maxSupported < minSupported || len(expected) < maxSupported {
		return fmt.Errorf("invalid schema preflight for component %q", component)
	}
	rows, err := db.QueryContext(ctx, `
		SELECT version, name, checksum
		  FROM public.runtime_schema_migrations
		 WHERE component=$1
		 ORDER BY version`, component)
	if err != nil {
		return fmt.Errorf("check %s migration ledger: %w", component, err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var version int
		var name, checksum string
		if err := rows.Scan(&version, &name, &checksum); err != nil {
			return fmt.Errorf("read %s migration ledger: %w", component, err)
		}
		if version != seen+1 || version > maxSupported {
			return fmt.Errorf("component %q migration ledger is not contiguous at version %d", component, version)
		}
		want := expected[version-1]
		if name != want.Name || checksum != migrationChecksum(want) {
			return fmt.Errorf("component %q migration %d name or checksum mismatch", component, version)
		}
		seen = version
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read %s migration ledger: %w", component, err)
	}
	if seen < minSupported || seen > maxSupported {
		return fmt.Errorf("component %q schema version %d outside supported range %d..%d",
			component, seen, minSupported, maxSupported)
	}
	return nil
}

// CheckCoreSchema validates the immutable core ledger and the structural
// objects a restricted agent relies on. Every query is read-only.
func CheckCoreSchema(ctx context.Context, db *sql.DB) error {
	if err := CheckSchemaMigrations(ctx, db, coreSchemaComponent,
		coreSchemaVersion, coreSchemaVersion, coreMigrations); err != nil {
		return err
	}
	var tableCount, rlsCount int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*), count(*) FILTER (WHERE c.relrowsecurity)
		  FROM pg_catalog.pg_class c
		  JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
		 WHERE n.nspname='public'
		   AND c.relkind IN ('r','p')
		   AND c.relname IN ('sessions','session_events','session_transcripts','online_eval_results')`,
	).Scan(&tableCount, &rlsCount); err != nil {
		return fmt.Errorf("check core tables and row security: %w", err)
	}
	if tableCount != 4 || rlsCount != 4 {
		return fmt.Errorf("core schema integrity: required tables=%d/4 row-security=%d/4", tableCount, rlsCount)
	}
	if err := checkCorePolicies(ctx, db); err != nil {
		return err
	}
	if err := checkCoreTriggers(ctx, db); err != nil {
		return err
	}
	return checkCoreForeignKeys(ctx, db)
}

// expectedPolicy is the authoritative definition of one core row-security
// policy. Predicates are the normalized text PostgreSQL renders from
// pg_get_expr, not the source DDL: the server adds parentheses and ::text
// casts, so these strings are what a correct schema actually reports. All four
// policies use the same expression for USING and WITH CHECK.
type expectedPolicy struct {
	table, name, qual string
}

var corePolicies = []expectedPolicy{
	{"sessions", "runtime_agent_tenant_sessions",
		`(((runtime_agent_tenant() = '*'::text) OR (tenant_id = runtime_agent_tenant())) AND ((runtime_agent_id() = '*'::text) OR (agent_id = runtime_agent_id())))`},
	{"session_events", "runtime_agent_tenant_events",
		`runtime_agent_can_access_session(session_id)`},
	{"session_transcripts", "runtime_agent_tenant_transcripts",
		`(((runtime_agent_tenant() = '*'::text) OR (tenant = runtime_agent_tenant())) AND runtime_agent_can_access_session(session_id))`},
	{"online_eval_results", "runtime_agent_tenant_online_results",
		`(((runtime_agent_tenant() = '*'::text) OR (tenant = runtime_agent_tenant())) AND runtime_agent_can_access_session(session_id))`},
}

// checkCorePolicies verifies each policy's semantics, not just its name: a
// policy rewritten to USING (true), narrowed to FOR SELECT, or turned
// restrictive under the same name must be rejected.
func checkCorePolicies(ctx context.Context, db *sql.DB) error {
	for _, want := range corePolicies {
		var (
			cmd        string
			permissive bool
			qual       string
			withCheck  string
		)
		err := db.QueryRowContext(ctx, `
			SELECT p.polcmd, p.polpermissive,
			       COALESCE(pg_catalog.pg_get_expr(p.polqual, p.polrelid), ''),
			       COALESCE(pg_catalog.pg_get_expr(p.polwithcheck, p.polrelid), '')
			  FROM pg_catalog.pg_policy p
			  JOIN pg_catalog.pg_class c ON c.oid=p.polrelid
			  JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
			 WHERE n.nspname='public' AND c.relname=$1 AND p.polname=$2`,
			want.table, want.name).Scan(&cmd, &permissive, &qual, &withCheck)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("core schema integrity: row-security policy %q on %q is missing",
				want.name, want.table)
		}
		if err != nil {
			return fmt.Errorf("check core row-security policy %q on %q: %w", want.name, want.table, err)
		}
		if cmd != "*" {
			return fmt.Errorf("core schema integrity: row-security policy %q on %q applies to command %q, want ALL",
				want.name, want.table, cmd)
		}
		if !permissive {
			return fmt.Errorf("core schema integrity: row-security policy %q on %q is not permissive",
				want.name, want.table)
		}
		if qual != want.qual {
			return fmt.Errorf("core schema integrity: row-security policy %q on %q has an unexpected USING predicate",
				want.name, want.table)
		}
		if withCheck != want.qual {
			return fmt.Errorf("core schema integrity: row-security policy %q on %q has an unexpected WITH CHECK predicate",
				want.name, want.table)
		}
	}
	return nil
}

// coreTriggerType is BEFORE|ROW|INSERT|UPDATE as pg_trigger.tgtype encodes it.
// Compared as an integer so a redirected timing or event set is rejected.
const coreTriggerType = 23

// coreTriggerFunction is the fully-qualified tenant-integrity trigger function.
const coreTriggerFunction = "public.runtime_enforce_session_child_tenant"

var coreTriggers = []struct{ table, name string }{
	{"session_transcripts", "runtime_session_transcript_tenant"},
	{"online_eval_results", "runtime_online_eval_tenant"},
}

// checkCoreTriggers verifies each tenant-integrity trigger still fires BEFORE
// each affected row and still executes the control-plane enforcement function,
// so a trigger redirected to a permissive stand-in is rejected.
func checkCoreTriggers(ctx context.Context, db *sql.DB) error {
	for _, want := range coreTriggers {
		var (
			tgtype   int
			enabled  string
			function string
		)
		err := db.QueryRowContext(ctx, `
			SELECT t.tgtype, t.tgenabled, np.nspname || '.' || p.proname
			  FROM pg_catalog.pg_trigger t
			  JOIN pg_catalog.pg_class c ON c.oid=t.tgrelid
			  JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
			  JOIN pg_catalog.pg_proc p ON p.oid=t.tgfoid
			  JOIN pg_catalog.pg_namespace np ON np.oid=p.pronamespace
			 WHERE n.nspname='public' AND NOT t.tgisinternal
			   AND c.relname=$1 AND t.tgname=$2`,
			want.table, want.name).Scan(&tgtype, &enabled, &function)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("core schema integrity: tenant-integrity trigger %q on %q is missing",
				want.name, want.table)
		}
		if err != nil {
			return fmt.Errorf("check core tenant-integrity trigger %q on %q: %w", want.name, want.table, err)
		}
		if tgtype != coreTriggerType {
			return fmt.Errorf("core schema integrity: tenant-integrity trigger %q on %q has timing/event mask %d, want %d",
				want.name, want.table, tgtype, coreTriggerType)
		}
		if enabled != "O" {
			return fmt.Errorf("core schema integrity: tenant-integrity trigger %q on %q is not enabled (tgenabled=%q)",
				want.name, want.table, enabled)
		}
		if function != coreTriggerFunction {
			return fmt.Errorf("core schema integrity: tenant-integrity trigger %q on %q executes %q, want %q",
				want.name, want.table, function, coreTriggerFunction)
		}
	}
	return nil
}

var coreForeignKeys = []struct{ table, name string }{
	{"session_events", "session_events_session_id_fkey"},
	{"session_transcripts", "session_transcripts_session_id_fkey"},
	{"online_eval_results", "online_eval_results_session_id_fkey"},
}

// checkCoreForeignKeys verifies each session child key still cascades deletes
// from exactly sessions(id), so a same-named constraint repointed at another
// column or downgraded to NO ACTION is rejected. The column arrays are
// flattened with array_to_string so they scan into a plain string over the pgx
// stdlib driver without an array codec.
func checkCoreForeignKeys(ctx context.Context, db *sql.DB) error {
	for _, want := range coreForeignKeys {
		var (
			deleteAction string
			validated    bool
			childColumns string
			parentCols   string
		)
		err := db.QueryRowContext(ctx, `
			SELECT fk.confdeltype, fk.convalidated,
			       array_to_string(ARRAY(
			           SELECT a.attname FROM unnest(fk.conkey) k
			             JOIN pg_catalog.pg_attribute a
			               ON a.attrelid=fk.conrelid AND a.attnum=k), ','),
			       array_to_string(ARRAY(
			           SELECT a.attname FROM unnest(fk.confkey) k
			             JOIN pg_catalog.pg_attribute a
			               ON a.attrelid=fk.confrelid AND a.attnum=k), ',')
			  FROM pg_catalog.pg_constraint fk
			  JOIN pg_catalog.pg_class child ON child.oid=fk.conrelid
			  JOIN pg_catalog.pg_class parent ON parent.oid=fk.confrelid
			  JOIN pg_catalog.pg_namespace n ON n.oid=child.relnamespace
			  JOIN pg_catalog.pg_namespace pn ON pn.oid=parent.relnamespace
			 WHERE n.nspname='public' AND pn.nspname='public'
			   AND fk.contype='f' AND fk.conname=$1
			   AND child.relname=$2 AND parent.relname='sessions'`,
			want.name, want.table).Scan(&deleteAction, &validated, &childColumns, &parentCols)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("core schema integrity: session foreign key %q on %q is missing",
				want.name, want.table)
		}
		if err != nil {
			return fmt.Errorf("check core session foreign key %q on %q: %w", want.name, want.table, err)
		}
		if deleteAction != "c" {
			return fmt.Errorf("core schema integrity: session foreign key %q on %q does not cascade deletes (confdeltype=%q)",
				want.name, want.table, deleteAction)
		}
		if !validated {
			return fmt.Errorf("core schema integrity: session foreign key %q on %q is not validated",
				want.name, want.table)
		}
		if childColumns != "session_id" || parentCols != "id" {
			return fmt.Errorf("core schema integrity: session foreign key %q on %q maps (%s) to sessions(%s), want (session_id) to sessions(id)",
				want.name, want.table, childColumns, parentCols)
		}
	}
	return nil
}

// schemaLockKey is the shared advisory-lock key all runtime processes use to
// serialize DDL. Arbitrary constant ("runtime" packed into an int8).
const schemaLockKey = 0x72756e74696d65

// ApplyDDLLocked runs the given DDL while holding a transaction-scoped Postgres
// advisory lock, so concurrently-starting processes apply DDL one at a time.
//
// `CREATE TABLE IF NOT EXISTS` is NOT atomic against a concurrent creator in
// Postgres — two processes racing can raise a duplicate pg_class/pg_type error
// (SQLSTATE 23505/42P07). A transaction-scoped lock (pg_advisory_xact_lock)
// binds to the single connection the tx holds (database/sql pools connections,
// so a session-scoped lock could unlock on a different connection) and
// auto-releases on commit/rollback. All callers share schemaLockKey, so the
// store schema and any caller-owned tables (e.g. agentd's marker table)
// serialize against each other on cold start.
func ApplyDDLLocked(ctx context.Context, db *sql.DB, ddl string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin ddl tx: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, schemaLockKey); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("acquire schema lock: %w", err)
	}
	if _, err := tx.ExecContext(ctx, ddl); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("apply ddl: %w", err)
	}
	if err := tx.Commit(); err != nil { // releases the advisory lock
		return fmt.Errorf("commit ddl tx: %w", err)
	}
	return nil
}

func (p *pgStore) CreateSession(ctx context.Context, agentID string, replica int) (string, error) {
	return p.CreateSessionForTenant(ctx, "default", agentID, replica)
}

func (p *pgStore) CreateSessionForTenant(ctx context.Context, tenantID, agentID string, replica int) (string, error) {
	return p.CreateSessionForIdentity(ctx, tenantID, agentID, "", replica)
}

func (p *pgStore) CreateSessionForIdentity(ctx context.Context, tenantID, agentID, agentGeneration string, replica int) (string, error) {
	id := "ses-" + uuid.NewString()
	_, err := p.db.ExecContext(ctx,
		`INSERT INTO sessions
		    (id, tenant_id, agent_id, agent_generation, workflow_id, status, replica)
		 VALUES ($1,$2,$3,$4,$1,'created',$5)`,
		id, tenantID, agentID, agentGeneration, replica)
	return id, err
}

func (p *pgStore) BindSession(ctx context.Context, id, tenantID, agentID, agentGeneration string, replica int) error {
	res, err := p.db.ExecContext(ctx,
		`INSERT INTO sessions
		    (id, tenant_id, agent_id, agent_generation, workflow_id, status, replica)
		 VALUES ($1,$2,$3,$4,$1,'external',$5)
		 ON CONFLICT (id) DO NOTHING`,
		id, tenantID, agentID, agentGeneration, replica)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		existing, getErr := p.GetSession(ctx, id)
		if getErr == nil && existing.TenantID == tenantID &&
			existing.AgentID == agentID &&
			existing.AgentGeneration == agentGeneration &&
			existing.Replica == replica {
			return nil
		}
		return fmt.Errorf("bind session %q: conflicts with existing owner", id)
	}
	return nil
}

func (p *pgStore) SessionReplica(ctx context.Context, id string) (int, error) {
	var r int
	err := p.db.QueryRowContext(ctx, `SELECT replica FROM sessions WHERE id=$1`, id).Scan(&r)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("%w: %q", ErrSessionNotFound, id)
	}
	return r, err
}

func (p *pgStore) ActiveSessionsByReplica(ctx context.Context, tenantID, agentID string) (map[int]int, error) {
	rows, err := p.db.QueryContext(ctx,
		`SELECT replica, count(*) FROM sessions
		 WHERE tenant_id=$1 AND agent_id=$2
		   AND status NOT IN ('external','completed','error','limit_exceeded')
		 GROUP BY replica`, tenantID, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]int{}
	for rows.Next() {
		var replica, n int
		if err := rows.Scan(&replica, &n); err != nil {
			return nil, err
		}
		out[replica] = n
	}
	return out, rows.Err()
}

func (p *pgStore) ListSessions(ctx context.Context, agentID string) ([]SessionRow, error) {
	return p.ListSessionsForTenant(ctx, "default", agentID)
}

func (p *pgStore) ListSessionsForTenant(ctx context.Context, tenantID, agentID string) ([]SessionRow, error) {
	rows, err := p.db.QueryContext(ctx,
		`SELECT id, tenant_id, agent_id, agent_generation, workflow_id, status, turn_count, replica,
		        tokens_total, cost_usd, failure_category, created_at, last_active_at
		   FROM sessions WHERE tenant_id=$1 AND agent_id=$2 ORDER BY created_at DESC`,
		tenantID, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionRow
	for rows.Next() {
		var s SessionRow
		if err := rows.Scan(&s.ID, &s.TenantID, &s.AgentID, &s.AgentGeneration,
			&s.WorkflowID, &s.Status,
			&s.TurnCount, &s.Replica, &s.TokensTotal, &s.CostUSD, &s.FailureCategory,
			&s.CreatedAt, &s.LastActiveAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (p *pgStore) SetTurnCount(ctx context.Context, id string, n int) error {
	_, err := p.db.ExecContext(ctx,
		`UPDATE sessions SET turn_count = $2, last_active_at = now() WHERE id=$1`, id, n)
	return err
}

func (p *pgStore) SetSessionUsage(ctx context.Context, id string, tokens int64, cost float64) error {
	_, err := p.db.ExecContext(ctx,
		`UPDATE sessions SET tokens_total = $2, cost_usd = $3, last_active_at = now() WHERE id=$1`,
		id, tokens, cost)
	return err
}

func (p *pgStore) SetFailureCategory(ctx context.Context, id, category string) error {
	_, err := p.db.ExecContext(ctx,
		`UPDATE sessions SET failure_category = $2, last_active_at = now() WHERE id=$1`,
		id, category)
	return err
}

func (p *pgStore) SetInitialFailureCategory(ctx context.Context, id, category string) (bool, error) {
	res, err := p.db.ExecContext(ctx,
		`UPDATE sessions
		    SET failure_category = $2, last_active_at = now()
		  WHERE id=$1 AND failure_category=''`,
		id, category)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 1 {
		return true, nil
	}
	var exists bool
	if err := p.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM sessions WHERE id=$1)`, id).Scan(&exists); err != nil {
		return false, err
	}
	if !exists {
		return false, fmt.Errorf("%w: %q", ErrSessionNotFound, id)
	}
	return false, nil
}

func (p *pgStore) RefineFailureCategory(ctx context.Context, id, from, to string) (bool, error) {
	res, err := p.db.ExecContext(ctx,
		`UPDATE sessions
		    SET failure_category=$3, last_active_at=now()
		  WHERE id=$1 AND failure_category=$2`,
		id, from, to)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 1 {
		return true, nil
	}
	var exists bool
	if err := p.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM sessions WHERE id=$1)`, id).Scan(&exists); err != nil {
		return false, err
	}
	if !exists {
		return false, fmt.Errorf("%w: %q", ErrSessionNotFound, id)
	}
	return false, nil
}

func (p *pgStore) FailureBreakdownByAgent(ctx context.Context, tenantID, agentID string, since time.Time) (map[string]int, error) {
	q := `SELECT failure_category, count(*) FROM sessions
	       WHERE tenant_id=$1 AND agent_id=$2 AND failure_category <> ''`
	args := []any{tenantID, agentID}
	if !since.IsZero() {
		q += ` AND created_at >= $3`
		args = append(args, since)
	}
	q += ` GROUP BY failure_category`
	rows, err := p.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var (
			cat string
			n   int
		)
		if err := rows.Scan(&cat, &n); err != nil {
			return nil, err
		}
		out[cat] = n
	}
	return out, rows.Err()
}

func (p *pgStore) GetSession(ctx context.Context, id string) (SessionRow, error) {
	var s SessionRow
	err := p.db.QueryRowContext(ctx,
		`SELECT id, tenant_id, agent_id, agent_generation, workflow_id, status, turn_count, replica,
		        tokens_total, cost_usd, failure_category, created_at, last_active_at
		   FROM sessions WHERE id=$1`, id).
		Scan(&s.ID, &s.TenantID, &s.AgentID, &s.AgentGeneration,
			&s.WorkflowID, &s.Status, &s.TurnCount,
			&s.Replica, &s.TokensTotal, &s.CostUSD, &s.FailureCategory,
			&s.CreatedAt, &s.LastActiveAt)
	if errors.Is(err, sql.ErrNoRows) {
		return SessionRow{}, fmt.Errorf("%w: %q", ErrSessionNotFound, id)
	}
	return s, err
}

func (p *pgStore) TouchSession(ctx context.Context, id string) error {
	res, err := p.db.ExecContext(ctx, `UPDATE sessions SET last_active_at=now() WHERE id=$1`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: %q", ErrSessionNotFound, id)
	}
	return nil
}

func (p *pgStore) SetSessionStatus(ctx context.Context, id, status string) error {
	_, err := p.db.ExecContext(ctx,
		`UPDATE sessions SET status=$2, last_active_at=now() WHERE id=$1`, id, status)
	return err
}

func (p *pgStore) AppendEvent(ctx context.Context, sessionID, typ string, payload []byte) (int64, error) {
	return p.appendEvent(ctx, sessionID, "", typ, payload)
}

func (p *pgStore) AppendEventOnce(ctx context.Context, sessionID, eventKey, typ string, payload []byte) (int64, error) {
	if eventKey == "" {
		return 0, fmt.Errorf("event key is required")
	}
	return p.appendEvent(ctx, sessionID, eventKey, typ, payload)
}

func (p *pgStore) appendEvent(ctx context.Context, sessionID, eventKey, typ string, payload []byte) (int64, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	// Serialize sequence allocation per session. This also makes the
	// deterministic-key check and append atomic if a recovered workflow races
	// a still-finishing attempt.
	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, sessionID); err != nil {
		return 0, err
	}
	if eventKey != "" {
		var existing int64
		err := tx.QueryRowContext(ctx,
			`SELECT seq FROM session_events WHERE session_id=$1 AND event_key=$2`,
			sessionID, eventKey).Scan(&existing)
		if err == nil {
			return existing, tx.Commit()
		}
		if err != sql.ErrNoRows {
			return 0, err
		}
	}
	var next int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq),0)+1 FROM session_events WHERE session_id=$1`, sessionID).Scan(&next); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO session_events (session_id, seq, event_key, type, payload) VALUES ($1,$2,$3,$4,$5)`,
		sessionID, next, nullableEventKey(eventKey), typ, payload); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return next, nil
}

func nullableEventKey(key string) any {
	if key == "" {
		return nil
	}
	return key
}

func (p *pgStore) EventsSince(ctx context.Context, sessionID string, afterSeq int64) ([]Event, error) {
	rows, err := p.db.QueryContext(ctx,
		`SELECT seq, type, payload FROM session_events WHERE session_id=$1 AND seq>$2 ORDER BY seq`,
		sessionID, afterSeq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.Seq, &e.Type, &e.Payload); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (p *pgStore) AppendTranscript(ctx context.Context, sessionID string, turn int, tenant, actor string, entries []byte, stopReason, status string) error {
	res, err := p.db.ExecContext(ctx,
		`INSERT INTO session_transcripts (session_id, turn_index, tenant, actor_id, entries, stop_reason, status)
		 SELECT s.id,$2,s.tenant_id,$3,$4::jsonb,$5,$6
		   FROM sessions s
		  WHERE s.id=$1
		 ON CONFLICT (session_id, turn_index) DO UPDATE SET
		   entries=EXCLUDED.entries, tenant=EXCLUDED.tenant, actor_id=EXCLUDED.actor_id,
		   stop_reason=EXCLUDED.stop_reason, status=EXCLUDED.status`,
		sessionID, turn, actor, string(entries), stopReason, status)
	if err != nil {
		return fmt.Errorf("append transcript (%s turn %d): %w", sessionID, turn, err)
	}
	if n, rowsErr := res.RowsAffected(); rowsErr != nil {
		return rowsErr
	} else if n == 0 {
		return fmt.Errorf("append transcript (%s turn %d): session missing", sessionID, turn)
	}
	return nil
}

func (p *pgStore) PutOnlineResult(ctx context.Context, sessionID, criterion, tenant, actor, scorer string, passed bool, detail string) error {
	_, _, err := p.PutOnlineResultIfNew(
		ctx, sessionID, criterion, tenant, actor, scorer, passed, detail)
	return err
}

func (p *pgStore) PutOnlineResultIfNew(ctx context.Context, sessionID, criterion, tenant, actor, scorer string, passed bool, detail string) (bool, bool, error) {
	var authoritative bool
	err := p.db.QueryRowContext(ctx,
		`INSERT INTO online_eval_results (session_id, criterion_name, tenant, actor_id, scorer, passed, detail)
		 SELECT s.id,$2,s.tenant_id,$3,$4,$5,$6
		   FROM sessions s
		  WHERE s.id=$1
		 ON CONFLICT (session_id, criterion_name) DO NOTHING
		 RETURNING passed`,
		sessionID, criterion, actor, scorer, passed, detail).Scan(&authoritative)
	if err == nil {
		return true, authoritative, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, false, fmt.Errorf("put online result (%s %s): %w", sessionID, criterion, err)
	}
	err = p.db.QueryRowContext(ctx,
		`SELECT passed FROM online_eval_results
		  WHERE session_id=$1 AND criterion_name=$2`,
		sessionID, criterion).Scan(&authoritative)
	if err == nil {
		return false, authoritative, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, fmt.Errorf("put online result (%s %s): session missing", sessionID, criterion)
	}
	return false, false, fmt.Errorf("read authoritative online result (%s %s): %w", sessionID, criterion, err)
}

func (p *pgStore) ListOnlineResults(ctx context.Context, sessionID string) ([]OnlineResult, error) {
	rows, err := p.db.QueryContext(ctx,
		`SELECT session_id, criterion_name, tenant, actor_id, scorer, passed, detail, created_at
		 FROM online_eval_results WHERE session_id=$1 ORDER BY criterion_name`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list online results (%s): %w", sessionID, err)
	}
	defer rows.Close()
	return scanOnlineResults(rows)
}

func (p *pgStore) ListOnlineResultsByTenant(ctx context.Context, tenant string, limit int) ([]OnlineResult, error) {
	rows, err := p.db.QueryContext(ctx,
		`SELECT session_id, criterion_name, tenant, actor_id, scorer, passed, detail, created_at
		 FROM online_eval_results WHERE tenant=$1 ORDER BY created_at DESC LIMIT $2`, tenant, limit)
	if err != nil {
		return nil, fmt.Errorf("list online results by tenant (%s): %w", tenant, err)
	}
	defer rows.Close()
	return scanOnlineResults(rows)
}

func (p *pgStore) ReapEvaluationData(ctx context.Context, before time.Time, batch int) (int64, error) {
	if batch < 1 {
		batch = 1
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var total int64
	for _, query := range []string{
		`DELETE FROM online_eval_results WHERE (session_id, criterion_name) IN (
		    SELECT session_id, criterion_name FROM online_eval_results
		     WHERE created_at < $1 ORDER BY created_at, session_id, criterion_name LIMIT $2
		)`,
		`DELETE FROM session_transcripts WHERE (session_id, turn_index) IN (
		    SELECT session_id, turn_index FROM session_transcripts
		     WHERE created_at < $1 ORDER BY created_at, session_id, turn_index LIMIT $2
		)`,
	} {
		res, execErr := tx.ExecContext(ctx, query, before, batch)
		if execErr != nil {
			return 0, execErr
		}
		n, rowsErr := res.RowsAffected()
		if rowsErr != nil {
			return 0, rowsErr
		}
		total += n
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return total, nil
}

func (p *pgStore) ReapSessions(ctx context.Context, before time.Time, batch int, dryRun bool) (int64, error) {
	if batch < 1 {
		batch = 1
	}
	const candidates = `
		SELECT id FROM sessions
		 WHERE last_active_at < $1
		   AND status IN ('external','completed','error','limit_exceeded')
		 ORDER BY last_active_at, id
		 LIMIT $2`
	if dryRun {
		var n int64
		err := p.db.QueryRowContext(ctx, `SELECT count(*) FROM (`+candidates+`) AS candidates`, before, batch).Scan(&n)
		return n, err
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	// Freeze one exact candidate set and lock every parent row before touching
	// any child. Concurrent activity updates and child inserts then serialize
	// behind this transaction instead of causing each DELETE statement to
	// re-evaluate a different READ COMMITTED snapshot.
	if _, err := tx.ExecContext(ctx, `
		CREATE TEMP TABLE runtime_session_reap_candidates (
			id TEXT PRIMARY KEY
		) ON COMMIT DROP`); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO runtime_session_reap_candidates (id)
		SELECT id FROM sessions
		 WHERE last_active_at < $1
		   AND status IN ('external','completed','error','limit_exceeded')
		 ORDER BY last_active_at, id
		 FOR UPDATE SKIP LOCKED
		 LIMIT $2`, before, batch); err != nil {
		return 0, err
	}
	// Child deletes are explicit as well as cascade-backed so upgrades remain
	// safe if a legacy database has not yet replaced its foreign key. Every
	// statement consumes the same locked temp-table set.
	for _, query := range []string{
		`DELETE FROM online_eval_results
		  WHERE session_id IN (SELECT id FROM runtime_session_reap_candidates)`,
		`DELETE FROM session_transcripts
		  WHERE session_id IN (SELECT id FROM runtime_session_reap_candidates)`,
		`DELETE FROM session_events
		  WHERE session_id IN (SELECT id FROM runtime_session_reap_candidates)`,
	} {
		if _, err := tx.ExecContext(ctx, query); err != nil {
			return 0, err
		}
	}
	res, err := tx.ExecContext(ctx, `
		DELETE FROM sessions
		 WHERE id IN (SELECT id FROM runtime_session_reap_candidates)`)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

func scanOnlineResults(rows *sql.Rows) ([]OnlineResult, error) {
	var out []OnlineResult
	for rows.Next() {
		var r OnlineResult
		if err := rows.Scan(&r.SessionID, &r.Criterion, &r.Tenant, &r.Actor, &r.Scorer, &r.Passed, &r.Detail, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (p *pgStore) Close() error { return p.db.Close() }
