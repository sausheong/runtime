# Memory M1 — Multi-tenant Postgres MemoryStore Design

**Date:** 2026-06-09
**Sub-project:** B2 Memory, first milestone
**Status:** Approved design, pre-implementation

---

## Goal

Give hosted agents **durable, multi-tenant memory**: a Postgres-backed
implementation of harness's existing `tool/memory.MemoryStore` interface, so an
agent's stock `MemoryTool` (`save`/`update`/`remove`/`list`/`get`) persists
entries across sessions and restarts, isolated per tenant. Opt-in per agent.
**Harness is not modified; agents use the memory tool they already understand.**

This is the spine of the Memory sub-project. It deliberately ships **tag/id
retrieval only** — semantic/vector recall is a later milestone via harness's
separate `KnowledgeGraph` seam, which this design leaves untouched and
unobstructed.

## Non-goals (explicit scope boundaries)

- **No semantic / vector search.** No embeddings, no pgvector use in this
  milestone. Retrieval is by tag and by id, exactly as `MemoryStore` defines.
  (pgvector is already the Postgres image; a later milestone uses it via
  `KnowledgeGraph`.)
- **No `KnowledgeGraph` implementation.** Auto recall/ingest is a separate
  milestone; this milestone neither implements nor blocks it.
- **No compaction / TTL / GC.** The event log grows append-only; pruning dead
  rows is future work (noted in Limitations).
- **No per-agent or per-user scoping.** The pool is **per-tenant**: all of a
  tenant's agents share one memory pool. Finer scoping is future work.
- **No new tool or contract divergence.** We implement harness's existing
  `MemoryStore`/`Entry`; the agent calls harness's existing `MemoryTool`.
- **No harness changes.** The backend lives entirely in the runtime repo.

---

## Decisions (locked during brainstorming)

| Decision | Choice |
|---|---|
| Harness seam | `tool/memory.MemoryStore` (explicit `MemoryTool`: save/update/remove/list/get). NOT `KnowledgeGraph` (deferred). |
| Isolation key | **Per-tenant** pool: all of a tenant's agents share one pool, keyed by `tenant_id`. Cross-tenant reads structurally impossible. |
| Enablement | **Opt-in per agent** via a `memory: true` flag in `runtime.yaml`. Absent ⇒ disabled (back-compat). |
| Storage model | **Append-only event log + projection.** One `memory_events` table; live set projected in SQL. Mirrors the JSONL reference backend's create/update/delete shape. |
| Backend location | **Approach A:** new `internal/memory` package in the runtime repo; projection computed in SQL. Harness untouched. |
| Tenant binding | Store is **pinned to its tenant at construction**; every query filters by the captured `tenant_id`. The agent's tool calls are unscoped (as harness defines) — safety comes from construction, not the caller. |
| Content validation | Lives in harness's `MemoryTool` (non-empty, ≤4000 chars). The store trusts the tool contract; it does not re-validate content (matches JSONL). |
| Opted-in but no DB | Fail fast at agentd startup (the builder errors). An agent that asked for memory never silently runs without it. |
| Absent tenant | `RUNTIME_AGENT_TENANT` empty ⇒ `"default"` (mirrors identity's absent-tenant convention). |

---

## Architecture & components

The runtime gains a tenant-pinned `*Store` that satisfies
`github.com/sausheong/harness/tool/memory.MemoryStore` using harness's
`memory.Entry` type. An `agentkind` builder wraps it in harness's stock
`memory.MemoryTool` and adds it to the agent's registry when the agent opts in.

| Unit | Change | Responsibility |
|---|---|---|
| `internal/memory/schema.sql` (new) | DDL | The `memory_events` append-only table + indexes (see Data model). |
| `internal/memory/store.go` (new) | The backend | `Store{db *sql.DB; tenant string}` implementing `memory.MemoryStore`. `NewStore(ctx, db, tenant) (*Store, error)` ensures the schema (under the shared DDL lock pattern, like `store.ApplyDDLLocked`) and returns the pinned store. ID generation mirrors JSONL: `mem_YYYY-MM-DD_<8-hex>`. Every statement filters by `s.tenant`. |
| `internal/memory/id.go` (new) | ID helper | `generateID(now time.Time) string` → `mem_2006-01-02_<8 hex>` (crypto/rand tail). Pure, unit-testable. |
| `internal/memory/store_test.go` (new, `//go:build integration`) | Tests | CRUD, supersede, tombstone, tag filter, origin-via-context, **cross-tenant isolation**, interface-conformance. |
| `internal/memory/id_test.go` (new, hermetic) | Tests | ID shape + uniqueness. |
| `internal/config/config.go` (modify) | Flag | `AgentConfig` gains `Memory bool` with `yaml:"memory"`. Absent ⇒ `false`. |
| `internal/config` test (modify/add) | Test | YAML parse: `memory: true` ⇒ true; absent ⇒ false. |
| `controlplane/proxy.go` (modify) | Spawn env | `buildEnv` appends `RUNTIME_AGENT_TENANT=<tenant>` (today it injects `RUNTIME_AGENT_ID`/`KIND`/`PG_DSN`/`LISTEN_ADDR` + secrets, but not the tenant). Also appends `RUNTIME_AGENT_MEMORY=1` when the agent's config opts in. |
| `controlplane/registry.go` (modify) | Carry fields | `AgentProcess` gains `Tenant` (already present) usage for env + a `Memory bool` field populated from `AgentConfig`. |
| `controlplane/proxy_test.go` (modify) | Test | Assert `RUNTIME_AGENT_TENANT` (and `RUNTIME_AGENT_MEMORY` when enabled) are in the spawn env; back-compat when absent. |
| `internal/agentkind/registry.go` (modify) | Wire | `Deps` gains `Tenant string` and `Memory bool`. When `Memory` is true, builders construct `memory.NewStore(ctx, db, tenant)` and add `&memory.MemoryTool{Store: st}` to the registry. A shared helper keeps each builder thin. |
| `cmd/agentd/main.go` (modify) | Read env | Read `RUNTIME_AGENT_TENANT` (default `"default"`) and `RUNTIME_AGENT_MEMORY`; pass into `agentkind.Deps`. |

### The seam

```go
// harness: github.com/sausheong/harness/tool/memory
type MemoryStore interface {
    Save(ctx, Entry) (Entry, error)
    Update(ctx, id, content string) (Entry, error)
    Remove(ctx, id string) error
    List(ctx, tag string) ([]Entry, error)
    Get(ctx, id string) (Entry, bool, error)
}
```

`internal/memory.Store` implements it. `&memory.MemoryTool{Store: st}` is
harness's own tool — no new tool, no contract divergence. A compile-time
assertion (`var _ memory.MemoryStore = (*Store)(nil)`) guards the contract.

### Tenant flow

```
runtime.yaml (agent→tenant, memory: true)
  → registry (AgentProcess{Tenant, Memory})
  → spawn env: RUNTIME_AGENT_TENANT=<t>, RUNTIME_AGENT_MEMORY=1
  → agentd reads them → agentkind.Deps{Tenant, Memory, DB}
  → memory.NewStore(ctx, db, tenant)  // store closes over tenant
  → &memory.MemoryTool{Store: st} added to the registry
```

The agent's tool calls are unscoped (harness's interface has no tenant param);
the store forces `WHERE tenant_id = s.tenant` on every query. An agent cannot
name another tenant's pool, so cross-tenant access is structurally impossible.

