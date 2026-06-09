# KnowledgeGraph → RunTurn Wiring Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make harness `RunTurn` consult `r.KG` — bounded-synchronous semantic recall before the LLM call (first round of an exchange) and auto-ingestion at exchange completion — so M2 recall and M3 ingest actually fire on the runtime's production turn path.

**Architecture:** Wire the KG seam inside harness's `RunTurn` (owned code, runs inside the runtime's DBOS step ⇒ replay-safe). Recall folds a non-cached hint into the prompt RunTurn already builds; ingest fires once, on the no-tool-calls completion round, over a thread derived from the session. The runtime side needs no serve.go change — only a through-serve regression test + doc reconciliation.

**Tech Stack:** Go 1.25.1. Two repos: `github.com/sausheong/harness` (at `/Users/sausheong/projects/harness`, branch `feat/kg-runturn-wiring`) and `github.com/sausheong/runtime` (at `/Users/sausheong/projects/runtime`, branch `feat/memory-m3-auto-ingestion`), linked by a `replace ../harness` directive. pgvector/Postgres for the integration test.

**Spec:** `docs/superpowers/specs/2026-06-09-kg-runturn-wiring-design.md`

---

## Critical conventions (read before starting)

- **Two repos, two branches.** The wiring lives in **harness** (`/Users/sausheong/projects/harness`, branch `feat/kg-runturn-wiring` — already created). The regression test + docs live in **runtime** (`/Users/sausheong/projects/runtime`, branch `feat/memory-m3-auto-ingestion` — already checked out). They are linked by `replace ../harness`, so a runtime build/test automatically picks up uncommitted harness changes. **Use `go -C <dir>` and `git -C <dir>`** rather than `cd` to avoid working-directory drift.
- **`go` CLI is ground truth.** The IDE/LSP is unreliable in this cross-module setup — ignore its diagnostics. Trust only `go build`/`go test`/`go vet` exit codes.
- **Harness commits** (use the harness dir):
  ```bash
  git -C /Users/sausheong/projects/harness add <files>
  git -C /Users/sausheong/projects/harness -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "<msg>

  Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
  ```
- **Runtime commits** (use the runtime dir, same author/trailer).
- **Harness baseline is green** on this branch: `go -C /Users/sausheong/projects/harness test ./runtime/ -run TestRunTurn` passes today. Existing `runturn_test.go` tests MUST stay green.
- **Integration tests** need local Postgres+pgvector at `postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable` with the `vector` extension already created. A test that SKIPs on "postgres not reachable" is NOT a pass.

---

## Background: how RunTurn works today (so the edits are precise)

`/Users/sausheong/projects/harness/runtime/runturn.go` (the FULL current body, for reference — your edits insert into it):

- Lines 39-58: signature `func (r *Runtime) RunTurn(ctx, userMsg string, images []llm.ImageContent, emit TurnEmit) (TurnResult, error)`; appends the user message (or user+images) to `r.Session` when `userMsg != "" || len(images) > 0`.
- Lines 60-62: pre-cancelled-ctx short-circuit → aborted.
- Lines 64-72: `history := r.Session.View()`; `assembleMessages`; tool defs.
- Line 73: `parts := []llm.SystemPromptPart{{Text: r.StaticSystemPrompt, Cache: true}}` — **only the static part today**.
- Lines 74-94: builds `llm.ChatRequest{... SystemPromptParts: parts ...}`, calls `r.LLM.ChatStream`.
- Lines 96-127: drains the stream; appends assistant text; **line 124** `if len(toolCalls) == 0 {` → emits `EventDone`, returns `r.turnSlice(startLen, true, "completed", lastUsage, nil)` — **this is the completing round**.
- Lines 129-141: serial tool dispatch, then returns `"continue"` (not done).

`r.KG` is type `KnowledgeGraph` (interface in `runtime/types.go`):
```go
type KnowledgeGraph interface {
	ShouldRecall(query string) bool
	Recall(ctx context.Context, query string) string
	Ingest(ctx context.Context, thread []Message)
}
```
`Message` is `struct { Role string; Content string }`. The runtime's KG (from `internal/memory`) implements all three; `Recall` returns a prompt-ready block or `""`; `Ingest` is async + best-effort.

Session entry types (`session/session.go`): `EntryTypeMessage` (`"message"`, role user/assistant/system), `EntryTypeToolCall` (`"tool_call"`), `EntryTypeToolResult` (`"tool_result"`), `EntryTypeCompaction`, `EntryTypeMeta`. Payloads: `MessageData{Text, Images}`, `ToolCallData{Tool, ID, Input}`, `ToolResultData{ToolCallID, Output, Error, IsError, Aborted, Images}`, all stored as `entry.Data json.RawMessage`.

