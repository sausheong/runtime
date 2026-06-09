# Memory M3 — Auto-ingestion Design

**Date:** 2026-06-09
**Sub-project:** B2 Memory, third milestone
**Status:** Approved design, pre-implementation
**Builds on:** Memory M1 (`docs/superpowers/specs/2026-06-09-memory-m1-pg-memorystore-design.md`),
Memory M2 (`docs/superpowers/specs/2026-06-09-memory-m2-semantic-recall-design.md`)

---

## Goal

Make memory **capture** automatic. Today (M2) an agent only remembers what it
explicitly `save`s via the memory tool; recall is semantic but the writes are
manual. M3 implements harness's `KnowledgeGraph.Ingest` (currently a no-op in
`internal/memory/kg.go`) so that after each chat turn a background extractor
reads the conversation, pulls out durable facts, dedups them against existing
memory, and saves the new ones — which M2's embed-on-save makes instantly
recallable.

This completes the Memory arc: M1 built the durable per-tenant store
(tag/id retrieval), M2 added recall-by-meaning, M3 adds capture-by-meaning.

## Non-goals (explicit scope boundaries)

- **No whole-session synthesis.** Harness calls `Ingest` per-`Run` (per turn)
  with one turn's thread — this runtime drives harness via `RunTurn` in a
  throwaway per-turn session, so each `Ingest` sees exactly the user message +
  that turn's assistant text, tool calls, and tool results. M3 ingests
  per-turn. There is no session-end hook and we add none.
- **No harness changes.** We implement the existing `KnowledgeGraph.Ingest`
  seam only. Harness's `LLMProvider`/runtime are not extended.
- **No `Update`-on-refinement.** Dedup is skip-if-similar (append-or-skip).
  Merging or refining a near-duplicate entry is deferred.
- **No new extraction provider abstraction.** Extraction is one call to the same
  OpenAI-compatible proxy the agent already uses for chat and embeddings.
- **No memory GC / TTL / compaction.** Auto-ingestion adds rows; pruning dead or
  stale rows remains its own later Memory milestone.
- **No recall-quality / extraction-quality tuning framework.** The milestone
  delivers correct extract → dedup → save with operator-tunable thresholds.

---

## Decisions (locked during brainstorming)

| Decision | Choice |
|---|---|
| What decides "worth remembering" | **A dedicated LLM extraction call** (quality judgment; a heuristic would pollute memory). Model from `RUNTIME_INGEST_MODEL`, reusing the proxy. |
| When extraction runs | **Gate on growth + async.** Skip trivial turns (cheap message-count gate); run extract+dedup+save in a background goroutine so the turn never waits. |
| Write policy vs existing memory | **Semantic dedup before save.** Embed each candidate, `SearchSimilar` at a dedup floor; a near-duplicate ⇒ skip. (Append-or-skip; no merge.) |
| Enablement | **Piggyback on recall + own opt-in flag.** Requires M2 semantic recall on (`memory:true` + embeddings) **plus** `RUNTIME_INGEST_ENABLED`. Auto-write is off by default (it mutates memory without the agent asking). |
| Architecture | **Approach A:** extractor client + ingest orchestration both in `internal/memory`; the existing `KG.Ingest` method becomes the orchestrator. Mirrors M2's `embed.go`+`kg.go` shape. |

---

## Why the seam forces async

The runtime drives harness with `RunTurn` inside a DBOS step
(`agentruntime/serve.go`). Harness fires `Ingest` in a **deferred call at the
end of every chat `Run`**, with a fresh `context.Background()` (the request ctx
is often already cancelled by then), gated by `IngestSource` (`""`/`"chat"`
ingest; subagents and reviewers skip). Because that defer runs **inside**
`RunTurn`, inside the durable step, on the user's critical path, a synchronous
extractor would make every user-visible turn wait for an extraction LLM call —
and would run extraction inside the durable step. So `Ingest` must return
immediately and do its work in a background goroutine. This is not just an
optimization; it is required to keep the turn fast and keep the LLM call out of
the durable step. (Moving `Ingest` out of `RunTurn` was rejected — it is
harness-internal and would require modifying harness.)

The `thread` harness hands to `Ingest` accumulates: the user message, each
assistant text message, each tool call (rendered `"[tool: name]\n<input>"`), and
each tool result (or `"[error] ..."`).

---

## Architecture & components

One new file, three small modifications; harness untouched. Mirrors M2's
`embed.go`+`kg.go` structure.