---

## Data model

`internal/memory/schema.sql` — one append-only row per operation:

```sql
CREATE TABLE IF NOT EXISTS memory_events (
    seq                 BIGSERIAL PRIMARY KEY,        -- append order (tiebreaker, audit)
    tenant_id           TEXT NOT NULL,
    op                  TEXT NOT NULL CHECK (op IN ('create','update','delete')),
    entry_id            TEXT NOT NULL,                -- the id this row introduces (fresh on create/update)
    content             TEXT,                         -- null on delete
    tags                TEXT[],                       -- null/empty allowed
    origin              TEXT NOT NULL DEFAULT '',
    supersedes          TEXT,                         -- set on update: the entry_id this replaces
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    original_created_at TIMESTAMPTZ                    -- set on update: carries birth time forward
);
CREATE INDEX IF NOT EXISTS memory_events_tenant_idx ON memory_events (tenant_id);
CREATE INDEX IF NOT EXISTS memory_events_supersedes_idx ON memory_events (tenant_id, supersedes);
```

It is an event log: `seq` is the key, `entry_id` repeats across an entry's
create/update/delete rows. No `(tenant,id)` PK.

### Projection — the live set (computed in SQL, scoped to the pinned tenant)

An `entry_id` is **live** iff, within the tenant:
1. it has a `create` or `update` row (its single defining event), **and**
2. it is **not superseded** — no `update` row names it in `supersedes`, **and**
3. it is **not tombstoned** — no `delete` row names its `entry_id`.

Because `Update` mints a **fresh** `entry_id` and points `supersedes` at the old
one, each live id maps to exactly one defining row — no recursive chain-walk.