Test fakes available IN PACKAGE `runtime` (same package as runturn_test.go):
- `scriptedStreamLLM{events []scriptedStreamEvent}` + `scriptedStreamEvent{typ, text, toolCall, delay}` (streaming_test.go) — emits scripted events on call 1, bare `EventDone` after.
- `capturingLLMStub{llmtest.Base; onChatStream func(req llm.ChatRequest)}` (agent_test.go) — calls `onChatStream(req)` then emits one `EventDone`. **Use this to capture the system-prompt parts.**
- `echoTool` + `newEchoRegistry()` (runturn_test.go).

---

## File structure

| File | Repo | Responsibility |
|---|---|---|
| `runtime/runturn.go` | harness | The wiring: recall block + completing-round ingest, gated on `r.KG != nil`. Adds `time` import. |
| `runtime/runturn.go` (`sessionThread` helper) | harness | Convert `[]session.SessionEntry` → `[]Message`, mirroring Run's rendering. |
| `runtime/runturn_kg_test.go` (new) | harness | Hermetic unit tests with a fake KG + capturing LLM. |
| `test/kg_runturn_e2e_test.go` (new) | runtime | Through-serve integration regression test. |
| `README.md`, `ROADMAP.md`, M3 spec, project-memory note | runtime | Doc reconciliation. |

---

## Task 1: `sessionThread` helper (harness)

A pure function converting session entries to `[]Message`. Done first because the ingest wiring (Task 3) depends on it, and it's independently testable.

**Files:**
- Modify: `/Users/sausheong/projects/harness/runtime/runturn.go`
- Create: `/Users/sausheong/projects/harness/runtime/runturn_kg_test.go`

- [ ] **Step 1: Write the failing test**

Create `/Users/sausheong/projects/harness/runtime/runturn_kg_test.go`:

```go
package runtime

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/sausheong/harness/llm"
	"github.com/sausheong/harness/session"
	"github.com/stretchr/testify/require"
)

func TestSessionThread_RendersEntries(t *testing.T) {
	sess := session.NewSession("a", "k")
	sess.Append(session.UserMessageEntry("hello"))
	sess.Append(session.AssistantMessageEntry("hi there"))
	sess.Append(session.ToolCallEntry("tc1", "echo", json.RawMessage(`{"x":1}`)))
	sess.Append(session.ToolResultEntry("tc1", "echoed", "", nil))
	sess.Append(session.ToolResultEntry("tc2", "", "boom", nil))
	// A compaction entry must be skipped (not a conversation turn).
	sess.Append(session.CompactionEntry("summary", "", "", "m", 0, 0, 0))

	thread := sessionThread(sess.View())

	require.Equal(t, []Message{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi there"},
		{Role: "assistant", Content: "[tool: echo]\n{\"x\":1}"},
		{Role: "user", Content: "echoed"},
		{Role: "user", Content: "[error] boom"},
	}, thread)
}

func TestSessionThread_Empty(t *testing.T) {
	require.Nil(t, sessionThread(nil))
}
```

NOTE: `session.CompactionEntry` is the most-recent-compaction terminator in `View()`'s walk-back. Because `View()` stops the back-walk AT the most recent compaction (inclusive), appending it last means `View()` returns all six entries with the compaction last — verify by running; if `View()` excludes earlier entries, adjust the test to append the compaction FIRST instead and assert the five non-compaction entries render. (Run Step 2 to see actual `View()` output before finalizing the expected slice.)

- [ ] **Step 2: Run to verify it fails**

Run: `go -C /Users/sausheong/projects/harness test ./runtime/ -run TestSessionThread -v`
Expected: FAIL — `undefined: sessionThread`.

- [ ] **Step 3: Add the helper to `runturn.go`**

Add to the import block: `"encoding/json"` (for decoding entry data) — check the existing imports first; `runturn.go` currently imports `context`, `encoding/base64`, `fmt`, `sort`, `strings`, `github.com/sausheong/harness/llm`, `github.com/sausheong/harness/session`. Add `encoding/json`.

Append this function to `runturn.go` (after `turnSlice`):

```go
// sessionThread converts session history into the minimal []Message the
// KnowledgeGraph ingests, mirroring how Run accumulates its thread: user and
// assistant messages by role+text, tool calls as "[tool: name]\n<input>"
// (assistant), tool results as their output or "[error] <err>" (user).
// Compaction/meta and non-user/assistant message entries are skipped — the
// thread carries only conversation turns and tool exchanges.
func sessionThread(history []session.SessionEntry) []Message {
	var thread []Message
	for _, e := range history {
		switch e.Type {
		case session.EntryTypeMessage:
			if e.Role != "user" && e.Role != "assistant" {
				continue // skip system/summary messages
			}
			var d session.MessageData
			if json.Unmarshal(e.Data, &d) != nil {
				continue
			}
			thread = append(thread, Message{Role: e.Role, Content: d.Text})
		case session.EntryTypeToolCall:
			var d session.ToolCallData
			if json.Unmarshal(e.Data, &d) != nil {
				continue
			}
			thread = append(thread, Message{
				Role:    "assistant",
				Content: fmt.Sprintf("[tool: %s]\n%s", d.Tool, string(d.Input)),
			})
		case session.EntryTypeToolResult:
			var d session.ToolResultData
			if json.Unmarshal(e.Data, &d) != nil {
				continue
			}
			content := d.Output
			if d.Error != "" {
				content = "[error] " + d.Error
			}
			thread = append(thread, Message{Role: "user", Content: content})
		}
	}
	return thread
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go -C /Users/sausheong/projects/harness test ./runtime/ -run TestSessionThread -v`
Expected: PASS. If the compaction-walk-back assumption was wrong (Step 1 note), fix the test's entry order/expectations to match `View()`'s real output, then re-run.