| Unit | Change | Responsibility |
|---|---|---|
| `internal/memory/ingest.go` | **new** | `Extractor` interface: `Extract(ctx, thread []hrt.Message) ([]string, error)`. `httpExtractor` POSTs `{OPENAI_BASE_URL}/chat/completions` (Bearer `OPENAI_API_KEY`, model `RUNTIME_INGEST_MODEL`) with a fixed system prompt asking for durable facts as a JSON array of strings; parses `choices[0].message.content`, validates, caps to `maxFacts`. `NewExtractorFromEnv() (Extractor, bool, error)`: model unset ⇒ `enabled=false`; model set ⇒ enabled. |
| `internal/memory/ingest_test.go` | **new (hermetic)** | `httptest` request/response shape; JSON-array parse; malformed reply (non-JSON / object / `[]` / missing choices) ⇒ zero facts; fact cap; transport/non-200 ⇒ error; env parsing. |
| `internal/memory/kg.go` | **modify** | `KG` gains `extractor Extractor`, `save func(ctx, hmem.Entry) error` seam, `dedupFloor float64`, `minMsgs int`, `maxFacts int`, and a `sem chan struct{}` concurrency limiter. Plus an optional `ingestDone func()` test hook (nil in prod). `Ingest` becomes the real orchestrator (see Data flow). `NewKG` extended; `newKGWithSearch` test seam extended to inject fakes. |
| `internal/memory/kg_test.go` | **modify (hermetic)** | Growth gate; extract→dedup→save happy path; dedup skip; extractor/save/embed-fail degrade; over-cap drop. Uses the `ingestDone` hook for deterministic async. |
| `internal/agentkind/registry.go` | **modify** | In `wireMemory`, after recall is wired: if `RUNTIME_INGEST_ENABLED` is truthy AND recall is enabled, build the extractor (`NewExtractorFromEnv`; **fatal** if model unset/bad) and construct the KG with the ingest path; else build the KG exactly as M2 (no-op `Ingest`). Ingest-enabled-without-embeddings ⇒ warn and ignore the flag. |

### The seams

- Harness already defines `runtime.KnowledgeGraph`
  (`ShouldRecall`/`Recall`/`Ingest`) and `RuntimeDeps.KGFn`. M2 wired
  `agentruntime.Config.KGFn` → `hrt.RuntimeDeps.KGFn`. M3 changes **only** the
  body of `KG.Ingest` and how `NewKG`/`wireMemory` build the KG. No new public
  field in `agentruntime`, no harness change.
- `Extractor` is an interface (like `Embedder`) so the KG and all tests run
  without a live proxy.
- `KG.save` is a func seam (mirrors the existing `search` seam) so `Ingest` is
  unit-testable without Postgres.
- The extractor reuses the same proxy + key as chat and embeddings — no new
  harness LLM surface, no new egress destination.

### Enablement matrix

Each row requires every condition from the rows above it.

| `memory:` | embeddings env | `RUNTIME_INGEST_ENABLED` | Result |
|---|---|---|---|
| false/absent | any | any | No memory tool, no KG. (M1) |
| true | unset | any | M1 tag/id memory tool only. |
| true | set (valid) | unset/false | M2: tool + embed-on-save + semantic recall; `Ingest` no-op. |
| true | set (valid) | true (valid model) | **M3: all the above + auto-ingestion.** |
| true | unset | true | Flag ignored (no recall to feed); `slog.Warn` at startup. M1 behavior. |
| true | set (valid) | true, model unset/bad | **Fatal at agentd startup.** |

A "truthy" `RUNTIME_INGEST_ENABLED` is one of `1`, `true`, `yes`, `on`
(case-insensitive); anything else (incl. unset/empty) is off.

---

## Data flow

### `KG.Ingest(ctx, thread)` — orchestration

```
Ingest(ctx, thread):
  if extractor == nil || save == nil: return          # M2 mode / ingest disabled
  if !shouldIngest(thread): return                     # growth gate
  select:
    case sem <- struct{}{}:                            # acquire a slot
        go func():
            defer signalDone()                          # release slot, recover panic, fire test hook
            bg := context.Background()                  # NOT the request ctx (may be cancelled)
            facts, err := extractor.Extract(bg, thread)
            if err != nil { log(tenant,err); return }   # degrade
            for _, f := range facts:
                f = strings.TrimSpace(f); if f == "" { continue }
                if isDuplicate(bg, f) { continue }       # semantic dedup
                if err := save(bg, entry(f)); err != nil { log(tenant,err) }  # continue to next
    default:
        log("ingest at capacity, dropping turn", tenant) # over cap → drop (degrade)
```