```sql
SELECT e.entry_id, e.content, e.tags, e.origin, e.created_at, e.original_created_at, e.op
FROM   memory_events e
WHERE  e.tenant_id = $1
  AND  e.op IN ('create','update')
  AND  NOT EXISTS (SELECT 1 FROM memory_events s
                   WHERE s.tenant_id = $1 AND s.supersedes = e.entry_id)
  AND  NOT EXISTS (SELECT 1 FROM memory_events d
                   WHERE d.tenant_id = $1 AND d.op = 'delete' AND d.entry_id = e.entry_id)
-- List: ORDER BY COALESCE(original_created_at, created_at) ASC, seq ASC
-- Get:  AND e.entry_id = $2
-- List with tag: AND $2 = ANY(e.tags)
```

The query returns one row per live `entry_id` in the normal case (each id has a
single defining row). A caller-supplied **duplicate** `entry_id` with two
`create` rows is the only way to get two defining rows for one id; that is a
tolerated non-path (the tool auto-generates unique ids), and the duplicate would
simply appear twice in `List` until superseded/removed — harmless and not
special-cased. `seq` provides deterministic ordering.

### Mapping a live row → `memory.Entry`

- `ID = entry_id`
- `Content = content`, `Tags = tags`, `Origin = origin`
- `CreatedAt =` `original_created_at` when non-null (update rows) else
  `created_at` (create rows) — the **birth time**, preserved across updates.
- `UpdatedAt = created_at` (the time this defining row was written).

This matches the JSONL backend's field semantics exactly.

---

## Data flow (the five methods)

All methods filter by the pinned `s.tenant`.

- **`Save(ctx, e)`** — if `e.ID == ""` generate `mem_YYYY-MM-DD_<hex>`; read
  `origin` from `memory.OriginKey` context value (default `"agent"`, as the tool
  sets); insert one `create` row (`created_at = now`). Return the entry with
  `CreatedAt = UpdatedAt = now`.
- **`Update(ctx, id, content)`** — run the live query for `id`; `ErrNotFound` if
  absent. Mint a fresh `entry_id`; insert an `update` row with `supersedes = id`,
  `tags`/`origin` copied from the old live row, `original_created_at =` old birth
  time, `created_at = now`. Return the new entry (`CreatedAt =` old birth,
  `UpdatedAt = now`).
- **`Remove(ctx, id)`** — insert a `delete` row for `id`; return nil. Idempotent
  (the tombstone is authoritative even with no matching create).
- **`List(ctx, tag)`** — live query; if `tag != ""` add `AND $2 = ANY(tags)`;
  order by birth `CreatedAt` then `seq`. Return `[]Entry`.
- **`Get(ctx, id)`** — live query filtered to `entry_id = id`; return
  `(Entry, true, nil)` if live, else `(Entry{}, false, nil)` for unknown or
  tombstoned (no error).

**Origin provenance:** harness's `MemoryTool` reads `memory.OriginKey` from
context to tag writes (`"agent"` foreground, `"review"` during review passes).
`Save` honors it; the store does not invent origins.

---

## Error handling & edge cases

| Situation | Behavior |
|---|---|
| Agent not opted in (`memory` absent/false) | No store, no tool. Byte-identical to today. |
| Opted in but Postgres unreachable at construction | `NewStore` errors → builder errors → agentd fails fast at startup. No silent memory-less run. |
| `RUNTIME_AGENT_TENANT` empty | Tenant = `"default"` (identity convention). |
| `Save` empty/oversize content | Rejected by the `MemoryTool` (`ErrInvalidContent`) before the store; store does not re-validate. |
| `Save` with caller-supplied duplicate `id` | Tolerated (event log). Projection's latest-defining-row-by-`seq` resolves one live row. Tool auto-generates ids, so a non-path. |
| `Update` unknown/tombstoned `id` | `ErrNotFound`; no row written. |
| `Remove` unknown `id` | Append tombstone, return nil (idempotent). |
| `Get`/`Update`/`Remove` for another tenant's `id` | Invisible: live query is tenant-scoped ⇒ `Get` `ok=false`, `Update` `ErrNotFound`, `Remove` no-op tombstone in the caller's own tenant. |
| `tag == ""` | No tag filter; all live entries. |
| Concurrent writers in one tenant pool (sibling agents) | Append-only inserts never conflict; `seq` serializes; projection at read time picks latest-by-`seq` for any contested id. No locks. |
| DB error mid-method | Wrapped (`fmt.Errorf("memory: <op> tenant %q id %q: %w")`), never including entry content. Surfaces as a tool error; the turn continues per harness. |
| Large append-only history | Acceptable for M1; the tenant + supersedes indexes keep the projection scoped. Compaction/TTL is a later milestone. |

