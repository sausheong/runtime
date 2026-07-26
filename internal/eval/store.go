package eval

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/sausheong/runtime/internal/store"
)

//go:embed schema.sql
var schemaSQL string

//go:embed run_state_constraints.sql
var runStateConstraintsSQL string

//go:embed referential_integrity.sql
var referentialIntegritySQL string

var (
	ErrRunExists            = errors.New("eval: run already exists")
	ErrInvalidRunTransition = errors.New("eval: invalid run transition")
)

func validateNewRun(r Run) error {
	if r.RunID == "" || r.Status != StatusPending || r.LeaseOwner != "" ||
		r.LeaseUntil != nil || r.FinishedAt != nil {
		return fmt.Errorf("%w: new run must be pending, unleased, and unfinished",
			ErrInvalidRunTransition)
	}
	return nil
}

func validateClaim(owner string, now, until time.Time) error {
	if owner == "" {
		return fmt.Errorf("%w: claim owner is required", ErrInvalidRunTransition)
	}
	if !until.After(now) {
		return fmt.Errorf("%w: claim lease must expire after acquisition",
			ErrInvalidRunTransition)
	}
	return nil
}

func validateFinalization(owner, status string) error {
	if owner == "" {
		return fmt.Errorf("%w: finalization owner is required", ErrInvalidRunTransition)
	}
	if status != StatusCompleted && status != StatusError {
		return fmt.Errorf("%w: final status must be completed or error",
			ErrInvalidRunTransition)
	}
	return nil
}

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
	ListIncompleteRuns(ctx context.Context, limit int) ([]Run, error)
	ClaimRun(ctx context.Context, runID, owner string, now, until time.Time) (bool, error)
	FailPendingRun(ctx context.Context, runID, errMsg string) (bool, error)
	FinishRunClaimed(ctx context.Context, runID, owner, status string, total, passed, failed int, score float64, errMsg string) (bool, error)
	PutResultClaimed(ctx context.Context, runID, owner string, res Result) (bool, error)
	ListResults(ctx context.Context, runID string) ([]Result, error)
	ReapBefore(ctx context.Context, before time.Time, batch int) (int64, error)
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
	if err := store.ApplyMigrationsLocked(ctx, db, "evaluation", 1, 3, []store.Migration{
		{Version: 1, Name: "baseline", SQL: schemaSQL},
		{Version: 2, Name: "enforce-run-state", SQL: runStateConstraintsSQL},
		{Version: 3, Name: "repair-referential-integrity", SQL: referentialIntegritySQL},
	}); err != nil {
		return nil, err
	}
	if err := store.ApplyDDLLocked(ctx, db, schemaSQL+"\n"+runStateConstraintsSQL+
		"\n"+referentialIntegritySQL); err != nil {
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
	if err := validateNewRun(r); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO eval_runs (run_id, tenant, set_name, agent_id, status) VALUES ($1,$2,$3,$4,$5)`,
		r.RunID, r.Tenant, r.SetName, r.AgentID, r.Status)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return fmt.Errorf("%w: %s", ErrRunExists, r.RunID)
		}
		return fmt.Errorf("eval create run %s: %w", r.RunID, err)
	}
	s.gen.Add(1)
	return nil
}

func (s *Store) GetRun(ctx context.Context, runID string) (Run, bool, error) {
	var (
		r          Run
		finished   sql.NullTime
		leaseUntil sql.NullTime
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT run_id, tenant, set_name, agent_id, status, total, passed, failed, score, error,
		        lease_owner, lease_until, created_at, finished_at
		   FROM eval_runs WHERE run_id=$1`, runID).Scan(
		&r.RunID, &r.Tenant, &r.SetName, &r.AgentID, &r.Status,
		&r.Total, &r.Passed, &r.Failed, &r.Score, &r.Error,
		&r.LeaseOwner, &leaseUntil, &r.CreatedAt, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, false, nil
	}
	if err != nil {
		return Run{}, false, fmt.Errorf("eval get run %s: %w", runID, err)
	}
	if finished.Valid {
		r.FinishedAt = &finished.Time
	}
	if leaseUntil.Valid {
		r.LeaseUntil = &leaseUntil.Time
	}
	return r, true, nil
}

func (s *Store) ListRuns(ctx context.Context, tenant string) ([]Run, error) {
	q := `SELECT run_id, tenant, set_name, agent_id, status, total, passed, failed, score, error,
	            lease_owner, lease_until, created_at, finished_at
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
			r          Run
			finished   sql.NullTime
			leaseUntil sql.NullTime
		)
		if err := rows.Scan(
			&r.RunID, &r.Tenant, &r.SetName, &r.AgentID, &r.Status,
			&r.Total, &r.Passed, &r.Failed, &r.Score, &r.Error,
			&r.LeaseOwner, &leaseUntil, &r.CreatedAt, &finished); err != nil {
			return nil, err
		}
		if finished.Valid {
			r.FinishedAt = &finished.Time
		}
		if leaseUntil.Valid {
			r.LeaseUntil = &leaseUntil.Time
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListIncompleteRuns returns a bounded oldest-first page of currently claimable
// work. Live foreign leases are excluded so they cannot starve later pending
// rows; a later periodic scan sees them once their lease expires.
func (s *Store) ListIncompleteRuns(ctx context.Context, limit int) ([]Run, error) {
	if limit < 1 {
		limit = 1
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT run_id, tenant, set_name, agent_id, status, total, passed, failed, score, error,
		        lease_owner, lease_until, created_at, finished_at
		   FROM eval_runs
		  WHERE status IN ($1,$2)
		    AND (lease_owner='' OR lease_until IS NULL OR lease_until <= now())
		  ORDER BY created_at, run_id
		  LIMIT $3`,
		StatusPending, StatusRunning, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRuns(rows)
}

