# KnowledgeGraph → RunTurn Wiring Design

**Date:** 2026-06-09
**Sub-project:** B2 Memory (corrective milestone, between M3 build and M3 merge)
**Status:** Approved design, pre-implementation
**Builds on:** Memory M2 (semantic recall) and M3 (auto-ingestion orchestration unit,
on branch `feat/memory-m3-auto-ingestion`, not yet merged)

---

## Goal

Make the harness `KnowledgeGraph` seam actually fire on the production turn path.
The runtime drives every turn through harness's `RunTurn`, which **by design does
not consult `r.KG`** — so M2 semantic recall AND M3 auto-ingestion are both
wired-but-inert in the deployed system. This milestone teaches `RunTurn` to
consult the KG: bounded-synchronous **recall** before the LLM call (first round
of an exchange) and **ingest** at exchange completion. It closes the gap the M3
final holistic review surfaced.

## Root cause (verified against source)

- The runtime executes every turn via `rt.RunTurn(...)` inside a DBOS step
  (`agentruntime/serve.go:159`). It never calls harness's `Run`.
- `RunTurn` (`harness/runtime/runturn.go`) builds only the static system prompt
  and contains no `r.KG` references. Its own doc comment states it "does NOT
  perform compaction, knowledge-graph recall/ingest, ... Those remain Run's
  responsibility." The only `Ingest`/`Recall` call sites in harness live inside
  `Run` (`runtime.go:316` ingest; `runtime.go:293-307,340-361` recall).
- `BuildRuntime` DOES set `r.KG` from our `agentruntime.Config.KGFn`
  (`builder.go:185`), so the seam is present on the Runtime object — but nothing
  on the `RunTurn` path reads it.
- Every M2/M3 test calls `kg.Recall`/`kg.Ingest` **directly**, never through
  `RunTurn`, so the unit tests are green while the wired path never fires. The M3
  design spec's premise ("harness fires Ingest in a deferred call inside
  RunTurn") was aspirational, not true of this harness.

## Decisions (locked during brainstorming)

| Decision | Choice |
|---|---|
| Where the wiring lives | **In harness `RunTurn`.** RunTurn is owned code this project added; teaching it the KG seam fixes recall + ingest for any RunTurn driver, and — because RunTurn runs inside the DBOS step — gets replay-safety for free. |
| Ingest cadence | **Once per exchange**, on the round that completes it (RunTurn returns `Done`+`"completed"`), over the full session thread. Mirrors `Run`'s once-per-`Run` semantics. |
| Recall cadence | **First round only** (the round carrying the user message). Mirrors `Run`'s once-per-`Run` recall. |
| Recall latency | **Bounded-synchronous**: run `Recall` synchronously under an 800ms `context.WithTimeout`; on timeout/error inject nothing and proceed. |
| Harness-unmodified rule | **Relaxed for this milestone**, scoped to `RunTurn` (owned code). A deliberate, approved exception. |

## Non-goals

- No change to the M3 orchestration unit (`internal/memory/*`) — it is correct
  and tested; it only needs calling.
- **No change to `agentruntime/serve.go` wiring** — it already passes `KGFn` into
  `BuildRuntime`. The runtime side of this milestone is the new through-serve test
  + doc reconciliation only.
- No compaction, streaming-tool kickoff, trace/slog parity, or the other
  `Run`-only behaviors. RunTurn stays minimal except for the KG seam.
- No per-round recall or per-round ingest (the locked cadence is first-round
  recall, completing-round ingest).
- No `IngestSource` field on RunTurn (see "IngestSource" below).

---

## Architecture: the exchange / round model

The crux is the mismatch between harness's `Run` (one call = a whole user
exchange) and the runtime's `RunTurn` (one call = one round; the serve loop calls
it repeatedly until `Done`). KG semantics are defined per-**exchange** but must be
enforced inside a per-**round** function.

**The serve loop** (`serve.go:132-191`): a `for turn` loop. Each iteration wraps
one `RunTurn` in `dbos.RunAsStep` on a throwaway per-turn session seeded with all
prior canonical history. Round 0 carries `userMsg`; continuation rounds pass `""`.
The loop ends when `RunTurn` returns `Done=true`.

**RunTurn detects the two boundary rounds from signals it already has:**
- **First round of the exchange** ⇔ `userMsg != "" || len(images) > 0` (the
  existing guard at `runturn.go:45`). → run recall here.
- **Completing round of the exchange** ⇔ the no-tool-calls path that returns
  `Done=true, "completed"` (`runturn.go:124-127`). → run ingest here, over the
  full session thread.

This yields the locked cadence without the serve loop having to tell RunTurn which
round it is.

**The thread for ingest:** `RunTurn` is stateless across rounds (unlike `Run`,
which accumulates `[]Message`). But on the completing round, `r.Session` already
holds the entire conversation — the serve loop seeds the per-turn session with all
prior canonical entries, and this round appended its own. So ingest derives the
thread from `r.Session.View()` via a helper that maps entries to `[]hrt.Message`
exactly as `Run` renders its thread.