- [ ] **Step 5: Build + vet**

Run: `go -C /Users/sausheong/projects/harness build ./... && go -C /Users/sausheong/projects/harness vet ./runtime/`
Expected: exit 0.

- [ ] **Step 6: Commit (harness)**

```bash
git -C /Users/sausheong/projects/harness add runtime/runturn.go runtime/runturn_kg_test.go
git -C /Users/sausheong/projects/harness -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "feat(runtime): sessionThread helper for KG ingest in RunTurn

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 2: Recall wiring in RunTurn (harness)

Bounded-synchronous recall on the first round, folded into the prompt as a non-cached suffix.

**Files:**
- Modify: `/Users/sausheong/projects/harness/runtime/runturn.go`
- Modify: `/Users/sausheong/projects/harness/runtime/runturn_kg_test.go`

- [ ] **Step 1: Write the failing tests**

First add a shared fake KG to `runturn_kg_test.go` (append):

```go
// fakeKG is a controllable KnowledgeGraph for RunTurn wiring tests.
type fakeKG struct {
	shouldRecall bool
	recallReturn string
	recallDelay  time.Duration
	recallCalls  int
	recallQuery  string

	ingestCalls   int
	ingestThreads [][]Message
}

func (f *fakeKG) ShouldRecall(q string) bool { return f.shouldRecall }
func (f *fakeKG) Recall(ctx context.Context, q string) string {
	f.recallCalls++
	f.recallQuery = q
	if f.recallDelay > 0 {
		select {
		case <-time.After(f.recallDelay):
		case <-ctx.Done():
			return ""
		}
	}
	return f.recallReturn
}
func (f *fakeKG) Ingest(_ context.Context, thread []Message) {
	f.ingestCalls++
	f.ingestThreads = append(f.ingestThreads, thread)
}
```

Then the recall tests:

```go
func TestRunTurn_RecallInjectedIntoPrompt(t *testing.T) {
	var captured []llm.SystemPromptPart
	cap := &capturingLLMStub{onChatStream: func(req llm.ChatRequest) { captured = req.SystemPromptParts }}
	kg := &fakeKG{shouldRecall: true, recallReturn: "Relevant memories:\n- the user likes tea\n"}

	rt := &Runtime{
		LLM: cap, Tools: newEchoRegistry(), Session: session.NewSession("a", "k"),
		AgentID: "a", Model: "test", StaticSystemPrompt: "STATIC", KG: kg,
	}
	_, err := rt.RunTurn(context.Background(), "what do I like?", nil, nil)
	require.NoError(t, err)
	require.Equal(t, 1, kg.recallCalls)
	require.Equal(t, "what do I like?", kg.recallQuery)

	// The static part is cached; the recall hint is a separate, non-cached part.
	require.GreaterOrEqual(t, len(captured), 2, "expected static + recall parts")
	require.Equal(t, "STATIC", captured[0].Text)
	require.True(t, captured[0].Cache)
	require.Equal(t, "Relevant memories:\n- the user likes tea\n", captured[len(captured)-1].Text)
	require.False(t, captured[len(captured)-1].Cache, "recall hint must NOT be cached")
}

func TestRunTurn_NoRecallWhenShouldRecallFalse(t *testing.T) {
	var captured []llm.SystemPromptPart
	cap := &capturingLLMStub{onChatStream: func(req llm.ChatRequest) { captured = req.SystemPromptParts }}
	kg := &fakeKG{shouldRecall: false, recallReturn: "should not appear"}

	rt := &Runtime{
		LLM: cap, Tools: newEchoRegistry(), Session: session.NewSession("a", "k"),
		AgentID: "a", Model: "test", StaticSystemPrompt: "STATIC", KG: kg,
	}
	_, err := rt.RunTurn(context.Background(), "hi", nil, nil)
	require.NoError(t, err)
	require.Equal(t, 0, kg.recallCalls, "ShouldRecall false ⇒ Recall not called")
	require.Len(t, captured, 1, "only the static part")
	require.Equal(t, "STATIC", captured[0].Text)
}

