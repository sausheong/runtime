# Memory M1 — Multi-tenant Postgres MemoryStore Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give hosted agents durable, per-tenant memory via a Postgres backend for harness's existing `tool/memory.MemoryStore`, exposed through harness's stock `MemoryTool`, opt-in per agent.

**Architecture:** A new `internal/memory` package holds a tenant-pinned `*Store` that implements `github.com/sausheong/harness/tool/memory.MemoryStore` over one append-only `memory_events` table, projecting the live set in SQL. The control plane injects the tenant (and a memory-enabled flag) into the agentd spawn env; an `agentkind` builder constructs the store (pinned to the agent's tenant) and adds the `MemoryTool` to the registry when the agent opts in. Harness is unmodified.

**Tech Stack:** Go 1.25.1, stdlib `database/sql` + pgx, Postgres (integration tests), module `github.com/sausheong/runtime` with `replace ../harness`.

**Spec:** `docs/superpowers/specs/2026-06-09-memory-m1-pg-memorystore-design.md`

---

## Conventions (read before starting)

- **`go` CLI is ground truth.** The IDE/LSP is broken by the `replace ../harness` cross-module setup — ignore its diagnostics; trust `go build ./...`, `go vet ./...`, `go test ./...`.
- **Hermetic tests** run via `go test ./...`. **Integration tests** are `//go:build integration` and need local Postgres.app at `postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable`. Run the integration packages **individually**, not via `./...`:
  - `go test -tags integration ./internal/memory/`
  - `go test -tags integration ./test/`
- **Commits** must use the project identity and trailer:
  ```bash
  git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' \
    commit -m "<message>

  Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
  ```
- **Branch:** all work lands on `feat/memory-m1-pg-memorystore` (already created; the spec commit is its first commit).
- **Never log or return memory content** in errors — op + tenant + id only.

## Key facts about the harness contract (verified against `../harness/tool/memory/`)

- `memory.MemoryStore` methods: `Save(ctx, Entry) (Entry, error)`, `Update(ctx, id, content string) (Entry, error)`, `Remove(ctx, id string) error`, `List(ctx, tag string) ([]Entry, error)`, `Get(ctx, id string) (Entry, bool, error)`.
- `memory.Entry{ID, Content string; Tags []string; CreatedAt, UpdatedAt time.Time; Origin string}`.
- Sentinels: `memory.ErrNotFound`, `memory.ErrInvalidContent`, `memory.ErrInvalidID`.
- **Origin:** the `MemoryTool` reads `memory.OriginKey` from context and passes `Origin` IN the `Entry` to `Save`. The STORE must persist `e.Origin` verbatim — it must NOT read `OriginKey` itself. (Spec's Save note is corrected here.)
- **Update semantics:** returns a FRESH id; old id becomes invalid; new entry's `CreatedAt` = original birth time, `UpdatedAt` = update time; tags+origin carried from the old entry. `ErrNotFound` if id unknown.
- **Remove:** idempotent; tombstone authoritative even with no matching create; returns nil.
- **List:** live entries, ordered by birth `CreatedAt`; `tag==""` ⇒ all, else entries whose `Tags` contains `tag`.
- **Get:** `(Entry, false, nil)` for unknown OR tombstoned (no error).
- Content validation (non-empty, ≤4000) lives in the TOOL; the store does not re-validate.

## File Structure

| File | Change | Responsibility |
|---|---|---|
| `internal/memory/schema.sql` | create | `memory_events` append-only table + indexes. |
| `internal/memory/id.go` | create | `generateID(now) string` → `mem_YYYY-MM-DD_<8hex>`. |
| `internal/memory/id_test.go` | create (hermetic) | ID shape + uniqueness. |
| `internal/memory/store.go` | create | `Store{db, tenant}` implementing `memory.MemoryStore`; `NewStore(ctx, db, tenant)`. SQL projection. |
| `internal/memory/store_test.go` | create (integration) | CRUD, supersede, tombstone, tag, origin, cross-tenant isolation, interface conformance. |
| `internal/config/config.go` | modify | `AgentConfig.Memory bool` (`yaml:"memory"`). |
| `internal/config/config_test.go` | modify/create | parse `memory: true`/absent. |
| `controlplane/registry.go` | modify | `AgentProcess.Memory bool`; populate from `AgentConfig`. |
| `controlplane/proxy.go` | modify | `buildEnv` appends `RUNTIME_AGENT_TENANT` always, `RUNTIME_AGENT_MEMORY=1` when enabled. |
| `controlplane/proxy_test.go` | modify | assert the two env vars; back-compat. |
| `internal/agentkind/registry.go` | modify | `Deps{Tenant string; Memory bool}`; helper wires `memory.Store`+`MemoryTool` when enabled. |
| `internal/agentkind/registry_test.go` | modify | builder includes memory tool when `Deps.Memory`. |
| `cmd/agentd/main.go` | modify | read `RUNTIME_AGENT_TENANT` (default `default`) + `RUNTIME_AGENT_MEMORY`; pass into `Deps`. |
| `test/memory_e2e_test.go` | create (integration) | end-to-end per-tenant memory across the real spawn path. |
| `README.md`, `ROADMAP.md`, `runtime.yaml`, `docs/images/project-layout.mmd` | docs | document the feature. |

**Ordering rationale:** T1–T2 build the standalone store (no runtime deps). T3 adds the config flag. T4 injects env (control plane). T5 wires agentkind. T6 threads agentd. T7 is the E2E. T8 docs. Each task leaves the tree green.

---

### Task 1: memory_events schema + ID generator

**Files:**
- Create: `internal/memory/schema.sql`
- Create: `internal/memory/id.go`
- Create: `internal/memory/id_test.go`

- [ ] **Step 1: Write the schema file**

Create `internal/memory/schema.sql`:

```sql
CREATE TABLE IF NOT EXISTS memory_events (
    seq                 BIGSERIAL PRIMARY KEY,
    tenant_id           TEXT NOT NULL,
    op                  TEXT NOT NULL CHECK (op IN ('create','update','delete')),
    entry_id            TEXT NOT NULL,
    content             TEXT,
    tags                TEXT[],
    origin              TEXT NOT NULL DEFAULT '',
    supersedes          TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    original_created_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS memory_events_tenant_idx ON memory_events (tenant_id);
CREATE INDEX IF NOT EXISTS memory_events_supersedes_idx ON memory_events (tenant_id, supersedes);
```

- [ ] **Step 2: Write the failing ID test**

Create `internal/memory/id_test.go`:

```go
package memory

import (
	"regexp"
	"testing"
	"time"
)

var idRe = regexp.MustCompile(`^mem_\d{4}-\d{2}-\d{2}_[0-9a-f]{8}$`)

func TestGenerateID_Shape(t *testing.T) {
	id := generateID(time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC))
	if !idRe.MatchString(id) {
		t.Fatalf("id %q does not match %s", id, idRe)
	}
}

func TestGenerateID_Unique(t *testing.T) {
	now := time.Now()
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := generateID(now)
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}
```

- [ ] **Step 3: Run to verify failure**

Run: `go test ./internal/memory/`
Expected: FAIL — `undefined: generateID` (or "no Go files" until id.go exists).

- [ ] **Step 4: Implement the ID generator**

Create `internal/memory/id.go`:

```go
// Package memory is a multi-tenant Postgres backend for harness's
// tool/memory.MemoryStore. One append-only memory_events table holds the
// create/update/delete event log; reads project the live set in SQL. Each Store
// instance is pinned to a tenant at construction; every query filters by it.
package memory

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// generateID returns a sortable, collision-resistant id of the form
// mem_YYYY-MM-DD_<8-char-hex>, matching the JSONL reference backend's scheme.
func generateID(now time.Time) string {
	var buf [4]byte
	_, _ = rand.Read(buf[:])
	return fmt.Sprintf("mem_%s_%s", now.UTC().Format("2006-01-02"), hex.EncodeToString(buf[:]))
}
```

- [ ] **Step 5: Run to verify pass**

Run: `go test ./internal/memory/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/memory/schema.sql internal/memory/id.go internal/memory/id_test.go
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "feat(memory): memory_events schema + id generator

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: The tenant-pinned Store (MemoryStore impl)

**Files:**
- Create: `internal/memory/store.go`
- Create: `internal/memory/store_test.go` (integration)

This is the substance. `Store` implements `memory.MemoryStore`, pinned to a tenant, projecting the live set in SQL.

- [ ] **Step 1: Write the integration tests first**

Create `internal/memory/store_test.go`:

```go
//go:build integration

package memory

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	hmem "github.com/sausheong/harness/tool/memory"
)

const dsn = "postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable"

// freshStore opens the DB, drops + recreates memory_events, and returns a Store
// pinned to tenant. t.Cleanup drops the table so sibling tests don't see it.
func freshStore(t *testing.T, tenant string) (*Store, *sql.DB) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Ping(); err != nil {
		t.Skipf("postgres not reachable: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE IF EXISTS memory_events CASCADE`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DROP TABLE IF EXISTS memory_events CASCADE`) })
	st, err := NewStore(context.Background(), db, tenant)
	if err != nil {
		t.Fatal(err)
	}
	return st, db
}

// compile-time proof the Store satisfies the harness interface.
var _ hmem.MemoryStore = (*Store)(nil)

func TestStore_SaveGetRoundTrip(t *testing.T) {
	st, db := freshStore(t, "alpha")
	defer db.Close()
	ctx := context.Background()

	saved, err := st.Save(ctx, hmem.Entry{Content: "hello", Tags: []string{"x"}, Origin: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	if saved.ID == "" || saved.Content != "hello" {
		t.Fatalf("bad saved entry: %+v", saved)
	}
	if !saved.CreatedAt.Equal(saved.UpdatedAt) {
		t.Fatalf("fresh entry CreatedAt != UpdatedAt: %+v", saved)
	}
	got, ok, err := st.Get(ctx, saved.ID)
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.Content != "hello" || got.Origin != "agent" || len(got.Tags) != 1 || got.Tags[0] != "x" {
		t.Fatalf("get mismatch: %+v", got)
	}
}

func TestStore_ListOrderingAndTagFilter(t *testing.T) {
	st, db := freshStore(t, "alpha")
	defer db.Close()
	ctx := context.Background()
	a, _ := st.Save(ctx, hmem.Entry{Content: "first", Tags: []string{"k"}})
	b, _ := st.Save(ctx, hmem.Entry{Content: "second"})
	all, err := st.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].ID != a.ID || all[1].ID != b.ID {
		t.Fatalf("list order wrong: %+v", all)
	}
	tagged, _ := st.List(ctx, "k")
	if len(tagged) != 1 || tagged[0].ID != a.ID {
		t.Fatalf("tag filter wrong: %+v", tagged)
	}
}

func TestStore_UpdateSupersedes(t *testing.T) {
	st, db := freshStore(t, "alpha")
	defer db.Close()
	ctx := context.Background()
	orig, _ := st.Save(ctx, hmem.Entry{Content: "v1", Tags: []string{"t"}, Origin: "agent"})
	upd, err := st.Update(ctx, orig.ID, "v2")
	if err != nil {
		t.Fatal(err)
	}
	if upd.ID == orig.ID {
		t.Fatal("Update must mint a fresh id")
	}
	if !upd.CreatedAt.Equal(orig.CreatedAt) {
		t.Fatalf("Update must preserve birth CreatedAt: orig=%v new=%v", orig.CreatedAt, upd.CreatedAt)
	}
	if !upd.UpdatedAt.After(orig.UpdatedAt) && !upd.UpdatedAt.Equal(orig.UpdatedAt) {
		// UpdatedAt is the update write time; must be >= original.
		t.Fatalf("UpdatedAt not advanced: %v", upd.UpdatedAt)
	}
	if len(upd.Tags) != 1 || upd.Tags[0] != "t" || upd.Origin != "agent" {
		t.Fatalf("Update must carry tags+origin: %+v", upd)
	}
	// old id invalid:
	if _, ok, _ := st.Get(ctx, orig.ID); ok {
		t.Fatal("old id must be invalid after update")
	}
	// new id live, exactly one live row:
	all, _ := st.List(ctx, "")
	if len(all) != 1 || all[0].ID != upd.ID || all[0].Content != "v2" {
		t.Fatalf("after update want one live row v2: %+v", all)
	}
	// Update on unknown id:
	if _, err := st.Update(ctx, "mem_2000-01-01_deadbeef", "x"); err != hmem.ErrNotFound {
		t.Fatalf("update unknown id: want ErrNotFound, got %v", err)
	}
}

func TestStore_RemoveTombstone(t *testing.T) {
	st, db := freshStore(t, "alpha")
	defer db.Close()
	ctx := context.Background()
	e, _ := st.Save(ctx, hmem.Entry{Content: "doomed"})
	if err := st.Remove(ctx, e.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.Get(ctx, e.ID); ok {
		t.Fatal("removed entry must be gone")
	}
	all, _ := st.List(ctx, "")
	if len(all) != 0 {
		t.Fatalf("list after remove must be empty: %+v", all)
	}
	// idempotent on unknown id:
	if err := st.Remove(ctx, "mem_2000-01-01_deadbeef"); err != nil {
		t.Fatalf("remove unknown id must be nil: %v", err)
	}
}

func TestStore_OriginPersistedVerbatim(t *testing.T) {
	st, db := freshStore(t, "alpha")
	defer db.Close()
	ctx := context.Background()
	e, _ := st.Save(ctx, hmem.Entry{Content: "r", Origin: "review"})
	got, _, _ := st.Get(ctx, e.ID)
	if got.Origin != "review" {
		t.Fatalf("origin not persisted verbatim: %q", got.Origin)
	}
}

func TestStore_CrossTenantIsolation(t *testing.T) {
	alpha, db := freshStore(t, "alpha")
	defer db.Close()
	// beta shares the same DB/table but is pinned to a different tenant.
	beta, err := NewStore(context.Background(), db, "beta")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ae, _ := alpha.Save(ctx, hmem.Entry{Content: "alpha-secret"})

	if list, _ := beta.List(ctx, ""); len(list) != 0 {
		t.Fatalf("beta must not see alpha's entries: %+v", list)
	}
	if _, ok, _ := beta.Get(ctx, ae.ID); ok {
		t.Fatal("beta must not Get alpha's entry")
	}
	if _, err := beta.Update(ctx, ae.ID, "hijack"); err != hmem.ErrNotFound {
		t.Fatalf("beta update of alpha id: want ErrNotFound, got %v", err)
	}
	if err := beta.Remove(ctx, ae.ID); err != nil {
		t.Fatalf("beta remove of alpha id must no-op nil: %v", err)
	}
	// alpha still has its entry intact:
	if _, ok, _ := alpha.Get(ctx, ae.ID); !ok {
		t.Fatal("alpha lost its entry after beta's no-op remove")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test -tags integration ./internal/memory/`
Expected: FAIL — `undefined: NewStore` / `undefined: Store`.

- [ ] **Step 3: Implement the Store**

Create `internal/memory/store.go`:

```go
package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "embed"

	"github.com/lib/pq"
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

// liveSelect is the projection: a defining (create|update) row for an entry_id
// that is neither superseded by an update nor tombstoned by a delete, within the
// pinned tenant.
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
		tags     pq.StringArray
		created  time.Time
		original sql.NullTime
	)
	if err := rows.Scan(&e.ID, &e.Content, &tags, &e.Origin, &created, &original); err != nil {
		return hmem.Entry{}, err
	}
	e.Tags = []string(tags)
	e.UpdatedAt = created
	if original.Valid {
		e.CreatedAt = original.Time
	} else {
		e.CreatedAt = created
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
		s.tenant, e.ID, e.Content, pq.StringArray(e.Tags), e.Origin, now)
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
		s.tenant, newID, content, pq.StringArray(old.Tags), old.Origin, id, now, old.CreatedAt)
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

var _ = errors.Is // reserved: future sentinel wrapping
```

> **Dependency note:** this uses `github.com/lib/pq` only for `pq.StringArray` (TEXT[] scanning/binding). Check `go.mod` first: if `lib/pq` is not already a dependency, AVOID adding it — instead scan the array with the pgx-native path. Specifically, if `lib/pq` is absent, replace `pq.StringArray` usage as follows: for binding use `pq.Array`... NO — to avoid a new dep entirely, prefer the pgx stdlib array support. Determine in Step 3a below.

- [ ] **Step 3a: Resolve the TEXT[] array dependency BEFORE finalizing store.go**

Run: `grep -E 'lib/pq|jackc/pgx' go.mod`

- If `github.com/lib/pq` is **already present**: keep the `pq.StringArray` code above as-is, and ensure the import is `"github.com/lib/pq"`.
- If `lib/pq` is **absent** (only pgx present): do NOT add lib/pq. Instead, change the three array sites to use pgx's array support via the `database/sql` driver. Replace `import "github.com/lib/pq"` with `"github.com/jackc/pgx/v5/pgtype"` is heavier — simplest: store tags as a Postgres array using pgx's `pgtype` is overkill. Use this minimal approach instead: bind tags with `pq.Array` is unavailable, so encode the array yourself is error-prone.

  **Decision rule (deterministic):** the runtime already imports `github.com/jackc/pgx/v5/stdlib` (it's the `database/sql` driver, per `go.mod`). pgx's stdlib driver supports scanning/binding Go `[]string` to Postgres `text[]` **directly** when wrapped — but the portable, dependency-free path that works under the `pgx/v5/stdlib` `database/sql` driver is to use `pgx`'s array type. To keep this plan deterministic and dependency-light, implement tags with the helper below and DROP the `lib/pq` import:

  Replace the bind sites `pq.StringArray(e.Tags)` / `pq.StringArray(old.Tags)` with `pgArray(e.Tags)` / `pgArray(old.Tags)`, replace the scan target `var tags pq.StringArray` with `var tags []string` plus a custom scan, and add this helper file.

  Create `internal/memory/array.go`:

```go
package memory

import (
	"database/sql/driver"
	"fmt"
	"strings"
)

// textArray adapts a Go []string to a Postgres text[] for both binding (Value)
// and scanning (Scan), so the package needs no third-party array dependency
// beyond the pgx stdlib driver already in use.
type textArray []string

// Value renders the slice as a Postgres array literal. Elements are quoted and
// internal quotes/backslashes escaped. nil/empty ⇒ empty array literal.
func (a textArray) Value() (driver.Value, error) {
	if len(a) == 0 {
		return "{}", nil
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, s := range a {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('"')
		b.WriteString(strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String(), nil
}

// Scan parses a Postgres text[] rendering ({a,b,"c d"}) back into a []string.
func (a *textArray) Scan(src any) error {
	if src == nil {
		*a = nil
		return nil
	}
	var s string
	switch v := src.(type) {
	case string:
		s = v
	case []byte:
		s = string(v)
	default:
		return fmt.Errorf("textArray: unsupported scan type %T", src)
	}
	*a = parsePGTextArray(s)
	return nil
}

// parsePGTextArray parses {a,b,"c, d"} into elements, handling quotes and
// backslash escapes. Returns nil for the empty array.
func parsePGTextArray(s string) []string {
	s = strings.TrimSpace(s)
	if len(s) < 2 || s[0] != '{' || s[len(s)-1] != '}' {
		return nil
	}
	inner := s[1 : len(s)-1]
	if inner == "" {
		return nil
	}
	var out []string
	var cur strings.Builder
	inQuotes := false
	escaped := false
	flush := func() { out = append(out, cur.String()); cur.Reset() }
	for i := 0; i < len(inner); i++ {
		c := inner[i]
		switch {
		case escaped:
			cur.WriteByte(c)
			escaped = false
		case c == '\\':
			escaped = true
		case c == '"':
			inQuotes = !inQuotes
		case c == ',' && !inQuotes:
			flush()
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return out
}
```

  Then in `store.go`: remove the `"github.com/lib/pq"` import; change `var tags pq.StringArray` → `var tags textArray`; change `e.Tags = []string(tags)` (keep, since `textArray` is `[]string`); change all `pq.StringArray(x)` → `textArray(x)`.

> Rationale for this detour: the project standard is "no build scripts / minimal deps," and adding `lib/pq` alongside `pgx` is redundant. The `textArray` adapter is ~50 lines, fully unit-testable, and removes the third-party array dependency. **If `grep` shows `lib/pq` is already a direct dependency, skip `array.go` and use `pq.StringArray` — don't add code the repo doesn't need.**

- [ ] **Step 3b: Add an array round-trip unit test (only if you created array.go)**

Create `internal/memory/array_test.go` (hermetic):

```go
package memory

import (
	"reflect"
	"testing"
)

func TestTextArray_RoundTrip(t *testing.T) {
	cases := [][]string{
		nil,
		{},
		{"a"},
		{"a", "b", "c"},
		{"has space", `has"quote`, `has\backslash`, "has,comma"},
	}
	for _, in := range cases {
		v, err := textArray(in).Value()
		if err != nil {
			t.Fatalf("value(%v): %v", in, err)
		}
		var out textArray
		if err := out.Scan(v.(string)); err != nil {
			t.Fatalf("scan(%q): %v", v, err)
		}
		want := in
		if len(in) == 0 {
			want = nil
		}
		if !reflect.DeepEqual([]string(out), want) {
			t.Fatalf("round-trip: in=%v got=%v", in, []string(out))
		}
	}
}
```

- [ ] **Step 4: Run hermetic + integration**

Run: `go test ./internal/memory/` (hermetic: id + array tests) → PASS.
Run: `go test -tags integration ./internal/memory/` → PASS (all store tests, incl. cross-tenant isolation). If Postgres is unreachable, the tests `t.Skip` — start Postgres.app and retry.

- [ ] **Step 5: Build + vet**

Run: `go build ./... && go vet ./...`
Expected: clean.

- [ ] **Step 6: Commit**

```bash
git add internal/memory/
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "feat(memory): tenant-pinned Postgres MemoryStore with SQL projection

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: AgentConfig.Memory flag

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go` (create if absent)

- [ ] **Step 1: Write/extend the config test**

Add to `internal/config/config_test.go` (create the file with `package config` + imports if it doesn't exist):

```go
func TestLoad_MemoryFlag(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime.yaml")
	body := `agents:
  - id: a1
    name: A1
    model: test/scripted
    listen_addr: "127.0.0.1:9101"
    memory: true
  - id: a2
    name: A2
    model: test/scripted
    listen_addr: "127.0.0.1:9102"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Agents[0].Memory {
		t.Fatal("agent a1 should have memory enabled")
	}
	if cfg.Agents[1].Memory {
		t.Fatal("agent a2 should default memory to false")
	}
}
```

Ensure the test file imports `os`, `path/filepath`, `testing`.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/config/ -run TestLoad_MemoryFlag`
Expected: FAIL — `Agents[0].Memory undefined`.

- [ ] **Step 3: Add the field**

In `internal/config/config.go`, add to `AgentConfig` (after `Tenant`):

```go
	Memory     bool     `yaml:"memory"`  // optional; opt-in to the per-tenant Postgres memory tool. Default false.
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/config/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "feat(config): AgentConfig.Memory opt-in flag

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 4: Inject tenant + memory flag into the spawn env

**Files:**
- Modify: `controlplane/registry.go`
- Modify: `controlplane/proxy.go`
- Modify: `controlplane/proxy_test.go`

Today `AgentProcess` already carries `Tenant`; `buildEnv` injects `RUNTIME_AGENT_ID/KIND/PG_DSN/LISTEN_ADDR` + secrets but NOT the tenant. Add `RUNTIME_AGENT_TENANT` (always) and `RUNTIME_AGENT_MEMORY=1` (when the agent opts in).

- [ ] **Step 1: Extend the proxy env test**

In `controlplane/proxy_test.go`, add a test (adapt to the file's existing helpers for building an `AgentProcess` + calling `buildEnv`; read the file first to match style):

```go
func TestBuildEnv_InjectsTenantAndMemory(t *testing.T) {
	ap := AgentProcess{AgentID: "a1", Addr: "127.0.0.1:9111", PGDSN: "dsn", Tenant: "alpha", Memory: true}
	env, err := ap.buildEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "RUNTIME_AGENT_TENANT=alpha") {
		t.Fatalf("missing tenant in env:\n%s", joined)
	}
	if !strings.Contains(joined, "RUNTIME_AGENT_MEMORY=1") {
		t.Fatalf("missing memory flag in env:\n%s", joined)
	}
}

func TestBuildEnv_NoMemoryFlagWhenDisabled(t *testing.T) {
	ap := AgentProcess{AgentID: "a1", Addr: "127.0.0.1:9111", PGDSN: "dsn", Tenant: "alpha"}
	env, _ := ap.buildEnv(context.Background())
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "RUNTIME_AGENT_MEMORY") {
		t.Fatalf("memory flag must be absent when disabled:\n%s", joined)
	}
	if !strings.Contains(joined, "RUNTIME_AGENT_TENANT=alpha") {
		t.Fatalf("tenant must always be present:\n%s", joined)
	}
}
```

Ensure `proxy_test.go` imports `context` and `strings`.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./controlplane/ -run TestBuildEnv`
Expected: FAIL — `AgentProcess.Memory undefined` (and the env assertions).

- [ ] **Step 3: Add the Memory field to AgentProcess**

In `controlplane/proxy.go`, add to the `AgentProcess` struct (after `Tenant`):

```go
	Memory  bool   // opt-in: when true, the spawn env carries RUNTIME_AGENT_MEMORY=1 so agentd wires the memory tool.
```

- [ ] **Step 4: Inject the env vars in buildEnv**

In `controlplane/proxy.go`, in `buildEnv`, change the base env append to include the tenant, and add the memory flag. Replace:

```go
	env := append(os.Environ(),
		"RUNTIME_PG_DSN="+a.PGDSN,
		"RUNTIME_LISTEN_ADDR="+a.Addr,
		"RUNTIME_AGENT_ID="+a.AgentID,
		"RUNTIME_AGENT_KIND="+a.Kind,
	)
```

with:

```go
	env := append(os.Environ(),
		"RUNTIME_PG_DSN="+a.PGDSN,
		"RUNTIME_LISTEN_ADDR="+a.Addr,
		"RUNTIME_AGENT_ID="+a.AgentID,
		"RUNTIME_AGENT_KIND="+a.Kind,
		"RUNTIME_AGENT_TENANT="+a.Tenant,
	)
	if a.Memory {
		env = append(env, "RUNTIME_AGENT_MEMORY=1")
	}
```

(The secrets-brokering block stays exactly as-is, after this.)

- [ ] **Step 5: Populate Memory in the registry**

In `controlplane/registry.go`, in `NewRegistry`, where the `AgentProcess` is built from `AgentConfig`, add `Memory: a.Memory` to the struct literal. Find:

```go
		r.agents[a.ID] = AgentProcess{
			AgentID: a.ID, Addr: a.ListenAddr, BinPath: binPath, PGDSN: dsn,
			Kind: a.Kind, Command: a.Command, WorkDir: a.WorkDir, Tenant: a.Tenant,
		}
```

and add `Memory: a.Memory,` to it.

- [ ] **Step 6: Run to verify pass + full hermetic**

Run: `go test ./controlplane/ -run TestBuildEnv` → PASS.
Run: `go build ./... && go vet ./... && go test ./...` → PASS.

- [ ] **Step 7: Commit**

```bash
git add controlplane/proxy.go controlplane/registry.go controlplane/proxy_test.go
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "feat(controlplane): inject RUNTIME_AGENT_TENANT + RUNTIME_AGENT_MEMORY into spawn env

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 5: Wire the memory tool in agentkind when enabled

**Files:**
- Modify: `internal/agentkind/registry.go`
- Modify: `internal/agentkind/registry_test.go`

`Deps` gains `Tenant string` and `Memory bool`. A shared helper adds the memory tool to a registry when enabled. The test agent builder uses it so the wiring is testable without a real agentd.

- [ ] **Step 1: Write the failing builder test**

In `internal/agentkind/registry_test.go`, add (read the file first; it currently tests `Get`):

```go
func TestBuildTestAgent_AddsMemoryToolWhenEnabled(t *testing.T) {
	build, _ := Get("testagent")
	cfg, err := build(Deps{AgentID: "a1", Memory: true, Tenant: "alpha", DB: nil})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Tools.Get("memory"); !ok {
		t.Fatal("memory tool should be registered when Deps.Memory is true")
	}
}

func TestBuildTestAgent_NoMemoryToolByDefault(t *testing.T) {
	build, _ := Get("testagent")
	cfg, err := build(Deps{AgentID: "a1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Tools.Get("memory"); ok {
		t.Fatal("memory tool must be absent when Deps.Memory is false")
	}
}
```

> Verify the registry getter method name first: `grep -n "func (r \*Registry) Get" ../harness/tool/tool.go`. The harness `tool.Registry` exposes a lookup; if it is not named `Get`, adapt the two assertions to the actual method (e.g. iterate `cfg.Tools` or use the real accessor). Do not invent a method.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/agentkind/`
Expected: FAIL — `Deps.Memory undefined` / `Deps.Tenant undefined`.

- [ ] **Step 3: Extend Deps and add the wiring helper**

In `internal/agentkind/registry.go`:

(a) Add fields to `Deps`:

```go
	Tenant string // the agent's tenant; used to pin the memory store. "" ⇒ "default".
	Memory bool   // when true, attach the per-tenant Postgres memory tool.
```

(b) Add imports: `"context"`, `hmemory "github.com/sausheong/harness/tool/memory"`, `"github.com/sausheong/runtime/internal/memory"`.

(c) Add a helper that attaches the memory tool to a registry when enabled:

```go
// attachMemory adds the per-tenant Postgres memory tool to reg when d.Memory is
// set. Requires d.DB (the shared Postgres handle). Returns an error if the store
// cannot be constructed — an agent that asked for memory must not start without
// it.
func attachMemory(reg *tool.Registry, d Deps) error {
	if !d.Memory {
		return nil
	}
	if d.DB == nil {
		return fmt.Errorf("agentkind: memory enabled for %q but no DB handle", d.AgentID)
	}
	st, err := memory.NewStore(context.Background(), d.DB, d.Tenant)
	if err != nil {
		return fmt.Errorf("agentkind: memory store for %q: %w", d.AgentID, err)
	}
	reg.Register(&hmemory.MemoryTool{Store: st})
	return nil
}
```

Add `"fmt"` to the imports if not present.

(d) Call it in `buildTestAgent` (so it's exercised) — after `reg.Register(testagent.MarkerTool{DB: d.DB})`:

```go
	if err := attachMemory(reg, d); err != nil {
		return agentruntime.Config{}, err
	}
```

(e) Also call it in `buildNutrition` so memory works for a real agent kind too. Change `buildNutrition` to:

```go
func buildNutrition(d Deps) (agentruntime.Config, error) {
	cfg, err := nutrition.BuildConfig(nutrition.Deps{AgentID: d.AgentID})
	if err != nil {
		return agentruntime.Config{}, err
	}
	if err := attachMemory(cfg.Tools, d); err != nil {
		return agentruntime.Config{}, err
	}
	return cfg, nil
}
```

> Verify `agentruntime.Config` exposes `Tools` as a `*tool.Registry` (it does, per `agentruntime/config.go`). If `cfg.Tools` is nil for the nutrition kind, guard with a nil check and skip attaching (memory simply won't be available) — but the field is a registry the builder populated, so it should be non-nil.

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/agentkind/`
Expected: PASS.

- [ ] **Step 5: Build + vet + full hermetic**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/agentkind/registry.go internal/agentkind/registry_test.go
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "feat(agentkind): attach per-tenant memory tool when enabled

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 6: Thread tenant + memory flag through agentd

**Files:**
- Modify: `cmd/agentd/main.go`

- [ ] **Step 1: Read RUNTIME_AGENT_TENANT + RUNTIME_AGENT_MEMORY and pass into Deps**

In `cmd/agentd/main.go`, after the existing `kind := os.Getenv("RUNTIME_AGENT_KIND")` line, add:

```go
	tenant := os.Getenv("RUNTIME_AGENT_TENANT") // "" ⇒ memory.NewStore defaults to "default"
	memoryOn := os.Getenv("RUNTIME_AGENT_MEMORY") == "1"
```

Then change the builder call from:

```go
	cfg, err := build(agentkind.Deps{AgentID: agentID, DB: db})
```

to:

```go
	cfg, err := build(agentkind.Deps{AgentID: agentID, DB: db, Tenant: tenant, Memory: memoryOn})
```

- [ ] **Step 2: Build + vet**

Run: `go build ./... && go vet ./...`
Expected: clean. (No new unit test here — agentd is a thin wiring main; the E2E in Task 7 exercises it.)

- [ ] **Step 3: Commit**

```bash
git add cmd/agentd/main.go
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "feat(agentd): pass tenant + memory flag into the kind builder

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 7: End-to-end — per-tenant memory across the spawn path

**Files:**
- Create: `test/memory_e2e_test.go`

This proves the chain WITHOUT a live LLM: it builds the registry + `memory.Store` the way the platform does, spawns a `command:` agent (the secrets-E2E pattern) to prove tenant injection reaches the subprocess env, and drives the `memory.MemoryStore` directly (per-tenant `Store` instances over the real DB) to prove isolation end-to-end. The store-level isolation is the security property; the spawn-env assertion proves the platform plumbs the tenant to agentd.

> Why not drive the LLM tool call: the bundled `testagent.Scripted` provider only scripts a "marker" tool call, and adding memory-action scripting to it is out of this milestone's scope. Driving the `Store` directly over the same Postgres the agent would use is the same correctness guarantee for memory, and the `command:`-spawn assertion covers the platform plumbing.

- [ ] **Step 1: Write the E2E test**

Create `test/memory_e2e_test.go`:

```go
//go:build integration

package test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	hmem "github.com/sausheong/harness/tool/memory"

	"github.com/sausheong/runtime/controlplane"
	"github.com/sausheong/runtime/internal/config"
	"github.com/sausheong/runtime/internal/memory"
)

func TestMemoryE2E_PerTenantIsolationAndEnvInjection(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Skipf("postgres not reachable: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE IF EXISTS memory_events CASCADE`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DROP TABLE IF EXISTS memory_events CASCADE`) })

	// --- Part A: store-level per-tenant isolation over the real DB ---
	alpha, err := memory.NewStore(ctx, db, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	beta, err := memory.NewStore(ctx, db, "beta")
	if err != nil {
		t.Fatal(err)
	}
	a1, _ := alpha.Save(ctx, hmem.Entry{Content: "alpha-fact", Origin: "agent"})
	a2, _ := alpha.Save(ctx, hmem.Entry{Content: "alpha-fact-2", Origin: "agent"})
	_ = a2
	if _, _ = beta.Save(ctx, hmem.Entry{Content: "beta-fact", Origin: "agent"}); false {
	}

	// Two agents in the SAME tenant share the pool: a second alpha store sees a1.
	alpha2, _ := memory.NewStore(ctx, db, "alpha")
	if got, _, _ := alpha2.Get(ctx, a1.ID); got.Content != "alpha-fact" {
		t.Fatalf("same-tenant agents must share the pool; got %+v", got)
	}
	// Different tenant cannot see it.
	if _, ok, _ := beta.Get(ctx, a1.ID); ok {
		t.Fatal("cross-tenant leak: beta saw alpha's entry")
	}
	la, _ := alpha.List(ctx, "")
	lb, _ := beta.List(ctx, "")
	if len(la) != 2 {
		t.Fatalf("alpha pool size = %d, want 2", len(la))
	}
	if len(lb) != 1 || lb[0].Content != "beta-fact" {
		t.Fatalf("beta pool wrong: %+v", lb)
	}

	// --- Part B: the platform injects RUNTIME_AGENT_TENANT + _MEMORY into spawn ---
	dir := t.TempDir()
	out := filepath.Join(dir, "env.txt")
	cfg := &config.Config{Agents: []config.AgentConfig{{
		ID:         "agent-alpha",
		ListenAddr: "127.0.0.1:0",
		Tenant:     "alpha",
		Memory:     true,
		Command:    []string{"sh", "-c", "env > " + out},
	}}}
	reg := controlplane.NewRegistry(cfg, "./agentd", dsn)
	ap, ok := reg.Get("agent-alpha")
	if !ok {
		t.Fatal("agent-alpha not found")
	}
	env := spawnAndWaitEnv(t, ap, out) // reused from secrets_e2e_test.go
	if !strings.Contains(env, "RUNTIME_AGENT_TENANT=alpha") {
		t.Fatalf("spawn env missing tenant:\n%s", env)
	}
	if !strings.Contains(env, "RUNTIME_AGENT_MEMORY=1") {
		t.Fatalf("spawn env missing memory flag:\n%s", env)
	}
	_ = os.Environ
}
```

> Note: `spawnAndWaitEnv`, `dsn` are defined in `test/secrets_e2e_test.go` / `test/resume_test.go` (same package `test`), so reuse them — do not redefine. If the compiler reports a redefinition or missing helper, read those files and adapt (use the existing helper names).

- [ ] **Step 2: Run the E2E**

Run: `go test -tags integration ./test/ -run TestMemoryE2E`
Expected: PASS (Postgres reachable; otherwise it Skips on Ping).

- [ ] **Step 3: Run the whole integration package (no cross-pollution)**

Run: `go test -tags integration ./test/`
Expected: PASS — all E2E tests including the secrets + identity + resume siblings. (Memory test self-cleans `memory_events`; it shares no tables with the others.)

- [ ] **Step 4: Commit**

```bash
git add test/memory_e2e_test.go
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "test(e2e): per-tenant memory isolation + spawn-env injection

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 8: Documentation

**Files:**
- Modify: `README.md`
- Modify: `ROADMAP.md`
- Modify: `runtime.yaml`
- Modify: `docs/images/project-layout.mmd` (+ regenerate `.png` if `mmdc` present)

- [ ] **Step 1: README — add a memory subsection**

In `README.md`, after the "Per-tenant secrets" section (search for `### Per-tenant secrets`), add a new subsection:

```markdown
### Per-tenant agent memory

An agent can opt into durable, per-tenant memory: set `memory: true` on its
`runtime.yaml` entry and it gets harness's `memory` tool
(`save`/`update`/`remove`/`list`/`get`), backed by Postgres. All agents owned by
a tenant share one memory pool; a tenant can never read another tenant's memory.
Entries survive restarts and are shared across the tenant's agents.

```yaml
agents:
  - id: assistant
    name: Assistant
    model: anthropic/claude-sonnet-4-6
    listen_addr: "127.0.0.1:8081"
    tenant: acme
    memory: true        # opt in to durable per-tenant memory
```

Retrieval in this milestone is by tag and by id (the durable store). Semantic
recall (embeddings + vector search) is a planned follow-up via harness's
`KnowledgeGraph` seam. Memory is disabled by default; agents without the flag are
unaffected. The platform injects `RUNTIME_AGENT_TENANT` (and, when enabled,
`RUNTIME_AGENT_MEMORY=1`) into the agent subprocess; agentd constructs a
tenant-pinned store so memory is isolated by construction.
```

- [ ] **Step 2: README — env-var table**

In the README env-var table, add two rows (agentd-side, injected by the platform):

```markdown
| `RUNTIME_AGENT_TENANT` | agentd | `default` | The agent's tenant, injected by the control plane; pins the memory store's isolation. |
| `RUNTIME_AGENT_MEMORY` | agentd | (unset) | `1` when the agent opted into memory (`memory: true`); tells agentd to wire the memory tool. |
```

- [ ] **Step 3: runtime.yaml example**

In `runtime.yaml`, add `memory: true` to ONE existing agent entry (pick the first), as a documented example. Add an inline comment `# durable per-tenant memory`. Do not change other fields.

- [ ] **Step 4: ROADMAP — mark B2 M1 done**

In `ROADMAP.md`:
(a) Update the `**Current state:**` header to mention Memory's first milestone.
(b) In §B2 (Memory), add a "First milestone DONE" note:

```markdown
   **First milestone DONE (merged to `master`, 2026-06-09):** multi-tenant
   durable memory. A Postgres backend (`internal/memory`) implements harness's
   `tool/memory.MemoryStore` over an append-only `memory_events` table with a
   SQL live-set projection; agents opt in with `memory: true` in `runtime.yaml`
   and get harness's stock `memory` tool. Per-tenant pool (shared across a
   tenant's agents), isolated by construction (the store is pinned to its tenant;
   the platform injects `RUNTIME_AGENT_TENANT`). Tag/id retrieval only — semantic
   recall via harness's `KnowledgeGraph` seam is the next milestone. Spec/plan:
   `docs/superpowers/{specs,plans}/2026-06-09-memory-m1-pg-memorystore*`.
```
(c) Note remaining B2 work: semantic/vector recall, compaction/TTL, finer scoping.

- [ ] **Step 5: Layout diagram**

In `docs/images/project-layout.mmd`, add a node under `internal/` for memory, mirroring the `store`/`ident` node style:

```
    mem["memory/<br/><i>per-tenant Postgres MemoryStore<br/>(append-only events + SQL projection)</i>"]
```
and an edge `internal --> mem`, and add `mem` to the `leaf` classDef line.

If `mmdc` is on PATH, regenerate:
```bash
command -v mmdc >/dev/null 2>&1 && mmdc -i docs/images/project-layout.mmd -o docs/images/project-layout.png -t neutral -b white -s 3 || echo "mmdc not available; skipping PNG regen"
```

- [ ] **Step 6: Final full verification**

Run:
```bash
go build ./... && go vet ./... && go test ./...
go test -tags integration ./internal/memory/
go test -tags integration ./test/
```
Expected: all PASS.

- [ ] **Step 7: Commit**

```bash
git add README.md ROADMAP.md runtime.yaml docs/images/project-layout.mmd docs/images/project-layout.png
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "docs(memory): document per-tenant agent memory (README, ROADMAP, layout, example)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

(The project memory file lives outside the repo; the orchestrator updates it separately.)

---

## Final review (after all tasks)

Dispatch a holistic reviewer over `git diff master...HEAD`. Focus, given this milestone's shape:

- **Isolation by construction** — confirm EVERY query in `store.go` filters by `s.tenant`, and there is no method/path that accepts a tenant from the caller. The cross-tenant test must genuinely exercise two pinned stores over one DB.
- **Projection correctness** — the three-clause liveness (defining row, not superseded, not tombstoned) and the `CreatedAt`=birth / `UpdatedAt`=defining-row-time mapping match the JSONL reference exactly. Check `Get`'s `ORDER BY seq DESC LIMIT 1` handles the duplicate-id non-path without returning a stale row.
- **TEXT[] handling** — whichever array path was chosen (lib/pq if already present, else the `textArray` adapter), confirm round-trip with quotes/commas/backslashes and empty/nil.
- **Contract conformance** — `var _ memory.MemoryStore = (*Store)(nil)` compiles; Update's fresh-id/birth-time/carry-forward semantics match harness's JSONL backend.
- **Back-compat** — a non-memory agent's spawn env and registry/build path are unchanged except the harmless `RUNTIME_AGENT_TENANT`; no harness change; full suite + both integration packages green.
- **Fail-fast** — opted-in-but-no-DB errors at build time (agent doesn't start memory-less).

Then proceed to `superpowers:finishing-a-development-branch`.

---

## Self-Review (plan vs. spec)

- **Spec coverage:** schema + projection (T1–T2), tenant-pinned Store implementing MemoryStore (T2), per-tenant isolation (T2 + T7), opt-in flag (T3), env injection of tenant + memory flag (T4), agentkind wiring with fail-fast (T5), agentd threading (T6), E2E (T7), docs incl. limitations (T8). All spec sections map to a task.
- **Deviation from spec (documented):** (1) the spec's `Save` data-flow said the store reads `OriginKey`; corrected — the *tool* reads it and passes `Origin` in the Entry, so the store persists `e.Origin` verbatim (verified against `../harness/tool/memory/tool.go`). (2) TEXT[] array handling: the spec didn't pick a mechanism; the plan adds a dependency-free `textArray` adapter unless `lib/pq` is already a dep (Step 3a decides deterministically). (3) The E2E drives the `Store` directly + asserts spawn-env injection rather than scripting an LLM memory tool call, because the bundled test provider only scripts the marker tool; documented in T7.
- **Type consistency:** `memory.NewStore(ctx, db, tenant) (*Store, error)`, `Store` methods match `hmem.MemoryStore` exactly, `hmem.Entry` fields, `Deps{AgentID, DB, Tenant, Memory}`, `AgentProcess.Memory`, `AgentConfig.Memory`, env vars `RUNTIME_AGENT_TENANT`/`RUNTIME_AGENT_MEMORY` — consistent across tasks.
- **Placeholder scan:** none — every step has concrete code/commands. The one branch point (array dependency) is resolved by a deterministic grep rule, not left open.