**Components touched:**

| Unit | Change |
|---|---|
| `harness/runtime/runturn.go` | Recall block (first round) folding a hint into the prompt parts; ingest call (completing round) over the session-derived thread; both gated on `r.KG != nil`. |
| `harness/runtime/runturn.go` (helper) | `sessionThread(history []session.SessionEntry) []hrt.Message` — entries→Message, mirroring `Run`'s role/tool rendering. |
| `harness/runtime/runturn_test.go` | Unit tests with a fake KnowledgeGraph + fake LLM. |
| `runtime` repo: `test/` | New through-serve integration test (the regression guard the review flagged as missing). |
| `runtime` repo: docs | Reconcile M3 spec/README/ROADMAP with the now-live behavior. |

**Replay safety (DBOS):** the wiring lives inside `RunTurn`, which runs inside
`dbos.RunAsStep`. On replay, the step's return value is memoized and the closure
does not re-execute — so recall is not recomputed and ingest does not re-fire.
This is the decisive advantage over a serve.go implementation (serve-loop code
re-runs on replay). One explicit consequence: ingest's background goroutine fires
on a **live** run only; on **replay** the step body is skipped, so it does not
fire — the correct outcome (no double-capture on recovery), recorded here as a
decision, not an accident.

---

## Data flow (the modified RunTurn)

### Recall — first round, bounded-synchronous, before the LLM call

Inserted after the user message is appended (`runturn.go:58`) and before the
prompt parts are built (`runturn.go:73`):

```go
var kgHint string
if r.KG != nil && (userMsg != "" || len(images) > 0) && r.KG.ShouldRecall(userMsg) {
    rctx, cancel := context.WithTimeout(ctx, 800*time.Millisecond)
    kgHint = r.KG.Recall(rctx, userMsg) // "" on timeout/error/no-hits
    cancel()
}
```

Prompt assembly (currently just the static part at `runturn.go:73`) gains the
dynamic suffix:

```go
parts := []llm.SystemPromptPart{{Text: r.StaticSystemPrompt, Cache: true}}
if kgHint != "" {
    parts = append(parts, llm.SystemPromptPart{Text: kgHint, Cache: false})
}
```

Mirrors `Run`'s structure (`runtime.go:353-361`): static part cached, recall hint
a separate **non-cached** suffix (it varies per query; caching would poison the
prefix cache). `Recall` already returns a prompt-ready block
(`"Relevant memories:\n- ...\n"`) or `""`. The 800ms `context.WithTimeout` is the
bounded-synchronous cap; on timeout the search is abandoned and the turn proceeds
hint-less. Recall only runs on the user-message round; continuation/tool rounds
skip it because `userMsg == ""`.

Note: `runturn.go` must add `time` to its imports (it does not import it today).

### Ingest — completing round, once, over the full thread

At the no-tool-calls return (`runturn.go:124-127`):

```go
if len(toolCalls) == 0 {
    if r.KG != nil {
        r.KG.Ingest(context.Background(), sessionThread(r.Session.View()))
    }
    emit(AgentEvent{Type: EventDone, Usage: lastUsage})
    return r.turnSlice(startLen, true, "completed", lastUsage, nil), nil
}
```

- `context.Background()` (not the request ctx) — ingest's goroutine outlives the
  turn; the request ctx may be cancelling. Same rationale as M3/Run.
- `Ingest` is already non-blocking (M3 spawns a bounded goroutine), so this adds
  negligible turn latency.
- Fires **only** on the completing round (no tool calls). A multi-round tool
  exchange ingests exactly once, over the whole conversation — matching `Run`'s
  once-per-exchange semantics.
- The abort and error returns (`runturn.go:61,92,116,137`) do **not** ingest —
  only clean completion does. Mirrors `Run` gating ingest to a clean finish; an
  aborted/errored exchange should not capture.

### `sessionThread` helper

Converts `r.Session.View()` to `[]hrt.Message`, mirroring `Run`'s thread
rendering:

- `EntryTypeMessage` with role `user`/`assistant` → `Message{Role: role, Content: text}`
  (decode `MessageData.Text` from `entry.Data`).
- `EntryTypeToolCall` → `Message{Role: "assistant", Content: "[tool: <tool>]\n<input>"}`
  (decode `ToolCallData`).
- `EntryTypeToolResult` → `Message{Role: "user", Content: output}`, or
  `"[error] <err>"` when the result is an error (decode `ToolResultData`).
- `EntryTypeCompaction` and `EntryTypeMeta` entries (and any `system`-role
  message) are skipped — `Run`'s thread carries only user/assistant turns and
  tool exchanges, not synthetic system/summary entries. (Entry types confirmed in
  `session/session.go`: `EntryTypeMessage`/`EntryTypeToolCall`/
  `EntryTypeToolResult`/`EntryTypeCompaction`/`EntryTypeMeta`.)