func scanRuns(rows *sql.Rows) ([]Run, error) {
	var out []Run
	for rows.Next() {
		var (
			r          Run
			finished   sql.NullTime
			leaseUntil sql.NullTime
		)
		if err := rows.Scan(
			&r.RunID, &r.Tenant, &r.SetName, &r.AgentID, &r.Status,
			&r.Total, &r.Passed, &r.Failed, &r.Score, &r.Error,
			&r.LeaseOwner, &leaseUntil, &r.CreatedAt, &finished); err != nil {
			return nil, err
		}
		if finished.Valid {
			r.FinishedAt = &finished.Time
		}
		if leaseUntil.Valid {
			r.LeaseUntil = &leaseUntil.Time
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ClaimRun atomically acquires or renews a lease for pending/running work. A
// live lease held by another worker is never stolen.
func (s *Store) ClaimRun(ctx context.Context, runID, owner string, now, until time.Time) (bool, error) {
	if err := validateClaim(owner, now, until); err != nil {
		return false, err
	}
	leaseFor := until.Sub(now)
	res, err := s.db.ExecContext(ctx,
		`UPDATE eval_runs
		    SET status=$2, lease_owner=$3, lease_until=now()+make_interval(secs => $4)
		  WHERE run_id=$1
		    AND status IN ($2,$5)
		    AND (lease_owner='' OR lease_owner=$3 OR lease_until IS NULL OR lease_until <= now())`,
		runID, StatusRunning, owner, leaseFor.Seconds(), StatusPending)
	if err != nil {
		return false, fmt.Errorf("eval claim run %s: %w", runID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("eval claim run %s rows: %w", runID, err)
	}
	if n > 0 {
		s.gen.Add(1)
	}
	return n > 0, nil
}

func (s *Store) FailPendingRun(ctx context.Context, runID, errMsg string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE eval_runs
		    SET status=$2, error=$3, finished_at=now()
		  WHERE run_id=$1 AND status=$4 AND lease_owner=''`,
		runID, StatusError, errMsg, StatusPending)
	if err != nil {
		return false, fmt.Errorf("eval fail pending run %s: %w", runID, err)
	}
	n, err := res.RowsAffected()
	if n > 0 {
		s.gen.Add(1)
	}
	return n == 1, err
}

// FinishRunClaimed finalizes a run only for its current lease owner.
func (s *Store) FinishRunClaimed(ctx context.Context, runID, owner, status string, total, passed, failed int, score float64, errMsg string) (bool, error) {
	if err := validateFinalization(owner, status); err != nil {
		return false, err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE eval_runs
		    SET status=$3, total=$4, passed=$5, failed=$6, score=$7, error=$8,
		        lease_owner='', lease_until=NULL, finished_at=now()
		  WHERE run_id=$1 AND lease_owner=$2 AND status=$9 AND lease_until > now()`,
		runID, owner, status, total, passed, failed, score, errMsg, StatusRunning)
	if err != nil {
		return false, fmt.Errorf("eval finish claimed run %s: %w", runID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("eval finish claimed run %s rows: %w", runID, err)
	}
	if n > 0 {
		s.gen.Add(1)
	}
	return n > 0, nil
}

func (s *Store) PutResultClaimed(ctx context.Context, runID, owner string, result Result) (bool, error) {
	if owner == "" {
		return false, fmt.Errorf("%w: result owner is required", ErrInvalidRunTransition)
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO eval_results (run_id, case_index, input, output, scorer, passed, detail)
		 SELECT $1,$3,$4,$5,$6,$7,$8
		   FROM eval_runs
		  WHERE run_id=$1 AND lease_owner=$2 AND status=$9 AND lease_until > now()
		  FOR UPDATE
		 ON CONFLICT (run_id, case_index) DO UPDATE SET
		   input=EXCLUDED.input, output=EXCLUDED.output, scorer=EXCLUDED.scorer,
		   passed=EXCLUDED.passed, detail=EXCLUDED.detail`,
		runID, owner, result.CaseIndex, result.Input, result.Output, result.Scorer,
		result.Passed, result.Detail, StatusRunning)
	if err != nil {
		return false, fmt.Errorf("eval put claimed result %s#%d: %w", runID, result.CaseIndex, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("eval put claimed result %s#%d rows: %w", runID, result.CaseIndex, err)
	}
	if n > 0 {
		s.gen.Add(1)
	}
	return n > 0, nil
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

// ReapBefore deletes completed/error runs whose completion time is older than
// before. Results cascade; pending/running work is retained for recovery. The
// state-coherence migration guarantees terminal rows have finished_at.
func (s *Store) ReapBefore(ctx context.Context, before time.Time, batch int) (int64, error) {
	if batch < 1 {
		batch = 1
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM eval_runs
		  WHERE run_id IN (
		        SELECT run_id FROM eval_runs
		         WHERE finished_at < $1 AND status IN ($2,$3)
		         ORDER BY finished_at, run_id
		         LIMIT $4
		  )`,
		before, StatusCompleted, StatusError, batch)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if n > 0 {
		s.gen.Add(1)
	}
	return n, err
}

var _ EvalStore = (*Store)(nil)
