package eval

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/sausheong/runtime/internal/store"
)

//go:embed policy_schema.sql
var policySchemaSQL string

// PolicyStoreAPI is the per-agent online-eval-policy persistence surface. Both
// *PolicyStore (Postgres) and *PolicyMemStore implement it.
type PolicyStoreAPI interface {
	PutPolicy(ctx context.Context, p Policy) error // upsert; ValidatePolicy first
	GetPolicy(ctx context.Context, tenant, agentID string) (Policy, bool, error)
	ListPolicies(ctx context.Context, tenant string) ([]Policy, error) // tenant=="" ⇒ all
	DeletePolicy(ctx context.Context, tenant, agentID string) (bool, error)
}

// PolicyStore persists per-agent online eval policies in Postgres with a
// generation counter (kept for idiom consistency with the eval golden-set Store).
type PolicyStore struct {
	db  *sql.DB
	gen atomic.Uint64
}

// NewPolicyStore applies the policy DDL under the shared DDL lock.
func NewPolicyStore(ctx context.Context, db *sql.DB) (*PolicyStore, error) {
	if err := store.ApplyDDLLocked(ctx, db, policySchemaSQL); err != nil {
		return nil, err
	}
	return &PolicyStore{db: db}, nil
}

func (s *PolicyStore) PutPolicy(ctx context.Context, p Policy) error {
	if err := ValidatePolicy(p); err != nil {
		return err
	}
	criteria := p.Criteria
	if criteria == nil {
		criteria = []Criterion{}
	}
	blob, err := json.Marshal(criteria)
	if err != nil {
		return fmt.Errorf("eval policy %s/%s: marshal criteria: %w", p.Tenant, p.AgentID, err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO eval_policies (tenant, agent_id, sample_rate, criteria) VALUES ($1,$2,$3,$4)
		 ON CONFLICT (tenant, agent_id) DO UPDATE SET
		   sample_rate=EXCLUDED.sample_rate, criteria=EXCLUDED.criteria, created_at=now()`,
		p.Tenant, p.AgentID, p.SampleRate, blob)
	if err != nil {
		return fmt.Errorf("eval policy %s/%s: %w", p.Tenant, p.AgentID, err)
	}
	s.gen.Add(1)
	return nil
}

func (s *PolicyStore) GetPolicy(ctx context.Context, tenant, agentID string) (Policy, bool, error) {
	var (
		blob []byte
		p    = Policy{Tenant: tenant, AgentID: agentID}
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT sample_rate, criteria, created_at FROM eval_policies WHERE tenant=$1 AND agent_id=$2`,
		tenant, agentID).Scan(&p.SampleRate, &blob, &p.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Policy{}, false, nil
	}
	if err != nil {
		return Policy{}, false, fmt.Errorf("eval get policy %s/%s: %w", tenant, agentID, err)
	}
	if err := json.Unmarshal(blob, &p.Criteria); err != nil {
		return Policy{}, false, fmt.Errorf("eval get policy %s/%s: unmarshal criteria: %w", tenant, agentID, err)
	}
	return p, true, nil
}

func (s *PolicyStore) ListPolicies(ctx context.Context, tenant string) ([]Policy, error) {
	q := `SELECT tenant, agent_id, sample_rate, criteria, created_at FROM eval_policies`
	args := []any{}
	if tenant != "" {
		q += ` WHERE tenant=$1`
		args = append(args, tenant)
	}
	q += ` ORDER BY tenant, agent_id`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Policy
	for rows.Next() {
		var (
			p    Policy
			blob []byte
		)
		if err := rows.Scan(&p.Tenant, &p.AgentID, &p.SampleRate, &blob, &p.CreatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(blob, &p.Criteria); err != nil {
			return nil, fmt.Errorf("eval list policies: unmarshal criteria for %s/%s: %w", p.Tenant, p.AgentID, err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *PolicyStore) DeletePolicy(ctx context.Context, tenant, agentID string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM eval_policies WHERE tenant=$1 AND agent_id=$2`, tenant, agentID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		s.gen.Add(1)
	}
	return n > 0, nil
}

var _ PolicyStoreAPI = (*PolicyStore)(nil)
