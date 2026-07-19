package policy

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	cedar "github.com/cedar-policy/cedar-go"

	"github.com/sausheong/runtime/internal/store"
)

//go:embed schema.sql
var schemaSQL string

// Row is one persisted tenant-layer Cedar policy (exactly one statement).
type Row struct {
	Tenant, Name, CedarText string
	CreatedAt, UpdatedAt    time.Time
}

// PolicyStore is the tenant policy persistence surface. Both the Postgres
// Store and the in-memory MemStore implement it; the Engine consumes it via
// the embedded TenantPolicies (PoliciesFor).
type PolicyStore interface {
	TenantPolicies // PoliciesFor(ctx, tenant) ([]NamedPolicy, uint64, error)
	Insert(ctx context.Context, r Row) error
	List(ctx context.Context, tenant string) ([]Row, error)
	Delete(ctx context.Context, tenant, name string) (bool, error)
}

// validateOne parses text and requires exactly one Cedar policy statement, so
// list/delete granularity is the statement and ids stay stable.
func validateOne(text string) error {
	ps, err := cedar.NewPolicySetFromBytes("policy.cedar", []byte(text))
	if err != nil {
		return fmt.Errorf("invalid Cedar: %w", err)
	}
	n := 0
	for range ps.All() {
		n++
	}
	if n != 1 {
		return fmt.Errorf("exactly one policy statement per row (got %d)", n)
	}
	return nil
}

// Store persists tenant policies in Postgres and tracks a generation counter
// so the Engine's per-tenant compiled cache invalidates on any mutation.
type Store struct {
	db  *sql.DB
	gen atomic.Uint64
}

// NewStore applies the gateway_policies DDL (under the shared DDL lock) and
// returns a store. The tenants table need not exist — policies are keyed by
// tenant string with no FK, so a tenant's policies can be authored before or
// after the tenant row (parity with how the engine evaluates by tenant id).
func NewStore(ctx context.Context, db *sql.DB) (*Store, error) {
	if err := store.ApplyDDLLocked(ctx, db, schemaSQL); err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

// Insert validates and persists one policy. Empty tenant/name, unparseable or
// multi-statement text, and duplicate (tenant,name) are rejected.
func (s *Store) Insert(ctx context.Context, r Row) error {
	if r.Tenant == "" || r.Name == "" {
		return errors.New("policy: tenant and name are required")
	}
	if err := validateOne(r.CedarText); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO gateway_policies (tenant, name, cedar_text) VALUES ($1,$2,$3)`,
		r.Tenant, r.Name, r.CedarText)
	if err != nil {
		// Best-effort duplicate detection without importing pq error codes:
		// the PK collision surfaces as a unique-violation error string.
		return fmt.Errorf("policy %q: %w", r.Name, err)
	}
	s.gen.Add(1)
	return nil
}

// List returns a tenant's policies (or all rows when tenant==""), name-ordered.
func (s *Store) List(ctx context.Context, tenant string) ([]Row, error) {
	q := `SELECT tenant, name, cedar_text, created_at, updated_at FROM gateway_policies`
	args := []any{}
	if tenant != "" {
		q += ` WHERE tenant=$1`
		args = append(args, tenant)
	}
	q += ` ORDER BY tenant, name`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.Tenant, &r.Name, &r.CedarText, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Delete removes one policy scoped to its tenant. Returns false when no row
// matched (the API layer maps that to no-oracle semantics).
func (s *Store) Delete(ctx context.Context, tenant, name string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM gateway_policies WHERE tenant=$1 AND name=$2`, tenant, name)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		s.gen.Add(1)
	}
	return n > 0, nil
}

// PoliciesFor returns the tenant's policies as NamedPolicy (id tenant/<name>)
// plus the current generation.
func (s *Store) PoliciesFor(ctx context.Context, tenant string) ([]NamedPolicy, uint64, error) {
	rows, err := s.List(ctx, tenant)
	if err != nil {
		return nil, 0, err
	}
	out := make([]NamedPolicy, 0, len(rows))
	for _, r := range rows {
		out = append(out, NamedPolicy{ID: "tenant/" + r.Name, Source: r.CedarText})
	}
	return out, s.gen.Load(), nil
}
