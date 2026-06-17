// Package agentstore persists dynamically-managed remote agents in Postgres,
// so the control plane can register/deregister/enable/disable agents at runtime
// (via the admin API or console) and have them survive a restart. It mirrors
// internal/gateway's UpstreamStore.
package agentstore

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"

	"github.com/sausheong/runtime/internal/store"
)

//go:embed schema.sql
var schemaSQL string

// AgentRow is one tenant-registered remote agent (attach-only; a url, never a
// spawned process). auth_secret is an optional per-tenant secret NAME brokered
// at dial; "" = no bearer.
type AgentRow struct {
	ID         string
	TenantID   string
	Name       string
	Model      string
	URL        string
	AuthSecret string
	Enabled    bool
}

// Store persists managed agents in Postgres.
type Store struct{ db *sql.DB }

// New applies the managed_agents DDL (under the shared lock) and returns a
// store. The tenants table (identity schema) must already exist (FK).
func New(ctx context.Context, db *sql.DB) (*Store, error) {
	if err := store.ApplyDDLLocked(ctx, db, schemaSQL); err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

// Insert adds a new managed agent (enabled). Duplicate id is a constraint error.
func (s *Store) Insert(ctx context.Context, r AgentRow) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO managed_agents (id, tenant_id, name, model, url, auth_secret, enabled)
		 VALUES ($1,$2,$3,$4,$5,$6,true)`,
		r.ID, r.TenantID, r.Name, r.Model, r.URL, r.AuthSecret)
	return err
}

// List returns rows for one tenant, or all rows when tenant=="".
func (s *Store) List(ctx context.Context, tenant string) ([]AgentRow, error) {
	q := `SELECT id, tenant_id, name, model, url, auth_secret, enabled FROM managed_agents`
	args := []any{}
	if tenant != "" {
		q += ` WHERE tenant_id=$1`
		args = append(args, tenant)
	}
	q += ` ORDER BY tenant_id, name`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgentRow
	for rows.Next() {
		var r AgentRow
		if err := rows.Scan(&r.ID, &r.TenantID, &r.Name, &r.Model, &r.URL,
			&r.AuthSecret, &r.Enabled); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Get returns one row by id (ok=false when absent).
func (s *Store) Get(ctx context.Context, id string) (AgentRow, bool, error) {
	var r AgentRow
	err := s.db.QueryRowContext(ctx,
		`SELECT id, tenant_id, name, model, url, auth_secret, enabled
		 FROM managed_agents WHERE id=$1`, id).
		Scan(&r.ID, &r.TenantID, &r.Name, &r.Model, &r.URL, &r.AuthSecret, &r.Enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentRow{}, false, nil
	}
	if err != nil {
		return AgentRow{}, false, err
	}
	return r, true, nil
}

// Delete removes a row scoped to its owning tenant (idempotent).
func (s *Store) Delete(ctx context.Context, tenant, id string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM managed_agents WHERE id=$1 AND tenant_id=$2`, id, tenant)
	return err
}

// SetEnabled flips the enabled flag, scoped to the owning tenant (idempotent).
func (s *Store) SetEnabled(ctx context.Context, tenant, id string, enabled bool) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE managed_agents SET enabled=$3 WHERE id=$1 AND tenant_id=$2`,
		id, tenant, enabled)
	return err
}
