package eval

import (
	"context"
	"database/sql"
	_ "embed"
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
