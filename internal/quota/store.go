package quota

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/sausheong/runtime/internal/store"
)

//go:embed schema.sql
var schemaSQL string

// Rule is one quota: rate_per_min requests for calls matching (Tenant, Upstream).
// Either key may be the literal "*" wildcard.
type Rule struct {
	Tenant     string
	Upstream   string
	RatePerMin int
}

// QuotaStore is the quota persistence surface. Both *Store (Postgres) and
// *MemStore implement it. Rules returns the full rule set + a generation
// counter so the Limiter reloads on any mutation.
type QuotaStore interface {
	Insert(ctx context.Context, r Rule) error
	List(ctx context.Context, tenant string) ([]Rule, error)
	Delete(ctx context.Context, tenant, upstream string) (bool, error)
	Rules(ctx context.Context) ([]Rule, uint64, error)
}

func validRule(r Rule) error {
	if r.Tenant == "" || r.Upstream == "" {
		return errors.New("quota: tenant and upstream are required")
	}
	if r.RatePerMin <= 0 {
		return errors.New("quota: rate_per_min must be > 0")
	}
	return nil
}

// Store persists quotas in Postgres with a generation counter for cache
// invalidation in the live Limiter.
type Store struct {
	db  *sql.DB
	gen atomic.Uint64
}

// NewStore applies the gateway_quotas DDL under the shared DDL lock.
func NewStore(ctx context.Context, db *sql.DB) (*Store, error) {
	if err := store.ApplyDDLLocked(ctx, db, schemaSQL); err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Insert(ctx context.Context, r Rule) error {
	if err := validRule(r); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO gateway_quotas (tenant, upstream, rate_per_min) VALUES ($1,$2,$3)`,
		r.Tenant, r.Upstream, r.RatePerMin)
	if err != nil {
		return fmt.Errorf("quota %s/%s: %w", r.Tenant, r.Upstream, err)
	}
	s.gen.Add(1)
	return nil
}

func (s *Store) List(ctx context.Context, tenant string) ([]Rule, error) {
	q := `SELECT tenant, upstream, rate_per_min FROM gateway_quotas`
	args := []any{}
	if tenant != "" {
		q += ` WHERE tenant=$1`
		args = append(args, tenant)
	}
	q += ` ORDER BY tenant, upstream`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Rule
	for rows.Next() {
		var r Rule
		if err := rows.Scan(&r.Tenant, &r.Upstream, &r.RatePerMin); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) Delete(ctx context.Context, tenant, upstream string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM gateway_quotas WHERE tenant=$1 AND upstream=$2`, tenant, upstream)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		s.gen.Add(1)
	}
	return n > 0, nil
}

func (s *Store) Rules(ctx context.Context) ([]Rule, uint64, error) {
	// Snapshot the generation BEFORE the read: a write committing between the
	// SELECT and the Load would otherwise pair stale rows with the newer gen,
	// and the limiter's gen-equality short-circuit would then mask the change
	// until the next mutation. A gen read before the rows only ever forces one
	// extra harmless rebuild, never a missed update.
	gen := s.gen.Load()
	rows, err := s.List(ctx, "")
	if err != nil {
		return nil, 0, err
	}
	return rows, gen, nil
}