func TestRunTurn_NoRecallOnContinuationRound(t *testing.T) {
	var captured []llm.SystemPromptPart
	cap := &capturingLLMStub{onChatStream: func(req llm.ChatRequest) { captured = req.SystemPromptParts }}
	kg := &fakeKG{shouldRecall: true, recallReturn: "hint"}

	rt := &Runtime{
		LLM: cap, Tools: newEchoRegistry(), Session: session.NewSession("a", "k"),
		AgentID: "a", Model: "test", StaticSystemPrompt: "STATIC", KG: kg,
	}
	// Continuation round: empty userMsg, no images.
	_, err := rt.RunTurn(context.Background(), "", nil, nil)
	require.NoError(t, err)
	require.Equal(t, 0, kg.recallCalls, "continuation round must not recall")
	require.Len(t, captured, 1)
}

func TestRunTurn_RecallTimeoutInjectsNothing(t *testing.T) {
	var captured []llm.SystemPromptPart
	cap := &capturingLLMStub{onChatStream: func(req llm.ChatRequest) { captured = req.SystemPromptParts }}
	// Recall blocks well past the 800ms cap; the bounded ctx cancels it ⇒ "".
	kg := &fakeKG{shouldRecall: true, recallReturn: "late", recallDelay: 2 * time.Second}

	rt := &Runtime{
		LLM: cap, Tools: newEchoRegistry(), Session: session.NewSession("a", "k"),
		AgentID: "a", Model: "test", StaticSystemPrompt: "STATIC", KG: kg,
	}
	start := time.Now()
	_, err := rt.RunTurn(context.Background(), "slow?", nil, nil)
	require.NoError(t, err)
	require.Less(t, time.Since(start), 1500*time.Millisecond, "recall must be bounded ~800ms, not block on the 2s delay")
	require.Len(t, captured, 1, "timed-out recall injects nothing")
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go -C /Users/sausheong/projects/harness test ./runtime/ -run 'TestRunTurn_Recall|TestRunTurn_NoRecall' -v`
Expected: FAIL — recall not yet wired (no `Recall` call; only 1 prompt part).

- [ ] **Step 3: Wire recall in `runturn.go`**

(a) Ensure `time` is imported (add `"time"` to the import block).

(b) Find this block (currently around lines 64-73):

```go
	history := r.Session.View()
	msgs := assembleMessages(history)
	toolDefs := r.Tools.ToolDefs()
	if r.Permission != nil {
		toolDefs = r.Permission.FilterToolDefs(toolDefs, r.AgentID)
	}
	sort.SliceStable(toolDefs, func(i, j int) bool { return toolDefs[i].Name < toolDefs[j].Name })
	toolDefs, _ = r.LLM.NormalizeToolSchema(toolDefs)

	parts := []llm.SystemPromptPart{{Text: r.StaticSystemPrompt, Cache: true}}
```

Replace the LAST line (`parts := ...`) with the recall block + the parts assembly:

```go
	// KG recall (first round of the exchange only): bounded-synchronous so a
	// slow embedder cannot stall the turn. The hint is a non-cached suffix —
	// it varies per query, so caching it would poison the static prefix cache.
	var kgHint string
	if r.KG != nil && (userMsg != "" || len(images) > 0) && r.KG.ShouldRecall(userMsg) {
		rctx, cancel := context.WithTimeout(ctx, 800*time.Millisecond)
		kgHint = r.KG.Recall(rctx, userMsg)
		cancel()
	}

	parts := []llm.SystemPromptPart{{Text: r.StaticSystemPrompt, Cache: true}}
	if kgHint != "" {
		parts = append(parts, llm.SystemPromptPart{Text: kgHint, Cache: false})
	}
```

- [ ] **Step 4: Run to verify they pass**

Run: `go -C /Users/sausheong/projects/harness test ./runtime/ -run 'TestRunTurn_Recall|TestRunTurn_NoRecall' -v`
Expected: PASS.

- [ ] **Step 5: Full runturn suite + race**

Run:
```bash
go -C /Users/sausheong/projects/harness test ./runtime/ -run TestRunTurn
go -C /Users/sausheong/projects/harness test -race ./runtime/ -run 'TestRunTurn_Recall'
```
Expected: PASS (existing RunTurn tests still green; no race).

- [ ] **Step 6: Commit (harness)**

```bash
git -C /Users/sausheong/projects/harness add runtime/runturn.go runtime/runturn_kg_test.go
git -C /Users/sausheong/projects/harness -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "feat(runtime): bounded-synchronous KG recall in RunTurn

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 3: Ingest wiring in RunTurn (harness)

Fire `KG.Ingest` once, on the completing (no-tool-calls) round, over the session thread.

**Files:**
- Modify: `/Users/sausheong/projects/harness/runtime/runturn.go`
- Modify: `/Users/sausheong/projects/harness/runtime/runturn_kg_test.go`

- [ ] **Step 1: Write the failing tests** (append to `runturn_kg_test.go`)

```go
func TestRunTurn_IngestOnCompletion(t *testing.T) {
	llmProvider := &scriptedStreamLLM{events: []scriptedStreamEvent{
		{typ: llm.EventTextDelta, text: "done"},
		{typ: llm.EventDone},
	}}
	kg := &fakeKG{}
	rt := &Runtime{
		LLM: llmProvider, Tools: newEchoRegistry(), Session: session.NewSession("a", "k"),
		AgentID: "a", Model: "test", StaticSystemPrompt: "S", KG: kg,
	}
	res, err := rt.RunTurn(context.Background(), "remember I like tea", nil, nil)
	require.NoError(t, err)
	require.True(t, res.Done)
	require.Equal(t, 1, kg.ingestCalls, "ingest fires once on the completing round")
	require.NotEmpty(t, kg.ingestThreads[0], "ingest gets a non-empty thread")
	// The thread is derived from the full session (user + assistant).
	require.Equal(t, "user", kg.ingestThreads[0][0].Role)
	require.Equal(t, "remember I like tea", kg.ingestThreads[0][0].Content)
}

func TestRunTurn_NoIngestOnToolRound(t *testing.T) {
	tc := &llm.ToolCall{ID: "tc_1", Name: "echo", Input: json.RawMessage(`{"x":1}`)}
	llmProvider := &scriptedStreamLLM{events: []scriptedStreamEvent{
		{typ: llm.EventToolCallStart, toolCall: tc},
		{typ: llm.EventToolCallDone, toolCall: tc},
		{typ: llm.EventDone},
	}}
	kg := &fakeKG{}
	rt := &Runtime{
		LLM: llmProvider, Tools: newEchoRegistry(), Session: session.NewSession("a", "k"),
		AgentID: "a", Model: "test", StaticSystemPrompt: "S", KG: kg,
	}
	res, err := rt.RunTurn(context.Background(), "use echo", nil, nil)
	require.NoError(t, err)
	require.False(t, res.Done, "tool round is not terminal")
	require.Equal(t, 0, kg.ingestCalls, "no ingest on a round that produced tool calls")
}

func TestRunTurn_NoIngestWhenKGNil(t *testing.T) {
	// Sanity: KG nil ⇒ no panic, behaves as before (covered by existing tests too).
	llmProvider := &scriptedStreamLLM{events: []scriptedStreamEvent{
		{typ: llm.EventTextDelta, text: "done"},
		{typ: llm.EventDone},
	}}
	rt := &Runtime{
		LLM: llmProvider, Tools: newEchoRegistry(), Session: session.NewSession("a", "k"),
		AgentID: "a", Model: "test", StaticSystemPrompt: "S", KG: nil,
	}
	res, err := rt.RunTurn(context.Background(), "hi", nil, nil)
	require.NoError(t, err)
	require.True(t, res.Done)
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go -C /Users/sausheong/projects/harness test ./runtime/ -run 'TestRunTurn_Ingest|TestRunTurn_NoIngest' -v`
Expected: FAIL — `TestRunTurn_IngestOnCompletion` fails (ingestCalls 0). The two "no ingest" tests may pass trivially (KG never called), but `IngestOnCompletion` drives the implementation.

- [ ] **Step 3: Wire ingest in `runturn.go`**

Find the completing-round return (currently lines 124-127):

```go
	if len(toolCalls) == 0 {
		emit(AgentEvent{Type: EventDone, Usage: lastUsage})
		return r.turnSlice(startLen, true, "completed", lastUsage, nil), nil
	}
```

Replace with:

```go
	if len(toolCalls) == 0 {
		// Exchange complete: ingest the full thread once. Background + best-effort
		// (the KG spawns its own bounded goroutine), so it never delays the turn.
		// context.Background() because the request ctx may be cancelling and the
		// ingest goroutine deliberately outlives the turn.
		if r.KG != nil {
			r.KG.Ingest(context.Background(), sessionThread(r.Session.View()))
		}
		emit(AgentEvent{Type: EventDone, Usage: lastUsage})
		return r.turnSlice(startLen, true, "completed", lastUsage, nil), nil
	}
```

Do NOT add ingest to the abort/error returns — only clean completion ingests (spec decision).

- [ ] **Step 4: Run to verify they pass**

Run: `go -C /Users/sausheong/projects/harness test ./runtime/ -run 'TestRunTurn_Ingest|TestRunTurn_NoIngest' -v`
Expected: PASS.

- [ ] **Step 5: Full runturn suite + vet + race**

Run:
```bash
go -C /Users/sausheong/projects/harness build ./...
go -C /Users/sausheong/projects/harness vet ./runtime/
go -C /Users/sausheong/projects/harness test ./runtime/ -run TestRunTurn
go -C /Users/sausheong/projects/harness test -race ./runtime/ -run 'TestRunTurn|TestSessionThread'
```
Expected: all PASS (incl. the 4 pre-existing RunTurn tests); no race.

- [ ] **Step 6: Commit (harness)**

```bash
git -C /Users/sausheong/projects/harness add runtime/runturn.go runtime/runturn_kg_test.go
git -C /Users/sausheong/projects/harness -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "feat(runtime): KG ingest on RunTurn exchange completion

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 4: Harness regression guard — whole-package tests (harness)

Confirm the harness change didn't disturb `Run`'s own KG behavior or anything else.

**Files:** none (verification + a single commit only if something needs fixing).

- [ ] **Step 1: Run the full harness runtime package**

Run: `go -C /Users/sausheong/projects/harness test ./runtime/`
Expected: PASS (the entire package, not just RunTurn — catches any accidental interaction with `Run`'s recall/ingest tests).

- [ ] **Step 2: Run the whole harness module**

Run: `go -C /Users/sausheong/projects/harness test ./...`
Expected: PASS. If any pre-existing test is environment-dependent (e.g. needs network/live API) and fails for that reason, note it explicitly and confirm it also fails on the base `main` branch (i.e. not caused by this change). Do not "fix" unrelated flakes.

- [ ] **Step 3: Confirm `Message`/interface unchanged**

Run: `git -C /Users/sausheong/projects/harness diff main -- runtime/types.go runtime/builder.go`
Expected: EMPTY — this milestone changes only `runturn.go` (+ its test). The `KnowledgeGraph` interface, `Message`, and `BuildRuntime` are untouched. If the diff is non-empty, STOP and report (scope creep).

No commit unless a fix was required; if so, commit it with a clear message.

---

## Task 5: Through-serve regression test (runtime)

The test the final review demanded: drive a real exchange through the runtime's `RunTurn` serve path with an ingest+recall-enabled KG, asserting both fire. Build the Runtime exactly as `agentruntime.buildRuntime` does (via `hrt.BuildRuntime` with `KGFn`), then call `RunTurn` the way the serve loop does — this exercises the real wiring without standing up the full DBOS workflow.

**Files:**
- Create: `/Users/sausheong/projects/runtime/test/kg_runturn_e2e_test.go`

**Context:** package `test` already defines (in sibling `//go:build integration` files): `dsn`; and in `memory_ingest_e2e_test.go`: `ingestEmbedder`, `fixedExtractor`, `waitForContent`, `countContent`. **Reuse them — do NOT redefine.** This new file adds only what's new: a request-capturing provider, and the test.

- [ ] **Step 1: Write the test**

Create `/Users/sausheong/projects/runtime/test/kg_runturn_e2e_test.go`:

```go
//go:build integration

package test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/sausheong/harness/llm"
	hrt "github.com/sausheong/harness/runtime"
	"github.com/sausheong/harness/session"

	"github.com/sausheong/runtime/internal/memory"
)

// captureProvider records the system-prompt parts of the last ChatStream
// request and emits a single assistant text message (no tool calls ⇒ the turn
// completes in one round).
type captureProvider struct {
	lastParts []llm.SystemPromptPart
	reply     string
}

func (captureProvider) Models() []llm.ModelInfo { return nil }
func (captureProvider) NormalizeToolSchema(t []llm.ToolDef) ([]llm.ToolDef, []llm.Diagnostic) {
	return t, nil
}
func (p *captureProvider) ChatStream(_ context.Context, req llm.ChatRequest) (<-chan llm.ChatEvent, error) {
	p.lastParts = req.SystemPromptParts
	ch := make(chan llm.ChatEvent, 2)
	ch <- llm.ChatEvent{Type: llm.EventTextDelta, Text: p.reply}
	ch <- llm.ChatEvent{Type: llm.EventDone}
	close(ch)
	return ch, nil
}

// buildKGRuntime builds a harness Runtime wired the way agentruntime.buildRuntime
// does: KGFn supplies the tenant-pinned KG, bound to the given session.
func buildKGRuntime(t *testing.T, prov llm.LLMProvider, kg hrt.KnowledgeGraph, sess *session.Session) *hrt.Runtime {
	t.Helper()
	rt, err := hrt.BuildRuntime(
		hrt.RuntimeDeps{KGFn: func(string) hrt.KnowledgeGraph { return kg }},
		hrt.RuntimeInputs{Provider: prov, Tools: toolRegistryForKGTest(), Session: sess},
		hrt.AgentSpec{ID: "a", Name: "a", Model: "test/scripted", MaxTurns: 10},
	)
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

func TestKGRunTurnE2E_IngestAndRecallOnServePath(t *testing.T) {
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

	const fact = "the user lives in Singapore"
	emb := ingestEmbedder{vecs: map[string][]float32{
		fact:                        {1, 0, 0},
		"where does the user live?": {1, 0, 0},
	}}
	st, err := memory.NewStore(ctx, db, "alpha", memory.WithEmbedder(emb))
	if err != nil {
		t.Fatal(err)
	}
	ext := fixedExtractor{facts: []string{fact}}
	kg := memory.NewKG(st, 5, 0.5, memory.WithIngest(ext, 0.85, 2, 4))

	// --- Exchange 1: drive a turn through RunTurn; ingest must fire on completion.
	prov := &captureProvider{reply: "noted"}
	sess1 := session.NewSession("a", "alpha")
	rt1 := buildKGRuntime(t, prov, kg, sess1)
	res, err := rt1.RunTurn(ctx, "I live in Singapore", nil, nil)
	if err != nil || !res.Done {
		t.Fatalf("turn 1: err=%v done=%v", err, res.Done)
	}
	waitForContent(t, st, fact) // ingest is async; poll until durable

	// --- Exchange 2: a fresh turn; recall must inject the fact into the prompt.
	prov2 := &captureProvider{reply: "ok"}
	sess2 := session.NewSession("a", "alpha")
	rt2 := buildKGRuntime(t, prov2, kg, sess2)
	if _, err := rt2.RunTurn(ctx, "where does the user live?", nil, nil); err != nil {
		t.Fatal(err)
	}
	var joined string
	for _, p := range prov2.lastParts {
		joined += p.Text + "\n"
	}
	if !strings.Contains(joined, "Singapore") {
		t.Fatalf("recall hint should reach the prompt on the serve path; parts=%q", joined)
	}
}
```

Add the tool-registry helper at the bottom of the file (RunTurn needs a non-nil `Tools`):

```go
func toolRegistryForKGTest() *tool.Registry {
	return tool.NewRegistry()
}
```

and add `"github.com/sausheong/harness/tool"` to the imports.

- [ ] **Step 2: Run the test**

Run: `go -C /Users/sausheong/projects/runtime test -tags integration ./test/ -run TestKGRunTurnE2E -v 2>&1 | tail -25`
Expected: PASS (real Postgres). A "postgres not reachable" SKIP is NOT a pass — ensure Postgres.app + the `vector` extension are up. If `BuildRuntime` or the provider interface needs a method you missed, the compiler will say so via `go test`; fix minimally (the `llm.LLMProvider` interface is `ChatStream` + `NormalizeToolSchema` + `Models`).

- [ ] **Step 3: Full runtime integration package**

Run: `go -C /Users/sausheong/projects/runtime test -tags integration ./test/ 2>&1 | tail -25`
Expected: PASS (all existing e2e + the new one). Note any pre-existing unrelated failure explicitly.

- [ ] **Step 4: Commit (runtime)**

```bash
git -C /Users/sausheong/projects/runtime add test/kg_runturn_e2e_test.go
git -C /Users/sausheong/projects/runtime -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "test(memory): through-serve regression test for KG recall+ingest on RunTurn

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 6: Documentation reconciliation (runtime)

**Files:**
- Modify: `/Users/sausheong/projects/runtime/README.md`
- Modify: `/Users/sausheong/projects/runtime/ROADMAP.md`
- Modify: `/Users/sausheong/projects/runtime/docs/superpowers/specs/2026-06-09-memory-m3-auto-ingestion-design.md`

- [ ] **Step 1: M3 spec addendum**

Append to the END of `docs/superpowers/specs/2026-06-09-memory-m3-auto-ingestion-design.md`:

```markdown

---

## Addendum (2026-06-09): RunTurn wiring correction

This spec's "Why the seam forces async" section assumed harness fires `Ingest`
inside `RunTurn`. That was not true: `RunTurn` did not consult `r.KG` at all, so
both M3 ingest and M2 recall were inert on the runtime's serve path. The M3 final
holistic review caught this. The fix is a separate milestone —
`docs/superpowers/specs/2026-06-09-kg-runturn-wiring-design.md` — which wires the
KG seam into `RunTurn` (bounded recall on the first round, ingest on the
completing round). M3 merges together with that wiring so the feature is live, not
dormant. The async/degrade/dedup design above is unchanged and correct; only the
call site was missing.
```

- [ ] **Step 2: README — adjust the auto-ingestion subsection**

In `README.md`, find this sentence in the `#### Auto-ingestion` subsection:

```
When semantic recall is enabled **and** `RUNTIME_INGEST_ENABLED` is set, the
agent also *captures* memories automatically. After each chat turn, a background
extractor reads the conversation, pulls out durable facts, dedups them against
existing memory, and saves the new ones — which embed-on-save makes recallable on
the next turn. The agent does not have to call the memory tool to remember.
```

It is accurate as written (auto-ingestion now genuinely runs after each completed
chat exchange). No change needed unless it implies more than per-exchange — verify
the wording says "after each chat turn"; if you want precision, change "After each
chat turn" to "After each completed chat exchange". Apply that one-word-precision
edit:

Replace `After each chat turn, a background extractor` with `After each completed chat exchange, a background extractor`.

- [ ] **Step 3: ROADMAP — record the wiring fix under §B2**

In `ROADMAP.md`, find the end of the M3 "Third milestone DONE" paragraph (the line
ending `...2026-06-09-memory-m3-auto-ingestion*`.) and insert AFTER it:

```markdown

   **Wiring correction (merged with M3):** the M3 final review found harness's
   `RunTurn` (the runtime's sole turn executor) never consulted `r.KG`, so M2
   recall AND M3 ingest were inert on the serve path. Fixed by wiring the KG seam
   into `RunTurn` (bounded-synchronous recall on the first round, ingest on the
   completing round); replay-safe because `RunTurn` runs inside the DBOS step.
   Harness `RunTurn` is owned code, so this was an in-scope change. A through-serve
   integration test now guards the path. Spec:
   `docs/superpowers/{specs}/2026-06-09-kg-runturn-wiring-design.md`.
```

- [ ] **Step 4: Verify + build**

Run:
```bash
go -C /Users/sausheong/projects/runtime build ./...
grep -n "RunTurn wiring correction\|Wiring correction\|completed chat exchange" /Users/sausheong/projects/runtime/docs/superpowers/specs/2026-06-09-memory-m3-auto-ingestion-design.md /Users/sausheong/projects/runtime/ROADMAP.md /Users/sausheong/projects/runtime/README.md
```
Expected: build exit 0; grep shows the three new anchors.

- [ ] **Step 5: Commit (runtime)**

```bash
git -C /Users/sausheong/projects/runtime add README.md ROADMAP.md docs/superpowers/specs/2026-06-09-memory-m3-auto-ingestion-design.md
git -C /Users/sausheong/projects/runtime -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "docs(memory): reconcile docs with KG→RunTurn wiring

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Final verification (after all tasks)

- [ ] **Harness:**
```bash
go -C /Users/sausheong/projects/harness build ./...
go -C /Users/sausheong/projects/harness vet ./runtime/
go -C /Users/sausheong/projects/harness test ./runtime/
go -C /Users/sausheong/projects/harness test -race ./runtime/ -run 'TestRunTurn|TestSessionThread'
```
- [ ] **Runtime (hermetic + integration):**
```bash
go -C /Users/sausheong/projects/runtime build ./...
go -C /Users/sausheong/projects/runtime vet ./...
go -C /Users/sausheong/projects/runtime test ./internal/memory/ ./internal/agentkind/ ./agentruntime/
go -C /Users/sausheong/projects/runtime test -tags integration ./internal/memory/
go -C /Users/sausheong/projects/runtime test -tags integration ./test/
```
Expected: all PASS (skips on integration ⇒ Postgres not running; fix and re-run).

- [ ] **Then:** REQUIRED SUB-SKILL `superpowers:finishing-a-development-branch`. NOTE: this finishes BOTH branches — the harness `feat/kg-runturn-wiring` and the runtime `feat/memory-m3-auto-ingestion` (which carries M3 + this wiring + the regression test). The harness branch merges to harness `main`; the runtime branch merges to runtime `master`. Surface both to the user at finish time.

---

## Self-review notes (plan author)

- **Spec coverage:** `sessionThread` (T1) ↔ spec "sessionThread helper". Recall wiring + non-cached suffix + 800ms bound + first-round gate (T2) ↔ spec "Recall — first round". Ingest on completion, once, context.Background, not on tool/abort rounds (T3) ↔ spec "Ingest — completing round". Whole-harness regression (T4) ↔ spec "Existing suites must stay green" + the no-interface-change guarantee. Through-serve test (T5) ↔ spec "Runtime through-serve integration test" (the review's required regression guard). Docs (T6) ↔ spec "Documentation updates".
- **Two-repo handling:** harness wiring on `feat/kg-runturn-wiring`; runtime test+docs on `feat/memory-m3-auto-ingestion`; `replace ../harness` means runtime tests see uncommitted harness edits, so T5 works even before harness is merged. All commands use `go -C`/`git -C` (no `cd`).
- **Type consistency:** `sessionThread(history []session.SessionEntry) []Message` (T1) is called in T3's ingest line and matches harness's in-package `Message` type. The recall block uses `r.KG.ShouldRecall/Recall` and `llm.SystemPromptPart{Text,Cache}` (verified fields). T5 uses `hrt.BuildRuntime(RuntimeDeps{KGFn}, RuntimeInputs{Provider,Tools,Session}, AgentSpec{...})` — the real signature (builder.go), and `llm.LLMProvider` = `ChatStream`+`NormalizeToolSchema`+`Models`.
- **Reuse, no duplication:** T5 reuses `ingestEmbedder`/`fixedExtractor`/`waitForContent` from `test/memory_ingest_e2e_test.go` (same package) and adds only `captureProvider` + the test. `runturn_kg_test.go` reuses `scriptedStreamLLM`/`capturingLLMStub`/`echoTool`/`newEchoRegistry` from the harness `runtime` package.
- **Deliberate deviation:** the `View()`-compaction interaction in T1's test is flagged for runtime verification (Step 1 note) rather than assumed — the implementer adjusts the expected slice to the real `View()` output.
```
