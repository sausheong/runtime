package identity

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"

	"github.com/sausheong/runtime/internal/store"
)

//go:embed schema.sql
var schemaSQL string

//go:embed registration_token_binding.sql
var registrationTokenBindingSQL string

//go:embed referential_integrity.sql
var referentialIntegritySQL string

// ErrNoUser / ErrNoKey signal a missing row during authentication resolution.
var (
	ErrNoUser     = errors.New("identity: no such user")
	ErrNoKey      = errors.New("identity: no such service key")
	ErrNoRegToken = errors.New("identity: no such registration token")
)

// TenantRow / UserRow / KeyRow are read models for listings.
type TenantRow struct {
	ID   string
	Name string
}
type UserRow struct {
	TenantID string
	Subject  string
	Role     Role
}
type KeyRow struct {
	ID       string
	TenantID string
	Role     Role
	Label    string
	Revoked  bool
}

// RegTokenRow is the listing read model for a registration token (never the secret).
type RegTokenRow struct {
	TokenID         string
	AgentID         string
	TenantID        string `json:"tenant_id"`
	AgentGeneration string `json:"-"`
	Revoked         bool
}

// RegTokenCredential is the authenticated registration-token binding.
type RegTokenCredential struct {
	AgentID         string
	TenantID        string
	AgentGeneration string
	Hash            string
}

// Store is the identity persistence layer over Postgres.
type Store struct{ db *sql.DB }

// NewStore creates the identity tables (under the shared DDL lock) and returns a
// Store. db must already be open and reachable.
func NewStore(ctx context.Context, db *sql.DB) (*Store, error) {
	if err := store.ApplyMigrationsLocked(ctx, db, "identity", 1, 3, []store.Migration{
		{Version: 1, Name: "baseline", SQL: schemaSQL},
		{Version: 2, Name: "bind-registration-tokens", SQL: registrationTokenBindingSQL},
		{Version: 3, Name: "repair-referential-integrity", SQL: referentialIntegritySQL},
	}); err != nil {
		return nil, err
	}
	if err := store.ApplyDDLLocked(ctx, db, schemaSQL+"\n"+registrationTokenBindingSQL+
		"\n"+referentialIntegritySQL); err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

// CreateTenant inserts a tenant. A duplicate id errors.
func (s *Store) CreateTenant(ctx context.Context, id, name string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO tenants (id, name) VALUES ($1,$2)`, id, name)
	return err
}

// TenantExists reports whether a tenant id is present.
func (s *Store) TenantExists(ctx context.Context, id string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT count(1) FROM tenants WHERE id=$1`, id).Scan(&n)
	return n > 0, err
}

// ListTenants returns all tenant ids+names.
func (s *Store) ListTenants(ctx context.Context) ([]TenantRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name FROM tenants ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TenantRow
	for rows.Next() {
		var tr TenantRow
		if err := rows.Scan(&tr.ID, &tr.Name); err != nil {
			return nil, err
		}
		out = append(out, tr)
	}
	return out, rows.Err()
}

// UpsertUser provisions (tenant, subject) -> role. A subject may belong to many
// tenants; re-upserting the same (tenant, subject) updates the role in place.
func (s *Store) UpsertUser(ctx context.Context, tenantID, subject string, role Role) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO identity_users (tenant_id, subject, role) VALUES ($1,$2,$3)
		 ON CONFLICT (tenant_id, subject) DO UPDATE SET role=EXCLUDED.role`,
		tenantID, subject, string(role))
	return err
}

// DeleteUser removes a user by subject within a tenant.
func (s *Store) DeleteUser(ctx context.Context, tenantID, subject string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM identity_users WHERE tenant_id=$1 AND subject=$2`, tenantID, subject)
	return err
}

// UsersBySubject lists every (tenant, role) the subject belongs to, ordered by
// tenant id. Empty slice (no error) when the subject is unprovisioned anywhere —
// the OIDC authenticator distinguishes 0/1/many to drive tenant selection.
func (s *Store) UsersBySubject(ctx context.Context, subject string) ([]UserRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT tenant_id, subject, role FROM identity_users WHERE subject=$1 ORDER BY tenant_id`, subject)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserRow
	for rows.Next() {
		var u UserRow
		var role string
		if err := rows.Scan(&u.TenantID, &u.Subject, &role); err != nil {
			return nil, err
		}
		u.Role = Role(role)
		out = append(out, u)
	}
	return out, rows.Err()
}

// ListUsers returns users in a tenant.
func (s *Store) ListUsers(ctx context.Context, tenantID string) ([]UserRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT tenant_id, subject, role FROM identity_users WHERE tenant_id=$1 ORDER BY subject`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserRow
	for rows.Next() {
		var u UserRow
		var role string
		if err := rows.Scan(&u.TenantID, &u.Subject, &role); err != nil {
			return nil, err
		}
		u.Role = Role(role)
		out = append(out, u)
	}
	return out, rows.Err()
}