`View()` (not `Entries()`) is used so a compacted session contributes its
post-compaction view, consistent with how the LLM messages are assembled in the
same function.

### IngestSource

`Run` gates ingest by `IngestSource` (`""`/`"chat"` ingest; `"review"`/subagent
skip). `RunTurn` has no `IngestSource` field. The runtime only ever drives
top-level chat agents through `RunTurn` (harness reviewers/subagents use `Run`),
so every `RunTurn` completion is ingest-eligible — equivalent to
`IngestSource=="chat"`. If a future caller routes reviewers/subagents through
`RunTurn`, adding an `IngestSource`-style gate is a documented follow-up, not a
silent surprise.

---

## Error handling & edge cases

| Situation | Behavior |
|---|---|
| `r.KG == nil` (memory/recall off) | Both blocks skipped; `RunTurn` is byte-identical to today. Whole feature gated on the existing `KGFn` wiring. |
| `ShouldRecall` false | No embed/search, no suffix; turn proceeds. |
| Recall times out (>800ms) or errors | `Recall` returns ""; no suffix; turn proceeds. `context.WithTimeout` cancelled regardless. |
| Ingest extraction/embed/save fail | Swallowed in M3's background goroutine (already proven); never touches the turn. |
| Continuation/tool round | `userMsg == ""` ⇒ no recall; `len(toolCalls) > 0` ⇒ not the completing round ⇒ no ingest. |
| Exchange ends via abort/error | No ingest (only clean "completed" triggers it). |
| DBOS replay of a completed step | Step body not re-executed ⇒ recall not recomputed, ingest does not re-fire. No double-capture on recovery. |
| Empty session on completing round | `sessionThread` returns a short slice; M3's growth gate (`minMsgs`) skips trivial threads. |

**Two load-bearing properties carry over unchanged:** recall/ingest never break a
turn (recall bounded + best-effort; ingest async + swallowed), and tenant
isolation is identical (the KG is the tenant-pinned one `wireMemory` built;
nothing here changes scoping).

**Security:** unchanged from M2/M3 — recall query and the ingest thread go to the
same proxy the agent already uses; no new egress; logs carry no content.

---

## Testing strategy

### Harness unit tests (`harness/runtime/runturn_test.go`, hermetic)

With a fake `KnowledgeGraph` and a fake LLM that captures the `ChatRequest`:
- **Recall reaches the prompt:** fake KG whose `Recall` returns a sentinel string;
  assert it appears in the request's system-prompt parts on the user-message
  round, as a non-cached part. Absent on continuation rounds (`userMsg==""`).
- **ShouldRecall false** ⇒ `Recall` not called, no suffix.
- **Recall timeout** ⇒ fake KG `Recall` blocks past 800ms; assert no suffix and
  the turn still completes (no added stall beyond the cap).
- **Ingest once on completion:** fake KG records `Ingest` calls; assert exactly
  one call on the no-tool-calls round, with a thread derived from the session;
  zero calls on a tool-call (continuation) round; zero on abort/error returns.
- **`sessionThread` rendering:** table test mapping user/assistant/tool-call/
  tool-result/system entries to the expected `[]Message`.
- **`r.KG == nil`** ⇒ neither called; the existing RunTurn tests remain green.

### Runtime through-serve integration test (`test/`, `//go:build integration`)

The regression guard the final review said was missing. Drive a real exchange
through the serve/`RunTurn` path against real Postgres+pgvector, with an
ingest+recall-enabled KG built the way `wireMemory` does (fake deterministic
embedder + fake extractor — no live proxy). Use a scripted test provider that
emits one assistant message and no tool calls (completes in one round).
- Assert the extracted fact lands in the store after the exchange (ingest fired
  through the real loop).
- Run a second exchange whose query is near that fact and assert a recall hint was
  produced on the live path — e.g. a test provider that records the system-prompt
  parts it received, asserting the recall block is present.

This test, run through the serve loop (not by calling `kg.*` directly), is what
would have caught the original gap.

### Existing suites (must stay green)

All current `runturn_test.go`, the M2 recall e2e, the M3 e2e, the conformance
suite, and both integration packages.

---

## Backward compatibility

- `RunTurn` with `r.KG == nil` is byte-identical to today — every existing caller
  and the conformance suite are unaffected.
- The harness change is additive (a new optional code path + one helper); no
  signature change to `RunTurn` or `TurnResult`.
- The runtime requires no serve.go change; it picks up the harness change through
  the local `replace ../harness` directive on rebuild.

---

## Documentation updates on completion

- M3 spec (`...memory-m3-auto-ingestion-design.md`) → addendum: the
  "Ingest fires inside RunTurn" premise was aspirational; this milestone makes it
  true by wiring the seam.
- README → recall + ingest are now genuinely live on the serve path (remove any
  implication they were already firing pre-wiring).
- ROADMAP §B2 → add the wiring milestone; note M2 recall was also inert until now.
- Project memory `kg-seam-not-wired-into-runturn.md` → mark resolved when merged.
```