`signalDone` (the goroutine's single `defer`): `recover()` (a malformed model
reply must never crash agentd), `<-sem` (release the slot), and — if non-nil —
call `ingestDone()` (test hook).

**Growth gate `shouldIngest(thread)`** — cheap, no LLM:
`len(thread) >= minMsgs` (default `minMsgs=2`, i.e. at least the user message +
one assistant message). Trivial single-message or empty turns produce no
extraction call. (Harness only calls `Ingest` at all when `len(thread) > 1`, so
this is a secondary, operator-tunable floor.)

**Concurrency cap** — `sem` is a buffered `chan struct{}` of size
`maxInflight` (default 4). At capacity we **drop**, not queue: an unbounded queue
under load is a memory leak and a cost runaway. Dropping a turn's ingest is safe
— facts that matter recur and are captured on a later turn.

**`context.Background()`** — identical to how harness itself invokes `Ingest`.
The goroutine deliberately outlives the turn; the request ctx is typically
cancelled by the time the defer runs.

### `isDuplicate(ctx, fact)` — semantic dedup

```
vec, err := embedder.Embed(ctx, fact)
if err != nil { return false }                 # embed failed → can't dedup → save anyway (degrade)
hits, err := search(ctx, vec, 1, dedupFloor)   # M2 SearchSimilar, k=1, tenant-scoped
if err != nil { return false }                 # search failed → save anyway (degrade)
return len(hits) > 0                            # a memory ≥ dedupFloor similar already exists
```

Reuses M2's exact tenant-scoped live-set search (`SearchSimilar`); cross-tenant
dedup is impossible by construction. `dedupFloor` default **0.85** —
deliberately higher than recall's 0.7 floor, so dedup suppresses only true
near-duplicates, not merely related facts.

### `entry(fact)` — the saved row

```
hmem.Entry{ Content: fact, Origin: "ingest", Tags: []string{"auto"} }
```

`Save` (M2) stamps id + timestamps and embeds on write, so the fact is
immediately recall-visible. `Origin:"ingest"` + the `auto` tag distinguish
auto-captured memories from tool-saved ones in `List`/audits and for a future GC
pass. The tenant is the Store's pinned tenant — isolation by construction,
unchanged from M1.

### Extraction call (`httpExtractor.Extract`)

POST `{OPENAI_BASE_URL}/chat/completions`:
- `model`: `RUNTIME_INGEST_MODEL`.
- `messages`: a fixed system prompt + one user message containing the rendered
  thread.
- System prompt (fixed): *"Extract durable, user-specific facts worth
  remembering long-term from this conversation. Return ONLY a JSON array of
  short factual statements (strings). Return `[]` if nothing is worth
  remembering. Exclude ephemeral details, pleasantries, and the assistant's own
  reasoning."*
- Thread render: join each `Message` as `"<role>: <content>"` lines.