**Two load-bearing properties stated outright:**
1. **Isolation is enforced by construction, not the caller.** The interface has
   no tenant param; safety is the store's captured `tenant`. A foreign id reads
   as absent rather than erroring — there is no API to name another pool.
2. **Validation is the tool's job.** The store trusts the `MemoryTool` contract
   for content limits (matches JSONL); no duplication.

**Security:** memory content is tenant-private and never crosses the tenant
filter. Error messages and logs carry op + tenant + id only, never content.
Memory rides the same Postgres + DSN trust level as the rest of the platform.

---

## Testing strategy

### Unit (hermetic, no DB)

- `internal/config`: `memory: true` parses to `true`; absent ⇒ `false`
  (back-compat) — table test over a YAML fixture.
- `internal/memory/id_test.go`: `generateID` shape (`^mem_\d{4}-\d{2}-\d{2}_[0-9a-f]{8}$`)
  + uniqueness across many calls.
- `controlplane/proxy_test.go`: `buildEnv` includes `RUNTIME_AGENT_TENANT=<t>`,
  and `RUNTIME_AGENT_MEMORY=1` only when enabled; well-formed when tenant empty;
  unchanged for non-memory agents (back-compat regression guard).

### Integration (`//go:build integration`, Postgres at the standard DSN)

`internal/memory/store_test.go` (drops + recreates `memory_events`; `t.Cleanup`
drops it — the M1 test-pollution lesson):
- Save→Get round-trip: content/tags/origin preserved; `CreatedAt == UpdatedAt`
  on a fresh entry.
- List ordering by birth `CreatedAt`; tag filter (`tag != ""` filters;
  `tag == ""` returns all).
- **Update**: returns a fresh ID; old ID `Get`s `ok=false`; new `CreatedAt` ==
  original birth, `UpdatedAt` == update time; tags/origin carried forward; `List`
  shows exactly one live row (no ghost).
- **Remove**: `Get` ⇒ `ok=false`, excluded from `List`; unknown-id `Remove` ⇒
  nil and stays absent.
- **Origin via context**: `Save` with `OriginKey="review"` persists
  `origin="review"`; default ⇒ `"agent"`.
- **Cross-tenant isolation (headline)**: two stores pinned to `alpha`/`beta`
  over the same DB; `beta` cannot see/update/remove `alpha`'s entries; `alpha`
  retains its own.
- **Contract conformance**: `var _ memory.MemoryStore = (*Store)(nil)` and drive
  the five methods through the interface value.

### End-to-end (`//go:build integration`, `test/memory_e2e_test.go`)

Using the existing deterministic test-agent / `command:` spawn machinery (the
secrets E2E pattern; no live LLM):
- A memory-enabled agent for tenant `alpha` spawns through the registry/SpawnFunc;
  a driven `memory` tool `save` lands a row in `memory_events` under
  `tenant_id=alpha` and reads back via `list`/`get`.
- A second agent in the **same tenant** sees the first's memory (per-tenant
  shared pool); an agent in a **different tenant** does not (end-to-end
  isolation).

### Deliberately not tested

Semantic/vector recall (not built — `KnowledgeGraph` milestone), compaction/TTL
(later), embeddings (later), live-LLM behavior (CI is hermetic-against-Postgres).

---

## Backward compatibility

Fully additive. An agent without `memory: true` is byte-identical to today (no
store, no tool, spawn env gains only the harmless `RUNTIME_AGENT_TENANT`, which
other code ignores). No harness change. Existing agents, the secrets/identity
paths, and the conformance suite are untouched.

---

## Limitations (record in README + ROADMAP)

- **No semantic search in M1** — tag/id retrieval only. Vector recall is the next
  Memory milestone via harness's `KnowledgeGraph` seam (pgvector + an embedding
  source).
- **Per-tenant pool only** — no per-agent/per-user scoping yet.
- **No compaction/TTL/GC** — the event log grows append-only; pruning dead
  (superseded/tombstoned) rows is future work.
- **Content trust** — the store relies on the `MemoryTool`'s content validation;
  a non-tool caller could write oversize content (not a path in the hosted
  platform).

---

## Documentation updates on completion

- README → a "Per-tenant agent memory" subsection (opt-in flag, durable,
  per-tenant, tag/id retrieval, semantic search noted as future).
- README env-var table → `RUNTIME_AGENT_TENANT`, `RUNTIME_AGENT_MEMORY`
  (agentd-side, injected by the platform).
- `runtime.yaml` example → show a `memory: true` agent.
- ROADMAP §B2 → mark the first Memory milestone done; note remaining (semantic
  recall, compaction, finer scoping).
- `docs/images/project-layout.mmd` → add the `internal/memory` node.
- Project memory `runtime-platform-project.md` → Memory M1 paragraph.