// InsertServiceKey stores a minted key's hash.
func (s *Store) InsertServiceKey(ctx context.Context, id, tenantID, hash string, role Role, label string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO service_keys (id, tenant_id, key_hash, role, label) VALUES ($1,$2,$3,$4,$5)`,
		id, tenantID, hash, string(role), label)
	return err
}

// activeKey is the auth-time read model for a service key.
type activeKey struct {
	TenantID string
	Hash     string
	Role     Role
}

// ActiveKeyByID returns a non-revoked key by id, or ErrNoKey.
func (s *Store) ActiveKeyByID(ctx context.Context, id string) (activeKey, error) {
	var k activeKey
	var role string
	err := s.db.QueryRowContext(ctx,
		`SELECT tenant_id, key_hash, role FROM service_keys WHERE id=$1 AND revoked_at IS NULL`, id).
		Scan(&k.TenantID, &k.Hash, &role)
	if errors.Is(err, sql.ErrNoRows) {
		return activeKey{}, ErrNoKey
	}
	if err != nil {
		return activeKey{}, err
	}
	k.Role = Role(role)
	return k, nil
}

// RevokeKey marks a key revoked. No-op if already revoked or absent.
func (s *Store) RevokeKey(ctx context.Context, tenantID, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE service_keys SET revoked_at=now() WHERE id=$1 AND tenant_id=$2 AND revoked_at IS NULL`,
		id, tenantID)
	return err
}

// ListKeys returns keys in a tenant (secrets never included).
func (s *Store) ListKeys(ctx context.Context, tenantID string) ([]KeyRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, tenant_id, role, label, (revoked_at IS NOT NULL) FROM service_keys WHERE tenant_id=$1 ORDER BY created_at`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KeyRow
	for rows.Next() {
		var k KeyRow
		var role string
		if err := rows.Scan(&k.ID, &k.TenantID, &role, &k.Label, &k.Revoked); err != nil {
			return nil, err
		}
		k.Role = Role(role)
		out = append(out, k)
	}
	return out, rows.Err()
}

// InsertRegistrationToken binds a token to one immutable agent identity.
func (s *Store) InsertRegistrationToken(ctx context.Context, tokenID, agentID, tenantID, agentGeneration, hash string) error {
	if tenantID == "" || agentGeneration == "" {
		return errors.New("identity: registration token requires tenant and agent generation")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO registration_tokens
		    (token_id, agent_id, tenant_id, agent_generation, hash)
		 VALUES ($1,$2,$3,$4,$5)`,
		tokenID, agentID, tenantID, agentGeneration, hash)
	return err
}

// ActiveRegTokenByID returns a non-revoked, fully bound credential. Legacy
// version-1 rows have empty bindings and deliberately fail closed.
func (s *Store) ActiveRegTokenByID(ctx context.Context, tokenID string) (RegTokenCredential, error) {
	var credential RegTokenCredential
	err := s.db.QueryRowContext(ctx,
		`SELECT agent_id, tenant_id, agent_generation, hash
		   FROM registration_tokens
		  WHERE token_id=$1 AND revoked_at IS NULL
		    AND tenant_id <> '' AND agent_generation <> ''`, tokenID).
		Scan(&credential.AgentID, &credential.TenantID,
			&credential.AgentGeneration, &credential.Hash)
	if errors.Is(err, sql.ErrNoRows) {
		return RegTokenCredential{}, ErrNoRegToken
	}
	return credential, err
}

// RevokeRegistrationToken marks a token revoked. No-op if already revoked/absent.
func (s *Store) RevokeRegistrationToken(ctx context.Context, tokenID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE registration_tokens SET revoked_at=now() WHERE token_id=$1 AND revoked_at IS NULL`, tokenID)
	return err
}

// RevokeRegistrationTokensForAgent invalidates one managed-agent generation.
func (s *Store) RevokeRegistrationTokensForAgent(ctx context.Context, tenantID, agentID, agentGeneration string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE registration_tokens
		    SET revoked_at=now()
		  WHERE tenant_id=$1 AND agent_id=$2 AND agent_generation=$3
		    AND revoked_at IS NULL`,
		tenantID, agentID, agentGeneration)
	return err
}

// ListRegistrationTokens returns all tokens (secrets never included).
func (s *Store) ListRegistrationTokens(ctx context.Context) ([]RegTokenRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT token_id, agent_id, tenant_id, agent_generation,
		        (revoked_at IS NOT NULL)
		   FROM registration_tokens ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RegTokenRow
	for rows.Next() {
		var r RegTokenRow
		if err := rows.Scan(&r.TokenID, &r.AgentID, &r.TenantID,
			&r.AgentGeneration, &r.Revoked); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AnyConfigured reports whether any identity rows exist (used for open-mode
// detection: with zero tenants/keys and no OIDC, the edge runs open).
func (s *Store) AnyConfigured(ctx context.Context) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT (SELECT count(1) FROM tenants) + (SELECT count(1) FROM service_keys)`).Scan(&n)
	return n > 0, err
}

// AnyCredentialConfigured reports whether a durable human or service
// credential exists. A tenant row alone is not enough: disabling the bootstrap
// at that point could lock an operator out between creating the tenant and
// minting its first administrator credential.
func (s *Store) AnyCredentialConfigured(ctx context.Context) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT (SELECT count(1) FROM identity_users) +
		        (SELECT count(1) FROM service_keys WHERE revoked_at IS NULL)`).Scan(&n)
	return n > 0, err
}