Parse `choices[0].message.content`; trim markdown code fences if present;
`json.Unmarshal` into `[]string`. **Any** parse failure or non-array ⇒ return
`nil, nil` (zero facts — never error the goroutine over a malformed reply).
Truncate to `maxFacts` (default 10). A non-200 status or transport error ⇒
`error` (drives the goroutine's degrade-and-log path).

### Config (operator env)

| Var | Meaning | Default |
|---|---|---|
| `RUNTIME_INGEST_ENABLED` | master switch (requires recall enabled) | _unset/false_ |
| `RUNTIME_INGEST_MODEL` | chat model for extraction (reuses `OPENAI_BASE_URL`/`OPENAI_API_KEY`); required when enabled | _unset_ |
| `RUNTIME_INGEST_MIN_MESSAGES` | growth gate: min thread messages to extract | 2 |
| `RUNTIME_INGEST_MAX_INFLIGHT` | max concurrent extraction goroutines (drop over) | 4 |
| `RUNTIME_INGEST_DEDUP_FLOOR` | cosine floor at/above which a candidate is a duplicate (skip) | 0.85 |
| `RUNTIME_INGEST_MAX_FACTS` | hard cap on facts saved per turn | 10 |
| (reused) `OPENAI_BASE_URL` | proxy base for `/chat/completions` | (existing) |
| (reused) `OPENAI_API_KEY` | proxy bearer key | (existing) |

Malformed numeric values follow the existing `envInt`/`envFloat` pattern in
`agentkind` (warn + default). These tuning knobs are read in `wireMemory` and
passed into `NewKG`.

---

## Error handling & edge cases

Every failure in the ingest path is swallowed in the background goroutine —
**ingestion never affects a turn**, because by the time it runs the turn's
response is already delivered. The only hard-fails are operator misconfiguration,
loud at startup.

| Situation | Behavior |
|---|---|
| `RUNTIME_INGEST_ENABLED` unset/false | `Ingest` stays no-op (M2). |
| Ingest enabled, embeddings unset | Flag ignored; `slog.Warn` at startup (no recall to feed). Not fatal. |
| Ingest enabled, `RUNTIME_INGEST_MODEL` unset/blank | **Fatal at agentd startup** (operator asked for ingest, didn't say with what). |
| Extraction call fails / times out / non-200 | Log (tenant, err — never content); goroutine returns; no save. |
| Model returns non-JSON / not an array / `[]` / no choices | Zero facts; clean no-op. |
| Model returns > `maxFacts` | Truncated to `maxFacts`. |
| Embed fails for a candidate during dedup | `isDuplicate` returns false → save anyway (Save re-embeds; on its own embed failure M2 stores NULL — entry still durable + tag/id-retrievable). |
| Dedup `search` fails | `isDuplicate` returns false → save anyway (degrade). |
| `Save` fails | Log; continue to next candidate. |
| Over `maxInflight` | Drop this turn's ingest; log. |
| Goroutine panics (bad reply, etc.) | `recover()` in the defer; slot released; agentd unaffected. |
| Trivial turn (below growth gate) | No extraction call at all. |
| Subagent / review run | Harness gates `Ingest` by `IngestSource`; only `""`/`"chat"` runs ingest. Subagents and reviewers never ingest — inherited for free. |
| Duplicate fact across turns | Suppressed by semantic dedup (≥ `dedupFloor`). |
| Empty/whitespace candidate fact | Skipped before dedup/save. |

**Three load-bearing properties:**
1. **Ingestion never affects a turn.** It runs in a background goroutine after
   the response is delivered; every error degrades silently. Genuine operator
   misconfiguration (enabled but no model) is loud at startup.
2. **Bounded cost and memory.** The growth gate skips trivial turns; the
   inflight cap drops rather than queues; the fact cap bounds a runaway reply;
   dedup prevents unbounded duplicate accumulation.
3. **Isolation carries over unchanged.** Dedup search and saves are M1's
   tenant-pinned store; the security boundary is identical to M1/M2.

**Security:** conversation content goes to the **same proxy** the agent already
uses for chat and embeddings — same trust level, no new egress destination.
Saved facts live under the pinned `tenant_id`; dedup search is tenant-scoped, so
auto-capture cannot cross tenants. Logs carry tenant + error — never
conversation content or extracted facts.

---

## Testing strategy

### Unit (hermetic — no DB, no live proxy)

`internal/memory/ingest_test.go`:
- `httpExtractor.Extract` against `httptest.Server`: POSTs to
  `{base}/chat/completions` with model + bearer; parses a JSON-array reply →
  `[]string`.
- Malformed replies degrade to zero facts (no error): non-JSON content, a JSON
  object (not array), `[]`, missing `choices`.
- Fact cap: a reply with more than `maxFacts` strings is truncated.
- Markdown code-fence stripping: a ` ```json … ``` `-wrapped array still parses.
- Transport error / non-200 ⇒ error.
- `NewExtractorFromEnv`: model set ⇒ enabled; unset ⇒ disabled. (Fatal-on-missing
  -model is a `wireMemory` decision, tested there.)

`internal/memory/kg_test.go` (extended; fake extractor/embedder/search/save):
- Growth gate: thread shorter than `minMsgs` ⇒ no extractor call.
- Happy path: 2 candidates, neither a duplicate ⇒ both saved with
  `Origin:"ingest"` and the `auto` tag, in order.
- Dedup: a candidate whose `search` returns a hit ⇒ skipped; the other saved.
- Degrade: extractor error ⇒ no save, no panic; save error on the first
  candidate ⇒ the second is still attempted; embed-fail-during-dedup ⇒ saved
  anyway.
- Over-cap drop: fill `sem` ⇒ the next `Ingest` returns immediately without
  calling the extractor.
- **Async determinism:** tests inject `ingestDone` (a chan-close or callback) and
  await it instead of sleeping; the prod path leaves it nil.

### Integration (`//go:build integration`, Postgres+pgvector at the standard DSN)

`internal/memory/store_test.go` (extend): real `SearchSimilar` at the dedup
floor with deterministic hand-crafted vectors — a near-duplicate (cosine ≥ 0.85)
is found (→ would skip); a merely-related fact (cosine between recall 0.7 and
dedup 0.85) is **not** found by a `k=1, floor=0.85` search (→ would save).
Proves the floor separation is real against pgvector, not just the fake.

### End-to-end (`//go:build integration`, `test/memory_ingest_e2e_test.go`)

Build the real construction path the way `agentkind` does (tenant-pinned `Store`
+ embedder + KG with the ingest path) using a **fake deterministic extractor**
(known thread → known facts) and the **fake deterministic embedder** from M2 — no
live proxy. Drive `KG.Ingest` with a thread, await the `ingestDone` hook, then:
- the extracted fact is now stored and **recallable** via `KG.Recall` for a near
  query (proves the M3 → M2 loop closes end-to-end);
- re-ingesting the same thread saves nothing new (dedup against the now-stored
  fact);
- a different tenant's KG neither dedups against nor recalls the first tenant's
  auto-captured fact (isolation carries over).

`internal/agentkind/registry_test.go` (extend): `wireMemory` with
`RUNTIME_INGEST_ENABLED=1` + recall configured builds a KG whose `Ingest` is the
real path (asserted via the injected extractor/save being exercised, or via a
non-default field); with the flag off it builds the M2 no-op KG;
ingest-enabled-with-recall-but-no-model is a fatal error from `wireMemory`;
ingest-enabled-without-embeddings warns and yields M1 behavior.

### Live (manual, gated — not in CI)

A documented smoke against the real LiteLLM proxy: a real extraction model pulls
facts from an actual exchange; a later turn recalls them. Gated on
`RUNTIME_INGEST_MODEL` + the embedding env, like the existing nutrition live test.

### Deliberately not tested

Real-proxy extraction quality in CI (gated live only), GC/TTL (later milestone),
`Update`-on-refinement (out of scope), whole-session synthesis (harness is
per-turn).

---

## Backward compatibility

Fully additive. With `RUNTIME_INGEST_ENABLED` unset, behavior is exactly M2:
`KG.Ingest` stays a no-op, no extraction calls, no auto-writes. An M2 deployment
upgrades with no schema change (M3 reuses M2's `embedding` column and
`SearchSimilar`; it adds no DDL). No harness change; the existing tag/id and
semantic-recall paths, secrets/identity, and conformance suite are untouched.

---

## Limitations (record in README + ROADMAP)

- **Per-turn extraction, not session-level.** Each turn is ingested
  independently (harness calls `Ingest` per-`Run`). Facts that only emerge across
  several turns are captured turn-by-turn as they surface, not synthesized at
  session end.
- **Append-or-skip dedup.** A near-duplicate is skipped, not merged; an existing
  entry is never refined by a later, better-phrased restatement. Refinement
  (`Update`-on-similar) is future work.
- **Dropped under load.** When `maxInflight` extractions are already running, a
  turn's ingest is dropped (not queued). High-throughput agents may miss some
  captures; the facts recur and are captured later.
- **Extraction quality is model-dependent.** A weak extraction model yields noisy
  or thin facts; this is an operator model-selection concern, not handled in code
  beyond the durable-facts prompt, the fact cap, and dedup.
- **Conversation content leaves the process** — sent to the operator's proxy for
  extraction, same trust level as chat/embeddings; relies on operator TLS as chat
  already does.

---

## Documentation updates on completion

- README → an "Auto-ingestion" subsection under agent memory (the
  `RUNTIME_INGEST_*` env vars, the opt-in combination with recall, the
  degrade/drop behavior, the per-turn + append-or-skip notes).
- README env-var table → `RUNTIME_INGEST_ENABLED`, `RUNTIME_INGEST_MODEL`,
  `RUNTIME_INGEST_MIN_MESSAGES`, `RUNTIME_INGEST_MAX_INFLIGHT`,
  `RUNTIME_INGEST_DEDUP_FLOOR`, `RUNTIME_INGEST_MAX_FACTS`.
- ROADMAP §B2 → mark auto-ingestion done; note remaining (compaction/TTL/GC,
  finer per-agent/per-user scoping, per-tenant embedding models,
  refinement/merge dedup, session-level synthesis).
- `docs/images/project-layout.mmd` → note ingest/extractor in the `memory/` node.
- Project memory `runtime-platform-project.md` → Memory M3 paragraph.
