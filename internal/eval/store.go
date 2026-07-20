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

//go:embed schema.sql
var schemaSQL string

// EvalStore is the eval persistence surface. Both *Store (Postgres) and
// *MemStore implement it.
type EvalStore interface {
	PutSet(ctx context.Context, s Set) error
	GetSet(ctx context.Context, tenant, name string) (Set, bool, error)
	ListSets(ctx context.Context, tenant string) ([]Set, error)
	DeleteSet(ctx context.Context, tenant, name string) (bool, error)
	CreateRun(ctx context.Context, r Run) error
	GetRun(ctx context.Context, runID string) (Run, bool, error)
	ListRuns(ctx context.Context, tenant string) ([]Run, error)
	SetRunStatus(ctx context.Context, runID, status string) error
	FinishRun(ctx context.Context, runID, status string, total, passed, failed int, score float64, errMsg string) error
	PutResult(ctx context.Context, runID string, res Result) error
	ListResults(ctx context.Context, runID string) ([]Result, error)
}

// Store persists eval sets/runs/results in Postgres with a generation counter
// (kept for idiom consistency with quota/policy; no live in-process consumer
// reloads on it in M1).
type Store struct {
	db  *sql.DB
	gen atomic.Uint64
}

// NewStore applies the eval DDL under the shared DDL lock.
func NewStore(ctx context.Context, db *sql.DB) (*Store, error) {
	if err := store.ApplyDDLLocked(ctx, db, schemaSQL); err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) PutSet(ctx context.Context, set Set) error {
	if err := ValidateSet(set.Name, set.Cases); err != nil {
		return err
	}
	cases := set.Cases
	if cases == nil {
		cases = []Case{}
	}
	blob, err := json.Marshal(cases)
	if err != nil {
		return fmt.Errorf("eval set %s/%s: marshal cases: %w", set.Tenant, set.Name, err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO eval_sets (tenant, name, cases) VALUES ($1,$2,$3)
		 ON CONFLICT (tenant, name) DO UPDATE SET cases=EXCLUDED.cases, created_at=now()`,
		set.Tenant, set.Name, blob)
	if err != nil {
		return fmt.Errorf("eval set %s/%s: %w", set.Tenant, set.Name, err)
	}
	s.gen.Add(1)
	return nil
}

func (s *Store) GetSet(ctx context.Context, tenant, name string) (Set, bool, error) {
	var (
		blob []byte
		set  = Set{Tenant: tenant, Name: name}
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT cases, created_at FROM eval_sets WHERE tenant=$1 AND name=$2`,
		tenant, name).Scan(&blob, &set.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Set{}, false, nil
	}
	if err != nil {
		return Set{}, false, fmt.Errorf("eval get set %s/%s: %w", tenant, name, err)
	}
	if err := json.Unmarshal(blob, &set.Cases); err != nil {
		return Set{}, false, fmt.Errorf("eval get set %s/%s: unmarshal cases: %w", tenant, name, err)
	}
	return set, true, nil
}

func (s *Store) ListSets(ctx context.Context, tenant string) ([]Set, error) {
	q := `SELECT tenant, name, cases, created_at FROM eval_sets`
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
	var out []Set
	for rows.Next() {
		var (
			set  Set
			blob []byte
		)
		if err := rows.Scan(&set.Tenant, &set.Name, &blob, &set.CreatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(blob, &set.Cases); err != nil {
			return nil, fmt.Errorf("eval list sets: unmarshal cases for %s/%s: %w", set.Tenant, set.Name, err)
		}
		out = append(out, set)
	}
	return out, rows.Err()
}

func (s *Store) DeleteSet(ctx context.Context, tenant, name string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM eval_sets WHERE tenant=$1 AND name=$2`, tenant, name)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		s.gen.Add(1)
	}
	return n > 0, nil
}

func (s *Store) CreateRun(ctx context.Context, r Run) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO eval_runs (run_id, tenant, set_name, agent_id, status) VALUES ($1,$2,$3,$4,$5)`,
		r.RunID, r.Tenant, r.SetName, r.AgentID, r.Status)
	if err != nil {
		return fmt.Errorf("eval create run %s: %w", r.RunID, err)
	}
	s.gen.Add(1)
	return nil
}

func (s *Store) GetRun(ctx context.Context, runID string) (Run, bool, error) {
	var (
		r        Run
		finished sql.NullTime
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT run_id, tenant, set_name, agent_id, status, total, passed, failed, score, error, created_at, finished_at
		   FROM eval_runs WHERE run_id=$1`, runID).Scan(
		&r.RunID, &r.Tenant, &r.SetName, &r.AgentID, &r.Status,
		&r.Total, &r.Passed, &r.Failed, &r.Score, &r.Error, &r.CreatedAt, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, false, nil
	}
	if err != nil {
		return Run{}, false, fmt.Errorf("eval get run %s: %w", runID, err)
	}
	if finished.Valid {
		r.FinishedAt = &finished.Time
	}
	return r, true, nil
}

func (s *Store) ListRuns(ctx context.Context, tenant string) ([]Run, error) {
	q := `SELECT run_id, tenant, set_name, agent_id, status, total, passed, failed, score, error, created_at, finished_at
	        FROM eval_runs`
	args := []any{}
	if tenant != "" {
		q += ` WHERE tenant=$1`
		args = append(args, tenant)
	}
	q += ` ORDER BY created_at DESC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var (
			r        Run
			finished sql.NullTime
		)
		if err := rows.Scan(
			&r.RunID, &r.Tenant, &r.SetName, &r.AgentID, &r.Status,
			&r.Total, &r.Passed, &r.Failed, &r.Score, &r.Error, &r.CreatedAt, &finished); err != nil {
			return nil, err
		}
		if finished.Valid {
			r.FinishedAt = &finished.Time
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) SetRunStatus(ctx context.Context, runID, status string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE eval_runs SET status=$2 WHERE run_id=$1`, runID, status)
	if err != nil {
		return fmt.Errorf("eval set run status %s: %w", runID, err)
	}
	s.gen.Add(1)
	return nil
}

func (s *Store) FinishRun(ctx context.Context, runID, status string, total, passed, failed int, score float64, errMsg string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE eval_runs SET status=$2, total=$3, passed=$4, failed=$5, score=$6, error=$7, finished_at=now()
		   WHERE run_id=$1`,
		runID, status, total, passed, failed, score, errMsg)
	if err != nil {
		return fmt.Errorf("eval finish run %s: %w", runID, err)
	}
	s.gen.Add(1)
	return nil
}

func (s *Store) PutResult(ctx context.Context, runID string, res Result) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO eval_results (run_id, case_index, input, output, scorer, passed, detail)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)
		 ON CONFLICT (run_id, case_index) DO UPDATE SET
		   input=EXCLUDED.input, output=EXCLUDED.output, scorer=EXCLUDED.scorer,
		   passed=EXCLUDED.passed, detail=EXCLUDED.detail`,
		runID, res.CaseIndex, res.Input, res.Output, res.Scorer, res.Passed, res.Detail)
	if err != nil {
		return fmt.Errorf("eval put result %s#%d: %w", runID, res.CaseIndex, err)
	}
	s.gen.Add(1)
	return nil
}

func (s *Store) ListResults(ctx context.Context, runID string) ([]Result, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT case_index, input, output, scorer, passed, detail
		   FROM eval_results WHERE run_id=$1 ORDER BY case_index ASC`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Result
	for rows.Next() {
		var res Result
		if err := rows.Scan(&res.CaseIndex, &res.Input, &res.Output, &res.Scorer, &res.Passed, &res.Detail); err != nil {
			return nil, err
		}
		out = append(out, res)
	}
	return out, rows.Err()
}

var _ EvalStore = (*Store)(nil)
