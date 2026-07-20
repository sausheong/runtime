package memory

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "embed"

	hmem "github.com/sausheong/harness/tool/memory"
	"github.com/sausheong/runtime/internal/store"
)

//go:embed schema.sql
var schemaSQL string

//go:embed embed_schema.sql
var embedSchemaSQL string

// KindFact / KindSummary / KindEpisode are the memory_events.kind discriminators.
// Facts are the existing tool/agent path (Save/Update); summaries are per-session
// rolling digests written via PutSessionSummary and excluded from similarity
// recall; episodes are strategy-captured turn records (SaveKind with KindEpisode).
const (
	KindFact    = "fact"
	KindSummary = "summary"
	KindEpisode = "episode"
)

// Store is a tenant-pinned Postgres MemoryStore. Every query filters by tenant,
// captured at construction — the agent's (unscoped) tool calls can never reach
// another tenant's pool. An optional Embedder enables semantic recall: Save/Update
// write an embedding vector and SearchSimilar ranks by cosine similarity.
type Store struct {
	db       *sql.DB
	tenant   string
	embedder Embedder
	// summaryLocks stripes a per-session mutex over PutSessionSummary so that two
	// overlapping same-session summary writes serialize. PutSessionSummary is a
	// non-atomic read (live prevID) then write (INSERT supersedes=prevID); without
	// this, two concurrent writers read the same prevID and both go live, forking
	// the supersede chain into multiple live rows. The KG is process-shared and one
	// Store is pinned per tenant, so an in-process lock is the correct scope: all a
	// session's ingest goroutines run in this process. Keyed by session_id (the
	// Store is already tenant-scoped); zero value of sync.Map is usable.
	summaryLocks sync.Map // map[string]*sync.Mutex
}

