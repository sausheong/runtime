# Memory M2 — Semantic Recall Design

**Date:** 2026-06-09
**Sub-project:** B2 Memory, second milestone
**Status:** Approved design, pre-implementation
**Builds on:** Memory M1 (`docs/superpowers/specs/2026-06-09-memory-m1-pg-memorystore-design.md`)

---

## Goal

Make the per-tenant memory from M1 **semantic**: embed each memory entry on
save, and at the start of every agent turn recall the most similar entries by
meaning and inject them into the prompt — using harness's existing
`KnowledgeGraph` seam, pgvector for similarity search, and the OpenAI-compatible
LiteLLM proxy the platform already uses for embeddings. Recall is automatic
(no agent code), tenant-isolated, and best-effort (never breaks a turn).

This completes the store→semantic arc M1 began: M1 built the durable per-tenant
store with tag/id retrieval; M2 adds retrieval-by-meaning.

## Non-goals (explicit scope boundaries)

- **No auto-ingestion.** `KnowledgeGraph.Ingest` is a no-op this milestone.
  Memories still come only from the agent's explicit `memory` tool `save`/`update`
  (M1). Auto-extracting facts from conversation threads is a later milestone.
- **No per-tenant embedding model.** One deployment-wide model/dimension
  (pgvector's `vector(N)` needs a fixed N). Per-tenant models are future work.
- **No model migration tooling.** Changing the embedding model/dimension is a
  documented operator migration (drop+re-add column, re-embed), not automated.
- **No new embedding capability in harness.** The embeddings client lives in
  `internal/memory`; harness's `LLMProvider` is not extended. Harness is
  unmodified (only its existing `KnowledgeGraph`/`KGFn` seam is implemented + wired).
- **No recall-quality tuning framework.** HNSW params and recall@k quality are
  operator concerns; the milestone delivers correct top-K + floor selection.
- **No standalone memory service.** Embeddings + recall run in-process in agentd,
  same as the M1 store.

---

## Decisions (locked during brainstorming)

| Decision | Choice |
|---|---|
| What is embedded/recalled | **M1 memory entries**, embedded on save. Recall = similarity over the tenant's live entries. `Ingest` deferred (no-op). |
| Vector storage | **A `vector(N)` column on `memory_events`** (reuses M1's append-only log + live-set projection; delete/supersede handled for free). |
| Embedding model/dim | **Operator env**: `RUNTIME_EMBED_MODEL` + `RUNTIME_EMBED_DIM`, endpoint reuses `OPENAI_BASE_URL`/`OPENAI_API_KEY`. One model per deployment. Unset ⇒ semantic recall disabled (back-compat). |
| Embed failure on write | **Save anyway with NULL embedding** (degrade). The entry stays durable + tag/id-retrievable; invisible to recall until re-embedded. |
| Recall selection | **Top-K with a similarity floor** (both operator env vars with defaults). Bounds prompt size; floor prevents injecting irrelevant memories. |
| Architecture | **Approach A:** embeddings client + KG both in `internal/memory`; wired through a new optional `agentruntime.Config.KGFn` into harness's `RuntimeDeps.KGFn`. |
| Enablement | Semantic recall is on only when the agent has M1 memory (`memory: true`) **and** embeddings are configured. Either absent ⇒ pure M1 behavior. |

---

## Architecture & components

| Unit | Change | Responsibility |
|---|---|---|
| `internal/memory/schema.sql` | modify | When embeddings are enabled: `CREATE EXTENSION IF NOT EXISTS vector;`, `embedding vector(N)` column on `memory_events`, and an HNSW cosine index. N templated from `RUNTIME_EMBED_DIM` at DDL time. Skipped entirely when embeddings are disabled (M1-only DDL). |
| `internal/memory/embed.go` (new) | Embeddings client | `Embedder` interface: `Embed(ctx, text string) ([]float32, error)`. `httpEmbedder` POSTs `{OPENAI_BASE_URL}/embeddings` with `Authorization: Bearer {OPENAI_API_KEY}` and `RUNTIME_EMBED_MODEL`, parses `data[0].embedding`, validates length == dim. `NewEmbedderFromEnv() (emb *httpEmbedder, dim int, enabled bool, err error)`: model unset ⇒ `enabled=false`; model set + bad dim ⇒ `err` (fatal upstream). |
| `internal/memory/embed_test.go` (new, hermetic) | Tests | `httptest` request/response shape; dim-length validation; env parsing; transport error. |
| `internal/memory/store.go` | modify | `Store` gains optional `embedder Embedder` + `dim int`. `NewStore` signature extended to accept them (nil embedder ⇒ M1 behavior). `Save`/`Update` embed content → write the vector column (NULL on embed error). New `SearchSimilar(ctx, queryVec []float32, k int, floor float64) ([]hmem.Entry, error)`: pgvector cosine search joined to the live-set projection, tenant-scoped. |
| `internal/memory/kg.go` (new) | KnowledgeGraph | `kg{store *Store; embedder Embedder; k int; floor float64}` implementing harness `runtime.KnowledgeGraph`: `ShouldRecall(query) bool` (cheap heuristic), `Recall(ctx, query) string` (embed → SearchSimilar → format block; "" on any miss/error), `Ingest(ctx, thread)` (no-op). |
| `internal/memory/kg_test.go` (new, hermetic) | Tests | `ShouldRecall` heuristic; `Recall` formatting with fake embedder + store; empty/below-floor/error ⇒ "". |
| `internal/memory/store_test.go` | modify (integration) | Schema-on/off; non-NULL vs NULL-on-embed-fail writes; `SearchSimilar` ordering/K/floor/NULL-skip; liveness reuse (superseded/tombstoned excluded); cross-tenant recall isolation. |
| `agentruntime/config.go` | modify | `Config` gains `KGFn func(model string) hrt.KnowledgeGraph` (optional, nil ⇒ disabled). |
| `agentruntime/serve.go` | modify | `buildRuntime` passes `cfg.KGFn` into `hrt.RuntimeDeps{KGFn: cfg.KGFn}`. |
| `internal/agentkind/registry.go` | modify | When memory is enabled AND embeddings are configured, build the embedder + a tenant-pinned `Store` (with embedder) + the `kg`, and set `cfg.KGFn` to return it. The tool and the KG share one tenant-pinned store. Fatal if embeddings are misconfigured (model set, bad dim). |
| `cmd/agentd/main.go` | (no change) | Already passes `Deps{Tenant, Memory}`; the builder reads embedding env directly. |

### The seams

- Harness already defines `runtime.KnowledgeGraph` (`ShouldRecall`/`Recall`/`Ingest`)
  and `RuntimeDeps.KGFn func(model) KnowledgeGraph`. We **implement and wire**, we
  do not modify harness.
- `agentruntime.Config.KGFn` is the one new public field; `buildRuntime` is the one
  new pass-through (`hrt.RuntimeDeps{KGFn: ...}`).
- `Embedder` is an interface so the KG, store, and all tests run without a live proxy.

### Enablement matrix

| `memory:` | embeddings env | Result |
|---|---|---|
| false/absent | any | No memory tool, no KG. (M1) |
| true | unset | M1 memory tool (tag/id), no vector column, no KG. |
| true | set (valid) | M1 memory tool + embeddings on save + KG semantic recall. |
| true | model set, dim bad | Fatal at agentd startup. |

---

## Data model

`internal/memory/schema.sql`, applied by `NewStore` only when embeddings are
enabled (the dimension N is templated from `RUNTIME_EMBED_DIM`, validated as a
positive integer before string-substitution):

```sql
CREATE EXTENSION IF NOT EXISTS vector;
ALTER TABLE memory_events ADD COLUMN IF NOT EXISTS embedding vector(N);
CREATE INDEX IF NOT EXISTS memory_events_embedding_idx
    ON memory_events USING hnsw (embedding vector_cosine_ops);
```

- The column is **nullable**: `create`/`update` rows carry their content's
  embedding; `delete` rows and embed-failures leave it NULL. NULL rows are absent
  from the HNSW index and skipped by recall.
- When embeddings are disabled, none of this DDL runs — the M1 table shape is
  unchanged and the `vector` extension is not required.
- Dimension N is fixed per deployment. Changing the model/dim requires a migration
  (drop the column + index, re-add at the new N, re-embed). Documented in
  Limitations.

---

## Data flow

### Write (Save / Update)

```
agent memory tool → Store.Save(ctx, entry)            # tenant-pinned (M1)
  if s.embedder != nil:
     vec, err := s.embedder.Embed(ctx, entry.Content)
     if err != nil { log(tenant,id,err); vec = nil }  # degrade — write still succeeds
  INSERT memory_events (..., embedding) VALUES (..., vec)   # vec NULL ⇒ NULL column
```

`Update` embeds the new content; the superseding row (fresh id) gets a fresh
embedding, the old row drops out of the live projection (M1 semantics unchanged).
Updating a NULL-embedding entry is the natural backfill path.

### Recall (per Run, via the KnowledgeGraph)

```
harness Run start:
  kg.ShouldRecall(userMsg)
     false (trivial/empty/very short) ⇒ skip
     true ⇒ background goroutine (harness caps wait at 800ms, honors ctx cancel):
        kg.Recall(ctx, userMsg):
           vec, err := embedder.Embed(ctx, userMsg)
           if err != nil { return "" }               # best-effort
           hits := store.SearchSimilar(ctx, vec, K, floor)   # tenant-scoped
           if len(hits) == 0 { return "" }
           return formatBlock(hits)                   # "Relevant memories:\n- …"
  → returned string concatenated verbatim into the dynamic system-prompt suffix
```

### SearchSimilar query

```sql
SELECT e.entry_id, e.content, e.tags, e.origin, e.created_at, e.original_created_at,
       1 - (e.embedding <=> $2) AS similarity
FROM   memory_events e
WHERE  e.tenant_id = $1
  AND  e.embedding IS NOT NULL
  AND  e.op IN ('create','update')
  AND  NOT EXISTS (SELECT 1 FROM memory_events s
                   WHERE s.tenant_id = $1 AND s.supersedes = e.entry_id)
  AND  NOT EXISTS (SELECT 1 FROM memory_events d
                   WHERE d.tenant_id = $1 AND d.op = 'delete' AND d.entry_id = e.entry_id)
  AND  1 - (e.embedding <=> $2) >= $3        -- similarity floor
ORDER BY e.embedding <=> $2                  -- pgvector cosine distance, nearest first
LIMIT  $4                                    -- K
```

Reuses M1's exact liveness clauses (superseded/tombstoned never surface), adds
`embedding IS NOT NULL`, the floor, and ANN ordering. `<=>` is pgvector's
cosine-distance operator; `1 - distance` is the reported/compared cosine
similarity. Tenant scoping is identical to M1 — isolation by construction carries
over to recall.

### Config (operator env)

| Var | Meaning | Default |
|---|---|---|
| `RUNTIME_EMBED_MODEL` | embedding model sent to the proxy; unset ⇒ semantic recall disabled | _unset_ |
| `RUNTIME_EMBED_DIM` | the `vector(N)` dimension; required when model is set | _unset_ |
| `RUNTIME_EMBED_RECALL_K` | top-K entries recalled | 5 |
| `RUNTIME_EMBED_RECALL_FLOOR` | cosine-similarity floor (0–1) | 0.7 |
| (reused) `OPENAI_BASE_URL` | proxy base URL for `/embeddings` | (existing) |
| (reused) `OPENAI_API_KEY` | proxy bearer key | (existing) |

---

## Error handling & edge cases

| Situation | Behavior |
|---|---|
| `RUNTIME_EMBED_MODEL` unset | Semantic recall disabled; embeddings DDL skipped; no `KGFn`. Pure M1. |
| Memory enabled, embeddings unset | M1 tag/id memory tool only. Back-compatible. |
| Model set, `RUNTIME_EMBED_DIM` missing/non-positive | **Fatal at agentd startup** (operator error; `vector(N)` needs valid N). |
| Embedding fails on Save/Update | Degrade: log (tenant+id+err, never content), write `embedding = NULL`. Entry durable + tag/id-retrievable; invisible to recall until re-embedded. |
| Embedding fails in Recall | `Recall` returns `""`; turn proceeds with no hint. |
| Recall slower than 800ms | Harness caps the wait, ignores the late result; `ctx` cancel honored. No turn delay beyond the cap. |
| `ShouldRecall` on trivial/empty input | Returns false → no embed call, no query. |
| No entry clears the floor | Empty result → `Recall` returns `""`. No "least-bad" injection. |
| Tenant has only NULL-embedding rows | Filtered by `embedding IS NOT NULL` → empty recall; still tag/id-retrievable. |
| Cross-tenant recall | Impossible: `SearchSimilar` filters `tenant_id = s.tenant` (pinned). Reuses M1 isolation. |
| Proxy returns wrong-length vector | `Embed` validates length == dim; mismatch ⇒ treated as embed failure (NULL on write / "" on recall) + logged. Prevents pgvector insert errors from a misconfigured model. |
| `Update` of a previously-NULL entry | Superseding row gets a fresh embedding ⇒ becomes recall-visible (natural backfill). |
| pgvector extension unavailable | `CREATE EXTENSION` fails at `NewStore` when embeddings enabled ⇒ startup error. (Deploy image is `pgvector/pgvector:pg16`.) |

**Two load-bearing properties:**
1. **Writes never fail because of embeddings; recall never breaks a turn.** The
   pathway is degrade-don't-fail — NULL on write, "" on recall — except genuine
   operator misconfiguration (model set but no valid dim; missing extension),
   which is loud at startup. Preserves M1's "memory write is durable" posture.
2. **Isolation carries over unchanged.** Recall is M1's tenant-scoped live
   projection plus a vector ordering; the security boundary is identical.

**Security:** memory content and query text go to the same proxy the agent
already uses for chat (same trust level). Embeddings are stored under `tenant_id`
and never cross the tenant filter. Error logs carry tenant+id+model — never
content or vectors.

---

## Testing strategy

### Unit (hermetic, no DB, no live proxy)

`internal/memory/embed_test.go`:
- `httpEmbedder.Embed` against `httptest.Server`: POSTs to `{base}/embeddings`
  with model + bearer; parses `data[0].embedding` → `[]float32`.
- Dim validation: response vector length ≠ configured dim → error.
- `NewEmbedderFromEnv`: model+valid dim ⇒ enabled; model unset ⇒ disabled; model
  set + bad/zero/negative dim ⇒ error.
- Transport error ⇒ error (drives degrade path).

`internal/memory/kg_test.go` (fake `Embedder` + store seam):
- `ShouldRecall`: false for empty/whitespace/very short; true for a normal question.
- `Recall`: 2 hits → block contains both in similarity order; 0 hits → ""; embedder
  error → "".

### Integration (`//go:build integration`, Postgres+pgvector at the standard DSN)

`internal/memory/store_test.go` (extend):
- Schema: embeddings enabled ⇒ extension + `vector(N)` column + HNSW index created;
  disabled ⇒ column absent (M1-only path).
- `Save` writes non-NULL embedding with an embedder set; writes NULL when the
  (fake) embedder errors — and the row stays tag/id-retrievable (degrade proven).
- `SearchSimilar` with **deterministic hand-crafted vectors** (fake embedder maps
  known content→known vectors): nearest-first ordering, K cap, floor exclusion,
  NULL-embedding rows skipped.
- Liveness reuse: superseded + tombstoned entries never appear in results.
- Cross-tenant isolation: alpha's high-similarity entry invisible to a beta-pinned
  store's `SearchSimilar`.

### End-to-end (`//go:build integration`, `test/memory_recall_e2e_test.go`)

Wire the real construction path with a deterministic fake embedder injected (no
live proxy): build a memory+embeddings store the way `agentkind` does, save
entries, drive the KG's `Recall` for a query near one entry → assert the block
returns the right memory and excludes the unrelated one; a different tenant's KG
recalls nothing. Proves store→embed→search→format end-to-end.

A focused `agentruntime` unit test asserts `buildRuntime` passes a non-nil `KGFn`
into `RuntimeDeps` (the wiring), without a live LLM.

### Live (manual, gated — not in CI)

A documented manual smoke against the real LiteLLM proxy (real embeddings, recall
injected into a real prompt), gated on `OPENAI_API_KEY` + `RUNTIME_EMBED_MODEL`
like the existing nutrition live test.

### Deliberately not tested

Auto-ingestion (`Ingest` is a no-op), model/dim migration (operator concern),
real-proxy behavior in CI (gated live only), HNSW recall@k quality tuning.

---

## Backward compatibility

Fully additive. A deployment with no embedding env behaves exactly as M1 (no
vector column, no extension required, no KG). An M1 agent with `memory: true` is
unaffected until embeddings are configured. No harness change; the existing
tag/id memory paths, secrets/identity, and conformance suite are untouched.

---

## Limitations (record in README + ROADMAP)

- **No auto-ingestion** — memories come only from explicit tool saves; `Ingest`
  is a no-op. Conversation-fact extraction is the next Memory milestone.
- **One embedding model per deployment** — changing model/dim is a manual
  migration (drop+re-add the column, re-embed). No per-tenant models.
- **NULL-embedding backfill is manual/lazy** — an entry written while the proxy
  was down stays unsearchable until it is updated (or an operator re-embeds).
  No automatic background backfill in M2.
- **Embedding content leaves the process** — sent to the operator's proxy, same
  trust level as chat; relies on operator TLS as chat already does.

---

## Documentation updates on completion

- README → a "Semantic recall" subsection under agent memory (env vars, opt-in
  combination, degrade behavior, no-auto-ingest note).
- README env-var table → `RUNTIME_EMBED_MODEL`, `RUNTIME_EMBED_DIM`,
  `RUNTIME_EMBED_RECALL_K`, `RUNTIME_EMBED_RECALL_FLOOR`.
- ROADMAP §B2 → mark semantic recall done; note remaining (auto-ingest,
  compaction/TTL, finer scoping, per-tenant models).
- `docs/images/project-layout.mmd` → note embeddings/KG in the `memory/` node.
- Project memory `runtime-platform-project.md` → Memory M2 paragraph.
