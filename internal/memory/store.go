package memory

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "embed"

	hmem "github.com/sausheong/harness/tool/memory"
	"github.com/sausheong/runtime/internal/store"
)

//go:embed schema.sql
var schemaSQL string

// Store is a tenant-pinned Postgres MemoryStore. Every query filters by tenant,
// captured at construction — the agent's (unscoped) tool calls can never reach
// another tenant's pool.
type Store struct {
	db     *sql.DB
	tenant string
}

// NewStore ensures the schema (under the shared DDL lock) and returns a Store
// pinned to tenant. An empty tenant becomes "default".
func NewStore(ctx context.Context, db *sql.DB, tenant string) (*Store, error) {
	if tenant == "" {
		tenant = "default"
	}
	if err := store.ApplyDDLLocked(ctx, db, schemaSQL); err != nil {
		return nil, err
	}
	return &Store{db: db, tenant: tenant}, nil
}

// The projection assumes entry ids are unique over the table's lifetime, which
// harness guarantees (generateID mints a fresh id per create/update). Two
// consequences of the append-only model follow from that assumption: a
// tombstoned id stays dead permanently (a later create reusing it would remain
// hidden), and a duplicate-id create would yield two live rows. Neither is
// reachable through normal unique-id operation; they are noted, not handled.
//
// liveSelect projects the live set: a defining (create|update) row for an
// entry_id that is neither superseded by an update nor tombstoned by a delete,
// within the pinned tenant.
const liveSelect = `
SELECT e.entry_id, e.content, e.tags, e.origin, e.created_at, e.original_created_at
FROM   memory_events e
WHERE  e.tenant_id = $1
  AND  e.op IN ('create','update')
  AND  NOT EXISTS (SELECT 1 FROM memory_events s
                   WHERE s.tenant_id = $1 AND s.supersedes = e.entry_id)
  AND  NOT EXISTS (SELECT 1 FROM memory_events d
                   WHERE d.tenant_id = $1 AND d.op = 'delete' AND d.entry_id = e.entry_id)`

func scanEntry(rows *sql.Rows) (hmem.Entry, error) {
	var (
		e        hmem.Entry
		tags     textArray
		created  time.Time
		original sql.NullTime
	)
	if err := rows.Scan(&e.ID, &e.Content, &tags, &e.Origin, &created, &original); err != nil {
		return hmem.Entry{}, err
	}
	e.Tags = []string(tags)
	// Normalize to UTC: pgx returns TIMESTAMPTZ in Local location, but the
	// store's contract (and Save/Update's return values) are UTC. Without this,
	// a re-read entry differs in zone string from the same freshly-saved entry.
	e.UpdatedAt = created.UTC()
	if original.Valid {
		e.CreatedAt = original.Time.UTC()
	} else {
		e.CreatedAt = created.UTC()
	}
	return e, nil
}

// Save appends a create row. Origin is persisted verbatim (the MemoryTool sets
// it from context before calling). Content validation is the tool's job.
func (s *Store) Save(ctx context.Context, e hmem.Entry) (hmem.Entry, error) {
	now := time.Now().UTC()
	if e.ID == "" {
		e.ID = generateID(now)
	}
	e.CreatedAt = now
	e.UpdatedAt = now
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO memory_events (tenant_id, op, entry_id, content, tags, origin, created_at)
		 VALUES ($1,'create',$2,$3,$4,$5,$6)`,
		s.tenant, e.ID, e.Content, textArray(e.Tags), e.Origin, now)
	if err != nil {
		return hmem.Entry{}, fmt.Errorf("memory: save tenant %q id %q: %w", s.tenant, e.ID, err)
	}
	return e, nil
}

// Update reads the live row for id, then appends an update row with a fresh id
// that supersedes it, carrying tags+origin forward and preserving birth time.
func (s *Store) Update(ctx context.Context, id, content string) (hmem.Entry, error) {
	old, ok, err := s.Get(ctx, id)
	if err != nil {
		return hmem.Entry{}, err
	}
	if !ok {
		return hmem.Entry{}, hmem.ErrNotFound
	}
	now := time.Now().UTC()
	newID := generateID(now)
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO memory_events (tenant_id, op, entry_id, content, tags, origin, supersedes, created_at, original_created_at)
		 VALUES ($1,'update',$2,$3,$4,$5,$6,$7,$8)`,
		s.tenant, newID, content, textArray(old.Tags), old.Origin, id, now, old.CreatedAt)
	if err != nil {
		return hmem.Entry{}, fmt.Errorf("memory: update tenant %q id %q: %w", s.tenant, id, err)
	}
	return hmem.Entry{
		ID:        newID,
		Content:   content,
		Tags:      old.Tags,
		Origin:    old.Origin,
		CreatedAt: old.CreatedAt,
		UpdatedAt: now,
	}, nil
}

// Remove appends a delete tombstone. Idempotent: unknown ids still tombstone and
// return nil.
func (s *Store) Remove(ctx context.Context, id string) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO memory_events (tenant_id, op, entry_id, created_at)
		 VALUES ($1,'delete',$2,$3)`,
		s.tenant, id, now)
	if err != nil {
		return fmt.Errorf("memory: remove tenant %q id %q: %w", s.tenant, id, err)
	}
	return nil
}

// List returns live entries ordered by birth time. tag=="" returns all; else
// entries whose tags contain tag.
func (s *Store) List(ctx context.Context, tag string) ([]hmem.Entry, error) {
	q := liveSelect
	args := []any{s.tenant}
	if tag != "" {
		q += ` AND $2 = ANY(e.tags)`
		args = append(args, tag)
	}
	q += ` ORDER BY COALESCE(e.original_created_at, e.created_at) ASC, e.seq ASC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("memory: list tenant %q: %w", s.tenant, err)
	}
	defer rows.Close()
	var out []hmem.Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, fmt.Errorf("memory: list scan tenant %q: %w", s.tenant, err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Get returns the live entry for id. ok=false (no error) for unknown or
// tombstoned ids.
func (s *Store) Get(ctx context.Context, id string) (hmem.Entry, bool, error) {
	q := liveSelect + ` AND e.entry_id = $2 ORDER BY e.seq DESC LIMIT 1`
	rows, err := s.db.QueryContext(ctx, q, s.tenant, id)
	if err != nil {
		return hmem.Entry{}, false, fmt.Errorf("memory: get tenant %q id %q: %w", s.tenant, id, err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return hmem.Entry{}, false, err
		}
		return hmem.Entry{}, false, nil
	}
	e, err := scanEntry(rows)
	if err != nil {
		return hmem.Entry{}, false, err
	}
	return e, true, nil
}