// summaryLock returns the per-session mutex, creating it on first use.
func (s *Store) summaryLock(sessionID string) *sync.Mutex {
	m, _ := s.summaryLocks.LoadOrStore(sessionID, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// Option configures a Store at construction.
type Option func(*Store)

// WithEmbedder enables semantic recall: entries are embedded on save and the
// embeddings DDL (pgvector extension, vector column, HNSW index) is applied.
func WithEmbedder(e Embedder) Option {
	return func(s *Store) { s.embedder = e }
}

// NewStore ensures the schema (under the shared DDL lock) and returns a Store
// pinned to tenant. An empty tenant becomes "default". With WithEmbedder, the
// embeddings DDL is also applied (dim from the embedder).
func NewStore(ctx context.Context, db *sql.DB, tenant string, opts ...Option) (*Store, error) {
	if tenant == "" {
		tenant = "default"
	}
	s := &Store{db: db, tenant: tenant}
	for _, o := range opts {
		o(s)
	}
	if err := store.ApplyDDLLocked(ctx, db, schemaSQL); err != nil {
		return nil, err
	}
	if s.embedder != nil {
		ddl := fmt.Sprintf(embedSchemaSQL, s.embedder.Dim())
		if err := store.ApplyDDLLocked(ctx, db, ddl); err != nil {
			return nil, fmt.Errorf("memory: embeddings schema: %w", err)
		}
	}
	return s, nil
}

// embedOrNil embeds text, returning nil (and logging) on failure so the write
// degrades rather than failing. Returns nil immediately when no embedder is set.
func (s *Store) embedOrNil(ctx context.Context, id, text string) any {
	if s.embedder == nil {
		return nil
	}
	vec, err := s.embedder.Embed(ctx, text)
	if err != nil {
		slog.Warn("memory: embed failed; storing NULL embedding", "tenant", s.tenant, "id", id, "err", err)
		return nil
	}
	return pgVector(vec)
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
  AND  e.actor_id = $2
  AND  e.kind = 'fact'
  AND  e.op IN ('create','update')
  AND  NOT EXISTS (SELECT 1 FROM memory_events s
                   WHERE s.tenant_id = $1 AND s.supersedes = e.entry_id)
  AND  NOT EXISTS (SELECT 1 FROM memory_events d
                   WHERE d.tenant_id = $1 AND d.op = 'delete' AND d.entry_id = e.entry_id)`

// liveSummaryIDSelect / liveSummaryContentSelect project the single live summary
// row for a (tenant, session_id): a create|update row not superseded and not
// tombstoned, with kind='summary'. Mirrors liveSelect's liveness clauses but keys
// on the session_id column (summaries rotate entry_ids on each update).
const liveSummaryLiveness = `
FROM   memory_events e
WHERE  e.tenant_id = $1 AND e.session_id = $2 AND e.kind = 'summary'
  AND  e.op IN ('create','update')
  AND  NOT EXISTS (SELECT 1 FROM memory_events s
                   WHERE s.tenant_id = $1 AND s.supersedes = e.entry_id)
  AND  NOT EXISTS (SELECT 1 FROM memory_events d
                   WHERE d.tenant_id = $1 AND d.op = 'delete' AND d.entry_id = e.entry_id)
ORDER BY e.seq DESC LIMIT 1`

// gcSweep deletes up to $3 dead create/update rows for the tenant older than
// $2, in ASCENDING seq order. "Dead" = superseded OR tombstoned — the exact
// complement of liveSelect's exclusions, within create/update rows. Ascending
// seq is the resurrection-safety ordering: a deleted row supersedes only
// smaller-seq rows, already gone. Delete-tombstone rows (op='delete') are never
// reaped (tiny; removing one could resurrect a surviving create row).
const gcSweep = `
DELETE FROM memory_events WHERE seq IN (
  SELECT e.seq FROM memory_events e
  WHERE e.tenant_id = $1
    AND e.op IN ('create','update')
    AND e.created_at < $2
    AND ( EXISTS (SELECT 1 FROM memory_events s
                  WHERE s.tenant_id = $1 AND s.supersedes = e.entry_id)
       OR EXISTS (SELECT 1 FROM memory_events d
                  WHERE d.tenant_id = $1 AND d.op = 'delete' AND d.entry_id = e.entry_id) )
  ORDER BY e.seq ASC
  LIMIT $3 )`

const liveSummaryIDSelect = `SELECT e.entry_id, e.original_created_at ` + liveSummaryLiveness
const liveSummaryContentSelect = `SELECT e.content ` + liveSummaryLiveness

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

// Save appends a create row for a fact (the harness MemoryStore contract; the
// MemoryTool sets Origin before calling). Content validation is the tool's job.
func (s *Store) Save(ctx context.Context, e hmem.Entry) (hmem.Entry, error) {
	return s.SaveKind(ctx, e, KindFact)
}

// SaveKind appends a create row with an explicit kind (KindFact | KindEpisode).
// The strategy pipeline uses this so each accumulate-strategy stamps its own
// kind; Save delegates here with KindFact for the tool path.
func (s *Store) SaveKind(ctx context.Context, e hmem.Entry, kind string) (hmem.Entry, error) {
	now := time.Now().UTC()
	if e.ID == "" {
		e.ID = generateID(now)
	}
	e.CreatedAt = now
	e.UpdatedAt = now
	actor := actorFrom(ctx)
	var err error
	if s.embedder == nil {
		_, err = s.db.ExecContext(ctx,
			`INSERT INTO memory_events (tenant_id, op, entry_id, content, tags, origin, created_at, kind, actor_id)
			 VALUES ($1,'create',$2,$3,$4,$5,$6,$7,$8)`,
			s.tenant, e.ID, e.Content, textArray(e.Tags), e.Origin, now, kind, actor)
	} else {
		_, err = s.db.ExecContext(ctx,
			`INSERT INTO memory_events (tenant_id, op, entry_id, content, tags, origin, created_at, kind, actor_id, embedding)
			 VALUES ($1,'create',$2,$3,$4,$5,$6,$7,$8,$9)`,
			s.tenant, e.ID, e.Content, textArray(e.Tags), e.Origin, now, kind, actor, s.embedOrNil(ctx, e.ID, e.Content))
	}
	if err != nil {
		return hmem.Entry{}, fmt.Errorf("memory: save tenant %q id %q kind %q: %w", s.tenant, e.ID, kind, err)
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
	actor := actorFrom(ctx)
	if s.embedder == nil {
		_, err = s.db.ExecContext(ctx,
			`INSERT INTO memory_events (tenant_id, op, entry_id, content, tags, origin, supersedes, created_at, original_created_at, kind, actor_id)
			 VALUES ($1,'update',$2,$3,$4,$5,$6,$7,$8,'fact',$9)`,
			s.tenant, newID, content, textArray(old.Tags), old.Origin, id, now, old.CreatedAt, actor)
	} else {
		_, err = s.db.ExecContext(ctx,
			`INSERT INTO memory_events (tenant_id, op, entry_id, content, tags, origin, supersedes, created_at, original_created_at, kind, actor_id, embedding)
			 VALUES ($1,'update',$2,$3,$4,$5,$6,$7,$8,'fact',$9,$10)`,
			s.tenant, newID, content, textArray(old.Tags), old.Origin, id, now, old.CreatedAt, actor, s.embedOrNil(ctx, newID, content))
	}
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
	args := []any{s.tenant, actorFrom(ctx)}
	if tag != "" {
		q += ` AND $3 = ANY(e.tags)`
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
	q := liveSelect + ` AND e.entry_id = $3 ORDER BY e.seq DESC LIMIT 1`
	rows, err := s.db.QueryContext(ctx, q, s.tenant, actorFrom(ctx), id)
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

// PutSessionSummary writes the rolling summary for a session as exactly one live
// kind='summary' row per (tenant, session_id). The first write creates it; each
// later write appends an update row (fresh id) that supersedes the prior live
// summary for this session, so the live set never grows beyond one. Summaries are
// keyed by the session_id column, not entry_id (Update rotates entry_ids). The
// summary is embedded when an embedder is present (harmless; recall is keyed, and
// SearchSimilar excludes kind='summary').
func (s *Store) PutSessionSummary(ctx context.Context, sessionID, content string) error {
	// Serialize same-session writes over the full read+write critical section:
	// two overlapping PutSessionSummary(S) must not both read the same live
	// prevID and both go live (forking the supersede chain). Different sessions
	// use distinct mutexes and do not contend.
	lock := s.summaryLock(sessionID)
	lock.Lock()
	defer lock.Unlock()

	actor := actorFrom(ctx)
	now := time.Now().UTC()
	// Find the current live summary row's entry_id + birth time (if any).
	var prevID string
	var prevOriginal sql.NullTime
	err := s.db.QueryRowContext(ctx, liveSummaryIDSelect, s.tenant, sessionID).Scan(&prevID, &prevOriginal)
	switch {
	case err == sql.ErrNoRows:
		// create
		newID := generateID(now)
		if s.embedder == nil {
			_, err = s.db.ExecContext(ctx,
				`INSERT INTO memory_events (tenant_id, op, entry_id, content, origin, created_at, kind, session_id, actor_id)
				 VALUES ($1,'create',$2,$3,'summary',$4,'summary',$5,$6)`,
				s.tenant, newID, content, now, sessionID, actor)
		} else {
			_, err = s.db.ExecContext(ctx,
				`INSERT INTO memory_events (tenant_id, op, entry_id, content, origin, created_at, kind, session_id, actor_id, embedding)
				 VALUES ($1,'create',$2,$3,'summary',$4,'summary',$5,$6,$7)`,
				s.tenant, newID, content, now, sessionID, actor, s.embedOrNil(ctx, newID, content))
		}
	case err != nil:
		return fmt.Errorf("memory: summary lookup tenant %q session %q: %w", s.tenant, sessionID, err)
	default:
		// update: supersede prev, preserve birth time
		newID := generateID(now)
		orig := prevOriginal
		if !orig.Valid {
			orig = sql.NullTime{Time: now, Valid: true}
		}
		if s.embedder == nil {
			_, err = s.db.ExecContext(ctx,
				`INSERT INTO memory_events (tenant_id, op, entry_id, content, origin, supersedes, created_at, original_created_at, kind, session_id, actor_id)
				 VALUES ($1,'update',$2,$3,'summary',$4,$5,$6,'summary',$7,$8)`,
				s.tenant, newID, content, prevID, now, orig.Time, sessionID, actor)
		} else {
			_, err = s.db.ExecContext(ctx,
				`INSERT INTO memory_events (tenant_id, op, entry_id, content, origin, supersedes, created_at, original_created_at, kind, session_id, actor_id, embedding)
				 VALUES ($1,'update',$2,$3,'summary',$4,$5,$6,'summary',$7,$8,$9)`,
				s.tenant, newID, content, prevID, now, orig.Time, sessionID, actor, s.embedOrNil(ctx, newID, content))
		}
	}
	if err != nil {
		return fmt.Errorf("memory: put summary tenant %q session %q: %w", s.tenant, sessionID, err)
	}
	return nil
}

// GetSessionSummary returns the live summary content for a session, ok=false when
// none exists.
func (s *Store) GetSessionSummary(ctx context.Context, sessionID string) (string, bool, error) {
	var content string
	err := s.db.QueryRowContext(ctx, liveSummaryContentSelect, s.tenant, sessionID).Scan(&content)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("memory: get summary tenant %q session %q: %w", s.tenant, sessionID, err)
	}
	return content, true, nil
}

// pgVector binds a []float32 as a pgvector literal ("[0.1,0.2,...]").
type pgVector []float32

func (v pgVector) Value() (driver.Value, error) {
	if v == nil {
		return nil, nil
	}
	var b strings.Builder
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(f), 'f', -1, 32))
	}
	b.WriteByte(']')
	return b.String(), nil
}

// SearchSimilar returns up to k live, embedded entries for the pinned tenant
// whose cosine similarity to queryVec is >= floor, nearest first. Reuses M1's
// liveness clauses (superseded/tombstoned excluded) and skips NULL embeddings.
func (s *Store) SearchSimilar(ctx context.Context, queryVec []float32, k int, floor float64, kind string) ([]hmem.Entry, error) {
	// The liveness clauses (op IN, the two NOT EXISTS) mirror the liveSelect
	// constant; they are re-spelled inline because SearchSimilar needs $3/$4/$5
	// for the vector/floor/limit args (actor_id is $2, kind is $6). Keep these in
	// sync with liveSelect.
	q := `
SELECT e.entry_id, e.content, e.tags, e.origin, e.created_at, e.original_created_at
FROM   memory_events e
WHERE  e.tenant_id = $1
  AND  e.actor_id = $2
  AND  e.embedding IS NOT NULL
  AND  e.kind = $6
  AND  e.op IN ('create','update')
  AND  NOT EXISTS (SELECT 1 FROM memory_events sup
                   WHERE sup.tenant_id = $1 AND sup.supersedes = e.entry_id)
  AND  NOT EXISTS (SELECT 1 FROM memory_events d
                   WHERE d.tenant_id = $1 AND d.op = 'delete' AND d.entry_id = e.entry_id)
  AND  1 - (e.embedding <=> $3) >= $4
ORDER BY e.embedding <=> $3
LIMIT $5`
	rows, err := s.db.QueryContext(ctx, q, s.tenant, actorFrom(ctx), pgVector(queryVec), floor, k, kind)
	if err != nil {
		return nil, fmt.Errorf("memory: search tenant %q: %w", s.tenant, err)
	}
	defer rows.Close()
	var out []hmem.Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, fmt.Errorf("memory: search scan tenant %q: %w", s.tenant, err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// GCOnce reaps dead (superseded or tombstoned) create/update rows older than
// grace, in ascending-seq batches of at most batch rows, looping until a pass
// deletes fewer than batch (backlog drained) or ctx is cancelled. Returns the
// total number of rows deleted. Tenant-scoped. The live set and every read path
// are unaffected (GC only deletes rows they already exclude).
func (s *Store) GCOnce(ctx context.Context, grace time.Duration, batch int) (int, error) {
	if batch < 1 {
		batch = 1
	}
	total := 0
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		cutoff := time.Now().UTC().Add(-grace)
		res, err := s.db.ExecContext(ctx, gcSweep, s.tenant, cutoff, batch)
		if err != nil {
			return total, fmt.Errorf("memory: gc sweep tenant %q: %w", s.tenant, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("memory: gc rows-affected tenant %q: %w", s.tenant, err)
		}
		total += int(n)
		if int(n) < batch {
			return total, nil
		}
	}
}

// StartGC runs GCOnce every interval until ctx is cancelled, in its own
// goroutine. Best-effort: a sweep error is logged and the loop continues (GC
// never crashes agentd, never blocks a turn). onReap (nil-safe) receives each
// successful pass's delete count for metrics.
func (s *Store) StartGC(ctx context.Context, interval, grace time.Duration, batch int, onReap func(int)) {
	sweep := func(c context.Context) (int, error) { return s.GCOnce(c, grace, batch) }
	go startGCLoop(ctx, interval, sweep, onReap)
}

// startGCLoop is the DB-free ticker body (test seam): tick → sweep → onReap,
// logging and continuing on error, returning when ctx is cancelled.
func startGCLoop(ctx context.Context, interval time.Duration, sweep func(context.Context) (int, error), onReap func(int)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := sweep(ctx)
			if err != nil {
				slog.Warn("memory: gc sweep failed", "err", err)
				continue
			}
			if onReap != nil {
				onReap(n)
			}
		}
	}
}
