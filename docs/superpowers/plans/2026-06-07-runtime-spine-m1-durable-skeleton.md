# Runtime Spine — Milestone 1: Durable Walking Skeleton — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Prove end-to-end that a harness agent can run as a supervised subprocess with a durable, resume-from-last-turn loop backed by DBOS+Postgres — the single most important risk in the whole platform.

**Architecture:** Three layers. (1) A small new **per-turn API** on harness (`Runtime.RunTurn`) that executes exactly one turn and returns the session entries it produced. (2) An **agent-runtime SDK** (`agentruntime`) that drives turns inside a DBOS workflow — each turn is a `RunAsStep` whose checkpointed return value is the turn's entries, so on crash-replay the session is rebuilt from checkpoints without re-calling the LLM or re-running committed tools. (3) A **minimal control plane** that spawns one agent subprocess, proxies `invoke` over SSE, and restarts the subprocess on crash. The flagship acceptance test kills the subprocess mid-turn and asserts the session resumes from the correct turn with no duplicated tool calls.

**Tech Stack:** Go 1.25.1+, `github.com/sausheong/harness` (owned, modified here), `github.com/dbos-inc/dbos-transact-golang/dbos`, Postgres 16 (+ pgvector image), `net/http` + SSE (no web framework), `database/sql` + `github.com/jackc/pgx/v5/stdlib`.

**Scope note:** This is Milestone 1 of the Approach-2 spec (`docs/superpowers/specs/2026-06-07-runtime-spine-design.md`). Milestones 2 (multi-agent registry, pools, full CLI, console) and 3 (token auth, conformance suite, Compose polish) get their own plans. This plan deliberately hardcodes a single statically-configured agent and a single subprocess.

**Repos touched:** `harness/` (add `RunTurn`) and `runtime/` (new platform code). Both live under `/Users/sausheong/projects/`. The `runtime/` module imports harness via a `replace` directive pointing at the local checkout so harness changes are picked up without tagging a release.

---

## File Structure

### harness/ (modify)
- `runtime/runturn.go` — **Create.** `RunTurn(ctx, userMsg, images, emit) (TurnResult, error)` and helpers; one-turn execution extracted to be callable in a loop. Reuses existing unexported helpers in `runtime.go`.
- `runtime/runturn_test.go` — **Create.** Unit tests for single-turn execution and the "no tool calls ⇒ Done" terminal.

### runtime/ (new module, all created)
- `go.mod` / `go.sum` — module `github.com/sausheong/runtime`, `replace github.com/sausheong/harness => ../harness`.
- `internal/store/store.go` — Postgres-backed control-plane store: agents/deployments/sessions/session_events. `Store` interface + `pgStore` impl.
- `internal/store/memstore.go` — in-memory `Store` for hermetic tests.
- `internal/store/schema.sql` — DDL for control-plane tables (embedded via `//go:embed`).
- `internal/store/store_test.go` — store contract tests run against the in-memory impl.
- `agentruntime/config.go` — `Config` struct + validation.
- `agentruntime/turnstep.go` — DBOS workflow that loops `RunTurn` as steps; `turnInput`/`turnOutput` serializable step payloads; `WireEvent` type; `applyEntries`/`publishableEvents` session-rebuild helpers.
- `agentruntime/server.go` — HTTP contract for M1: `POST /sessions` (create+invoke combined), `GET /sessions/{id}/stream` (re-attach SSE), `GET /sessions/{id}` (status), `GET /healthz`, `GET /meta`. **Deferred to M2:** the split `POST /sessions/{id}/invoke` and `POST /sessions/{id}/cancel` endpoints from the spec contract — M1 proves durability with the combined create+invoke + re-attach-stream; cancel needs the workflow-cancel wiring that lands with multi-agent lifecycle. The `/meta` `contract_version` stays `"v1"` so M2 can add endpoints additively.
- `agentruntime/serve.go` — `Serve(Config)` wiring + the `Manager` type: DBOS launch, session manager, HTTP listen, graceful shutdown.
- `agentruntime/sse.go` — tiny SSE writer. (The shared `WireEvent` wire type is defined in `turnstep.go`.)
- `agentruntime/*_test.go` — unit tests for config validation, SSE encoding, the turn-step rebuild logic (with a fake provider).
- `controlplane/supervisor.go` — spawn/health/restart one subprocess.
- `controlplane/proxy.go` — invoke proxy that streams the subprocess SSE back to the caller.
- `controlplane/api.go` — control REST API (`POST /agents/{id}/sessions`, `POST .../invoke`, `GET .../stream`).
- `controlplane/*_test.go` — supervisor restart logic against a fake spawnable, proxy SSE passthrough.
- `cmd/agentd/main.go` — the **agent subprocess** binary; reads env, builds a harness agent, calls `agentruntime.Serve`.
- `cmd/runtimectl/main.go` — the **CLI**: `deploy`, `invoke`, `logs`.
- `cmd/runtimed/main.go` — the **control plane** binary.
- `test/resume_test.go` — **flagship** `//go:build integration` kill-mid-turn resume test.
- `deploy/docker-compose.yml` — Postgres (pgvector image) for local/integration.
- `testagent/` — a tiny fake-provider harness agent used by integration tests (deterministic, scriptable tool calls).

---

## Task 0: Bootstrap the runtime module

**Files:**
- Create: `runtime/go.mod`
- Create: `runtime/deploy/docker-compose.yml`
- Create: `runtime/.gitignore` (already exists from spec commit — verify)

- [ ] **Step 1: Initialize the module**

Run from `/Users/sausheong/projects/runtime`:

```bash
go mod init github.com/sausheong/runtime
go mod edit -replace github.com/sausheong/harness=../harness
go mod edit -require github.com/sausheong/harness@v0.0.0
go mod edit -require github.com/dbos-inc/dbos-transact-golang@latest
go mod edit -require github.com/jackc/pgx/v5@latest
```

- [ ] **Step 2: Add the Postgres Compose file**

Create `runtime/deploy/docker-compose.yml`:

```yaml
services:
  postgres:
    image: pgvector/pgvector:pg16
    environment:
      POSTGRES_USER: runtime
      POSTGRES_PASSWORD: runtime
      POSTGRES_DB: runtime
    ports:
      - "5432:5432"
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U runtime"]
      interval: 2s
      timeout: 3s
      retries: 20
```

- [ ] **Step 3: Bring Postgres up and resolve deps**

Run:
```bash
docker compose -f deploy/docker-compose.yml up -d
go mod tidy
```
Expected: `docker compose` prints the container as healthy within ~10s; `go mod tidy` resolves DBOS + pgx and rewrites `go.sum` with no errors.

- [ ] **Step 4: Commit**

```bash
git add go.mod go.sum deploy/docker-compose.yml
git commit -m "chore: bootstrap runtime module with harness replace + postgres compose"
```

---

## Task 1: harness `RunTurn` — single-turn execution API

This is the foundational change. `Run()` owns the whole multi-turn loop and is therefore unusable for per-turn DBOS durability. `RunTurn` executes **exactly one turn** (one LLM call + its tool batch), returns a structured result describing whether the agent is done, and reuses the already-tested `dispatchTool` / `partitionToolCalls` helpers. M1 intentionally omits compaction, streaming-tool kickoff, and KG from this path (the full `Run` keeps them); they return in a later milestone once durable correctness is proven.

**Files:**
- Create: `harness/runtime/runturn.go`
- Test: `harness/runtime/runturn_test.go`

- [ ] **Step 1: Write the failing test**

Create `harness/runtime/runturn_test.go`. This reuses the existing test fakes in the `runtime` package (a fake `llm.LLMProvider`). Check `runtime/streaming_test.go` for the exact fake-provider helper name in this codebase (e.g. `newScriptedProvider` / `fakeProvider`); the test below assumes a helper `scriptedProvider` that yields a fixed sequence of `llm.StreamEvent`s. If the existing fake has a different constructor, adapt the two `prov :=` lines only.

```go
package runtime

import (
	"context"
	"testing"

	"github.com/sausheong/harness/llm"
	"github.com/sausheong/harness/session"
	"github.com/sausheong/harness/tool"
)

func TestRunTurn_TextOnly_IsDone(t *testing.T) {
	// Provider emits text then EventDone with no tool calls ⇒ turn is terminal.
	prov := scriptedProvider([][]llm.StreamEvent{{
		{Type: llm.EventTextDelta, Text: "hello"},
		{Type: llm.EventDone, Usage: &llm.Usage{InputTokens: 3}},
	}})
	sess := session.NewSession("a", "k")
	rt := &Runtime{LLM: prov, Tools: tool.NewRegistry(), Session: sess, Model: "x", MaxTurns: 25}

	res, err := rt.RunTurn(context.Background(), "hi", nil, nil)
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if !res.Done {
		t.Fatalf("expected Done=true on a no-tool-call turn")
	}
	if res.StopReason != "completed" {
		t.Fatalf("StopReason = %q, want completed", res.StopReason)
	}
	// First turn appends: user msg + assistant msg = 2 new entries.
	if len(res.Entries) != 2 {
		t.Fatalf("len(Entries) = %d, want 2 (user+assistant)", len(res.Entries))
	}
}

func TestRunTurn_ToolCall_NotDone_ThenDone(t *testing.T) {
	reg := tool.NewRegistry()
	reg.Register(&echoTool{}) // a trivial test tool returning its input; define in this test file
	// Turn 1: model calls the tool. Turn 2: model emits text, no tools.
	prov := scriptedProvider([][]llm.StreamEvent{
		{
			{Type: llm.EventToolCallStart, ToolCall: &llm.ToolCall{ID: "t1", Name: "echo", Input: []byte(`{"msg":"x"}`)}},
			{Type: llm.EventToolCallDone, ToolCall: &llm.ToolCall{ID: "t1", Name: "echo", Input: []byte(`{"msg":"x"}`)}},
			{Type: llm.EventDone},
		},
		{
			{Type: llm.EventTextDelta, Text: "done"},
			{Type: llm.EventDone},
		},
	})
	sess := session.NewSession("a", "k")
	rt := &Runtime{LLM: prov, Tools: reg, Session: sess, Model: "x", MaxTurns: 25}

	r1, err := rt.RunTurn(context.Background(), "go", nil, nil)
	if err != nil {
		t.Fatalf("turn1: %v", err)
	}
	if r1.Done {
		t.Fatalf("turn1 should not be Done (it ran a tool)")
	}
	// turn1 entries: user + assistant(empty text omitted) + tool_call + tool_result.
	// At minimum the tool_call and tool_result must be present.
	if !hasType(r1.Entries, session.EntryTypeToolResult) {
		t.Fatalf("turn1 entries missing tool_result: %+v", r1.Entries)
	}

	r2, err := rt.RunTurn(context.Background(), "", nil, nil) // empty msg ⇒ continuation
	if err != nil {
		t.Fatalf("turn2: %v", err)
	}
	if !r2.Done {
		t.Fatalf("turn2 should be Done")
	}
}

func hasType(es []session.SessionEntry, want session.EntryType) bool {
	for _, e := range es {
		if e.Type == want {
			return true
		}
	}
	return false
}
```

You must also define `echoTool` in this test file if no equivalent exists. Minimal implementation:

```go
type echoTool struct{}

func (echoTool) Name() string                          { return "echo" }
func (echoTool) Description() string                   { return "echoes msg" }
func (echoTool) InputSchema() map[string]any           { return map[string]any{"type": "object"} }
func (echoTool) IsConcurrencySafe() bool               { return true }
func (echoTool) Execute(_ context.Context, in []byte) (tool.ToolResult, error) {
	return tool.ToolResult{Output: string(in)}, nil
}
```

Before writing, verify the `tool.Tool` interface method set in `harness/tool/tool.go` and match it exactly (method names/signatures may differ — `InputSchema` vs `Schema`, etc.). Adjust `echoTool` to satisfy the real interface.

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd /Users/sausheong/projects/harness && go test ./runtime/ -run TestRunTurn -v`
Expected: compile error / FAIL — `rt.RunTurn undefined`.

- [ ] **Step 3: Implement `RunTurn`**

Create `harness/runtime/runturn.go`. It mirrors one iteration of the `Run` loop body but: (a) takes the user message only on the first call of a session (empty string = continuation), (b) captures entries appended during the turn by snapshotting `Session.Entries()` length before/after, (c) returns rather than looping. It reuses `assembleMessages`, `dispatchTool`, and `partitionToolCalls` verbatim.

```go
package runtime

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/sausheong/harness/llm"
	"github.com/sausheong/harness/session"
)

// TurnResult describes the outcome of a single RunTurn call.
type TurnResult struct {
	// Done is true when the agent produced no tool calls this turn (the
	// loop should stop) or an unrecoverable condition was hit.
	Done bool
	// StopReason mirrors Run's reasons: "continue" (not done),
	// "completed", "error", "aborted".
	StopReason string
	// Entries are the session entries appended during THIS turn, in order.
	// On DBOS replay this is the checkpointed value the caller re-applies
	// to rebuild session state without re-executing the turn.
	Entries []session.SessionEntry
	// Err carries a turn-level error (also reflected in StopReason="error").
	Err error
	// Usage is the provider usage for the turn's LLM call, when reported.
	Usage *llm.Usage
}

// TurnEmit is an optional callback invoked for live streaming. It receives
// the same AgentEvents Run would emit. Pass nil for headless execution
// (e.g. during DBOS replay, where live emission must be suppressed).
type TurnEmit func(AgentEvent)

// RunTurn executes exactly one turn of the agent loop against the current
// session and returns the entries it produced. The first call of a session
// should pass the user message; continuation calls pass "".
//
// RunTurn is the durable unit for the agent-runtime SDK: each call is wrapped
// as a DBOS step whose checkpointed return value is the TurnResult. It
// deliberately omits compaction, KG, and streaming-tool kickoff — those live
// in Run and return to this path in a later milestone.
func (r *Runtime) RunTurn(ctx context.Context, userMsg string, images []llm.ImageContent, emit TurnEmit) (TurnResult, error) {
	if emit == nil {
		emit = func(AgentEvent) {}
	}
	startLen := len(r.Session.Entries())

	// Append the user message only when one is provided (first turn).
	if userMsg != "" || len(images) > 0 {
		if len(images) > 0 {
			var imgData []session.ImageData
			for _, img := range images {
				imgData = append(imgData, session.ImageData{
					MimeType: img.MimeType,
					Data:     encodeBase64(img.Data),
				})
			}
			r.Session.Append(session.UserMessageWithImagesEntry(userMsg, imgData))
		} else {
			r.Session.Append(session.UserMessageEntry(userMsg))
		}
	}

	if ctx.Err() != nil {
		return r.turnSlice(startLen, true, "aborted", nil, ctx.Err()), nil
	}

	// Assemble request from current session view (reuses Run's helpers).
	history := r.Session.View()
	msgs := assembleMessages(history)
	toolDefs := r.Tools.ToolDefs()
	if r.Permission != nil {
		toolDefs = r.Permission.FilterToolDefs(toolDefs, r.AgentID)
	}
	sort.SliceStable(toolDefs, func(i, j int) bool { return toolDefs[i].Name < toolDefs[j].Name })
	toolDefs, _ = r.LLM.NormalizeToolSchema(toolDefs)

	parts := []llm.SystemPromptPart{{Text: r.StaticSystemPrompt, Cache: true}}
	req := llm.ChatRequest{
		Model:             r.Model,
		Messages:          msgs,
		Tools:             toolDefs,
		MaxTokens:         8192,
		SystemPromptParts: parts,
		CacheLastMessage:  r.providerSupportsCaching(),
		Reasoning:         r.Reasoning,
	}

	stream, err := r.LLM.ChatStream(ctx, req)
	if err != nil {
		if r.FallbackModel != "" && r.FallbackModel != req.Model && llm.IsRetryableModelError(err) {
			req.Model = r.FallbackModel
			stream, err = r.LLM.ChatStream(ctx, req)
		}
		if err != nil {
			emit(AgentEvent{Type: EventError, Error: fmt.Errorf("llm error: %w", err)})
			return r.turnSlice(startLen, true, "error", nil, err), nil
		}
	}

	var textContent strings.Builder
	var toolCalls []llm.ToolCall
	var lastUsage *llm.Usage
	for event := range stream {
		switch event.Type {
		case llm.EventTextDelta:
			textContent.WriteString(event.Text)
			emit(AgentEvent{Type: EventTextDelta, Text: event.Text})
		case llm.EventToolCallStart:
			emit(AgentEvent{Type: EventToolCallStart, ToolCall: event.ToolCall})
		case llm.EventToolCallDone:
			if event.ToolCall != nil {
				toolCalls = append(toolCalls, *event.ToolCall)
			}
		case llm.EventDone:
			if event.Usage != nil {
				lastUsage = event.Usage
			}
		case llm.EventError:
			emit(AgentEvent{Type: EventError, Error: event.Error})
			return r.turnSlice(startLen, true, "error", lastUsage, event.Error), nil
		}
	}

	if textContent.Len() > 0 {
		r.Session.Append(session.AssistantMessageEntry(textContent.String()))
	}

	// Terminal: no tool calls ⇒ the loop is done.
	if len(toolCalls) == 0 {
		emit(AgentEvent{Type: EventDone, Usage: lastUsage})
		return r.turnSlice(startLen, true, "completed", lastUsage, nil), nil
	}

	// Execute tool calls using the existing, tested dispatch path. dispatchTool
	// appends the tool_call and tool_result entries to the session.
	batches := partitionToolCalls(toolCalls, r.Tools)
	for _, b := range batches {
		for _, tc := range b.calls {
			result, aborted := r.dispatchTool(ctx, tc, nil)
			emit(AgentEvent{Type: EventToolResult, Result: &result})
			if aborted {
				return r.turnSlice(startLen, true, "aborted", lastUsage, nil), nil
			}
		}
	}

	// Tool calls ran ⇒ not done; caller invokes RunTurn again.
	return r.turnSlice(startLen, false, "continue", lastUsage, nil), nil
}

// turnSlice builds a TurnResult from the entries appended since startLen.
func (r *Runtime) turnSlice(startLen int, done bool, reason string, usage *llm.Usage, err error) TurnResult {
	all := r.Session.Entries()
	var delta []session.SessionEntry
	if startLen <= len(all) {
		delta = append(delta, all[startLen:]...)
	}
	return TurnResult{Done: done, StopReason: reason, Entries: delta, Usage: usage, Err: err}
}
```

Note on `b.calls`: confirm the `batch` struct field name in `runtime/partition.go` (Task scouting showed `partitionToolCalls` returns `[]batch`). If the field is unexported with a different name, adjust the range accordingly. Also add `encodeBase64` only if no existing helper is importable — otherwise replace the two call sites with the existing `base64.StdEncoding.EncodeToString` and import `encoding/base64` (matching `runtime.go`'s usage).

- [ ] **Step 4: Run the test to verify it passes**

Run: `cd /Users/sausheong/projects/harness && go test ./runtime/ -run TestRunTurn -v`
Expected: PASS for both tests.

- [ ] **Step 5: Run the full harness suite to ensure nothing regressed**

Run: `cd /Users/sausheong/projects/harness && go test ./... && go vet ./...`
Expected: all PASS (RunTurn is additive; `Run` is untouched).

- [ ] **Step 6: Commit (in the harness repo)**

```bash
cd /Users/sausheong/projects/harness
git add runtime/runturn.go runtime/runturn_test.go
git commit -m "feat(runtime): add RunTurn single-turn API for durable drivers

Run() owns the entire multi-turn loop, which prevents per-turn durable
execution. RunTurn executes exactly one turn and returns the session
entries it produced, so a DBOS-backed driver can checkpoint each turn and
resume from the last completed one. Additive; Run() is unchanged."
```

---

## Task 2: Control-plane store (interface + in-memory + Postgres)

The store persists platform/operational state. M1 needs the minimum: a sessions row (linking to the DBOS workflow id) and an append-only `session_events` log for client re-attach. Agents/deployments are stubbed as a single hardcoded row. We define a narrow `Store` interface, an in-memory impl for hermetic tests, and a Postgres impl behind the same interface.

**Files:**
- Create: `runtime/internal/store/store.go`
- Create: `runtime/internal/store/memstore.go`
- Create: `runtime/internal/store/schema.sql`
- Create: `runtime/internal/store/pgstore.go`
- Test: `runtime/internal/store/store_test.go`

- [ ] **Step 1: Write the failing contract test**

Create `runtime/internal/store/store_test.go`:

```go
package store

import (
	"context"
	"testing"
)

func TestStore_SessionLifecycle(t *testing.T) {
	s := NewMemStore()
	ctx := context.Background()

	id, err := s.CreateSession(ctx, "agent1", "wf-123")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if id == "" {
		t.Fatal("empty session id")
	}

	got, err := s.GetSession(ctx, id)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.WorkflowID != "wf-123" || got.AgentID != "agent1" {
		t.Fatalf("session mismatch: %+v", got)
	}
	if got.Status != "created" {
		t.Fatalf("status = %q, want created", got.Status)
	}
}

func TestStore_EventLogAppendAndReplay(t *testing.T) {
	s := NewMemStore()
	ctx := context.Background()
	id, _ := s.CreateSession(ctx, "agent1", "wf-1")

	for i, typ := range []string{"text_delta", "text_delta", "done"} {
		if err := s.AppendEvent(ctx, id, typ, []byte(`{"i":`+itoa(i)+`}`)); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}

	evs, err := s.EventsSince(ctx, id, 0)
	if err != nil {
		t.Fatalf("EventsSince: %v", err)
	}
	if len(evs) != 3 {
		t.Fatalf("len(events) = %d, want 3", len(evs))
	}
	if evs[0].Seq != 1 || evs[2].Seq != 3 {
		t.Fatalf("seq not monotonic from 1: %+v", evs)
	}

	// Replay tail only.
	tail, _ := s.EventsSince(ctx, id, 2)
	if len(tail) != 1 || tail[0].Type != "done" {
		t.Fatalf("tail replay wrong: %+v", tail)
	}
}

func itoa(i int) string { return string(rune('0' + i)) }
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/store/ -v`
Expected: compile error — `NewMemStore`, `Store` undefined.

- [ ] **Step 3: Define the interface and types**

Create `runtime/internal/store/store.go`:

```go
package store

import "context"

// SessionRow is the control-plane view of a session.
type SessionRow struct {
	ID         string
	AgentID    string
	WorkflowID string
	Status     string // created | running | idle | recovering | closed | failed
	TurnCount  int
}

// Event is one row in the append-only session event log.
type Event struct {
	Seq     int64
	Type    string
	Payload []byte
}

// Store is the control-plane persistence surface needed by Milestone 1.
type Store interface {
	CreateSession(ctx context.Context, agentID, workflowID string) (string, error)
	GetSession(ctx context.Context, id string) (SessionRow, error)
	SetSessionStatus(ctx context.Context, id, status string) error
	AppendEvent(ctx context.Context, sessionID, typ string, payload []byte) error
	EventsSince(ctx context.Context, sessionID string, afterSeq int64) ([]Event, error)
	Close() error
}
```

- [ ] **Step 4: Implement the in-memory store**

Create `runtime/internal/store/memstore.go`:

```go
package store

import (
	"context"
	"fmt"
	"sync"
)

type memStore struct {
	mu       sync.Mutex
	seq      int
	sessions map[string]*SessionRow
	events   map[string][]Event
}

// NewMemStore returns a hermetic in-memory Store for tests.
func NewMemStore() Store {
	return &memStore{sessions: map[string]*SessionRow{}, events: map[string][]Event{}}
}

func (m *memStore) CreateSession(_ context.Context, agentID, workflowID string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	id := fmt.Sprintf("ses-%d", m.seq)
	m.sessions[id] = &SessionRow{ID: id, AgentID: agentID, WorkflowID: workflowID, Status: "created"}
	return id, nil
}

func (m *memStore) GetSession(_ context.Context, id string) (SessionRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return SessionRow{}, fmt.Errorf("session %q not found", id)
	}
	return *s, nil
}

func (m *memStore) SetSessionStatus(_ context.Context, id, status string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return fmt.Errorf("session %q not found", id)
	}
	s.Status = status
	return nil
}

func (m *memStore) AppendEvent(_ context.Context, sessionID, typ string, payload []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	evs := m.events[sessionID]
	next := int64(len(evs) + 1)
	cp := make([]byte, len(payload))
	copy(cp, payload)
	m.events[sessionID] = append(evs, Event{Seq: next, Type: typ, Payload: cp})
	return nil
}

func (m *memStore) EventsSince(_ context.Context, sessionID string, afterSeq int64) ([]Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Event
	for _, e := range m.events[sessionID] {
		if e.Seq > afterSeq {
			out = append(out, e)
		}
	}
	return out, nil
}

func (m *memStore) Close() error { return nil }
```

- [ ] **Step 5: Run to verify the in-memory tests pass**

Run: `cd /Users/sausheong/projects/runtime && go test ./internal/store/ -v`
Expected: PASS.

- [ ] **Step 6: Add the SQL schema**

Create `runtime/internal/store/schema.sql`:

```sql
CREATE TABLE IF NOT EXISTS agents (
    id               TEXT PRIMARY KEY,
    name             TEXT NOT NULL,
    contract_version TEXT NOT NULL DEFAULT 'v1',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS sessions (
    id          TEXT PRIMARY KEY,
    agent_id    TEXT NOT NULL,
    workflow_id TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'created',
    turn_count  INT  NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_active_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS session_events (
    session_id TEXT NOT NULL REFERENCES sessions(id),
    seq        BIGINT NOT NULL,
    type       TEXT NOT NULL,
    payload    JSONB NOT NULL,
    ts         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (session_id, seq)
);
```

- [ ] **Step 7: Implement the Postgres store**

Create `runtime/internal/store/pgstore.go`. Uses `database/sql` with the pgx stdlib driver. `seq` is allocated per session via `COALESCE(MAX(seq),0)+1` inside a transaction to keep the append-only log monotonic.

```go
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

//go:embed schema.sql
var schemaSQL string

type pgStore struct{ db *sql.DB }

// NewPGStore opens a Postgres-backed Store and applies the schema.
func NewPGStore(ctx context.Context, dsn string) (Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &pgStore{db: db}, nil
}

func (p *pgStore) CreateSession(ctx context.Context, agentID, workflowID string) (string, error) {
	id := "ses-" + uuid.NewString()
	_, err := p.db.ExecContext(ctx,
		`INSERT INTO sessions (id, agent_id, workflow_id, status) VALUES ($1,$2,$3,'created')`,
		id, agentID, workflowID)
	return id, err
}

func (p *pgStore) GetSession(ctx context.Context, id string) (SessionRow, error) {
	var s SessionRow
	err := p.db.QueryRowContext(ctx,
		`SELECT id, agent_id, workflow_id, status, turn_count FROM sessions WHERE id=$1`, id).
		Scan(&s.ID, &s.AgentID, &s.WorkflowID, &s.Status, &s.TurnCount)
	if err == sql.ErrNoRows {
		return SessionRow{}, fmt.Errorf("session %q not found", id)
	}
	return s, err
}

func (p *pgStore) SetSessionStatus(ctx context.Context, id, status string) error {
	_, err := p.db.ExecContext(ctx,
		`UPDATE sessions SET status=$2, last_active_at=now() WHERE id=$1`, id, status)
	return err
}

func (p *pgStore) AppendEvent(ctx context.Context, sessionID, typ string, payload []byte) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var next int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq),0)+1 FROM session_events WHERE session_id=$1`, sessionID).
		Scan(&next); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO session_events (session_id, seq, type, payload) VALUES ($1,$2,$3,$4)`,
		sessionID, next, typ, payload); err != nil {
		return err
	}
	return tx.Commit()
}

func (p *pgStore) EventsSince(ctx context.Context, sessionID string, afterSeq int64) ([]Event, error) {
	rows, err := p.db.QueryContext(ctx,
		`SELECT seq, type, payload FROM session_events WHERE session_id=$1 AND seq>$2 ORDER BY seq`,
		sessionID, afterSeq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.Seq, &e.Type, &e.Payload); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (p *pgStore) Close() error { return p.db.Close() }
```

Run `go get github.com/google/uuid` if not already present.

- [ ] **Step 8: Run full store tests + vet**

Run: `cd /Users/sausheong/projects/runtime && go test ./internal/store/ && go vet ./internal/store/`
Expected: PASS (Postgres impl compiles; it's exercised later by the integration test).

- [ ] **Step 9: Commit**

```bash
git add internal/store/
git commit -m "feat(store): control-plane store interface, in-memory + postgres impls"
```

---

## Task 3: agentruntime — the durable turn-step workflow (the crux)

This is the most important code in the milestone. A session's loop is a DBOS workflow. Each turn is `dbos.RunAsStep`. The step's **return value is the turn's `session.SessionEntry` slice** (JSON-serializable). On replay, DBOS returns the checkpointed entries instead of re-running the step — so the workflow rebuilds the in-memory harness session by re-applying entries, and never re-calls the LLM or re-runs committed tools.

**Key determinism rule (from DBOS docs):** the workflow body must be deterministic; all non-determinism (LLM calls, tool exec) lives inside steps. Live SSE emission is non-deterministic side-effect output, so the step runs the turn **headlessly** (emit=nil) and the *workflow* publishes events from the returned entries. This guarantees identical behavior on replay.

**Files:**
- Create: `runtime/agentruntime/turnstep.go`
- Test: `runtime/agentruntime/turnstep_test.go`

- [ ] **Step 1: Write the failing test (rebuild-from-entries logic)**

The fully durable path needs Postgres+DBOS and is covered by the integration test (Task 8). Here we unit-test the pure, deterministic piece: rebuilding a harness session by applying entry slices, and deciding loop termination. Create `runtime/agentruntime/turnstep_test.go`:

```go
package agentruntime

import (
	"testing"

	"github.com/sausheong/harness/session"
)

func TestApplyEntries_RebuildsSession(t *testing.T) {
	sess := session.NewSession("a", "k")
	turn1 := []session.SessionEntry{
		session.UserMessageEntry("hi"),
		session.AssistantMessageEntry("hello"),
	}
	applyEntries(sess, turn1)

	if got := len(sess.Entries()); got != 2 {
		t.Fatalf("after apply, entries = %d, want 2", got)
	}
}

func TestPublishableEvents_FromEntries(t *testing.T) {
	entries := []session.SessionEntry{
		session.UserMessageEntry("hi"),
		session.AssistantMessageEntry("the answer"),
	}
	evs := publishableEvents(entries)
	// Only the assistant message yields a client-facing text event in M1.
	if len(evs) != 1 || evs[0].Type != "text" {
		t.Fatalf("events = %+v, want one text event", evs)
	}
	if evs[0].Text != "the answer" {
		t.Fatalf("text = %q", evs[0].Text)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./agentruntime/ -run 'TestApplyEntries|TestPublishableEvents' -v`
Expected: compile error — `applyEntries`, `publishableEvents` undefined.

- [ ] **Step 3: Implement the workflow + helpers**

Create `runtime/agentruntime/turnstep.go`. Confirm the DBOS Go API names against the installed version (`go doc github.com/dbos-inc/dbos-transact-golang/dbos`) — the calls below follow the documented surface (`RegisterWorkflow`, `RunWorkflow`, `RunAsStep`, `WithWorkflowID`). Adjust call shapes to match the exact installed signatures; the *structure* (loop → step-per-turn → checkpointed entries) is what matters and must not change.

```go
package agentruntime

import (
	"context"
	"encoding/json"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	hrt "github.com/sausheong/harness/runtime"
	"github.com/sausheong/harness/llm"
	"github.com/sausheong/harness/session"
)

// WireEvent is the SSE-facing event the workflow publishes. Kept minimal for
// M1: text + lifecycle. Shared with the HTTP layer (sse.go).
type WireEvent struct {
	Type string `json:"type"` // text | tool_result | done | error
	Text string `json:"text,omitempty"`
	Err  string `json:"error,omitempty"`
}

// turnInput is the JSON-serializable input to a single turn step.
type turnInput struct {
	UserMsg string `json:"user_msg"` // non-empty only on the first turn
}

// turnOutput is the checkpointed return value of a single turn step. On
// replay DBOS returns this verbatim without re-executing the turn.
type turnOutput struct {
	Done    bool                    `json:"done"`
	Reason  string                  `json:"reason"`
	Entries []session.SessionEntry  `json:"entries"`
}

// applyEntries re-applies a turn's entries onto the in-memory session. Used
// both after a live step and (critically) when replaying checkpointed turns
// so the session state matches what the LLM saw originally.
func applyEntries(sess *session.Session, entries []session.SessionEntry) {
	for _, e := range entries {
		sess.Append(e)
	}
}

// publishableEvents derives the client-facing events from a turn's entries.
// Deterministic: same entries always yield the same events, so replay and
// live execution publish identically. M1 surfaces assistant text only.
func publishableEvents(entries []session.SessionEntry) []WireEvent {
	var out []WireEvent
	for _, e := range entries {
		if e.Type != session.EntryTypeMessage || e.Role != "assistant" {
			continue
		}
		var md session.MessageData
		if err := json.Unmarshal(e.Data, &md); err != nil {
			continue
		}
		if md.Text != "" {
			out = append(out, WireEvent{Type: "text", Text: md.Text})
		}
	}
	return out
}

// sessionWorkflow is the durable per-session loop. It is registered once and
// run with a stable workflow ID == the platform session id, so a process
// restart recovers exactly this workflow and replays completed turns.
//
// buildRuntime constructs a fresh harness Runtime bound to `sess`; it is a
// field on the manager (Task 5) closed over here. publish streams a WireEvent
// to any attached SSE client AND appends it to the store event log.
func (m *Manager) sessionWorkflow(ctx dbos.DBOSContext, in turnInput) (string, error) {
	// Reconstruct a fresh session + harness Runtime for this workflow.
	sess := session.NewSession(m.agentID, dbos.GetWorkflowID(ctx))
	rt := m.buildRuntime(sess)

	userMsg := in.UserMsg
	for {
		// Each turn is a durable step. On replay, RunAsStep returns the
		// checkpointed turnOutput WITHOUT re-running the closure.
		out, err := dbos.RunAsStep(ctx, func(stepCtx context.Context) (turnOutput, error) {
			// Headless: emit=nil so the step has no non-deterministic side
			// effects. Live events are published by the workflow below from
			// the returned entries.
			tr, terr := rt.RunTurn(stepCtx, userMsg, nil, nil)
			if terr != nil {
				return turnOutput{}, terr
			}
			return turnOutput{Done: tr.Done, Reason: tr.StopReason, Entries: tr.Entries}, nil
		})
		if err != nil {
			m.publish(dbos.GetWorkflowID(ctx), WireEvent{Type: "error", Err: err.Error()})
			return "error", err
		}

		// Rebuild in-memory session state from the (possibly checkpointed)
		// entries, then publish this turn's client events. Both are
		// deterministic functions of `out.Entries`, so replay is identical.
		applyEntries(sess, out.Entries)
		for _, ev := range publishableEvents(out.Entries) {
			m.publish(dbos.GetWorkflowID(ctx), ev)
		}

		if out.Done {
			m.publish(dbos.GetWorkflowID(ctx), WireEvent{Type: "done"})
			return out.Reason, nil
		}
		userMsg = "" // continuation turns carry no new user message
	}
}

var _ = llm.ImageContent{} // keep llm import if unused after edits; remove if vet complains
```

Two correctness notes the implementer MUST preserve:

1. **Stable workflow ID.** When starting the workflow (Task 5), pass the platform session id as the DBOS workflow id (`dbos.WithWorkflowID(sessionID)` or the installed equivalent). Recovery keys off this id. The `sessionWorkflow` reads it via `dbos.GetWorkflowID(ctx)` for publishing.
2. **The step must be pure-ish.** `RunTurn` is called with `emit=nil` inside the step. Do NOT publish SSE from inside the step closure — only from the workflow body, derived from returned entries. This is what makes live and replayed runs publish the same events.

- [ ] **Step 4: Run to verify the unit tests pass**

Run: `cd /Users/sausheong/projects/runtime && go test ./agentruntime/ -run 'TestApplyEntries|TestPublishableEvents' -v`
Expected: PASS. (`Manager.sessionWorkflow` won't be exercised until Task 5/8; it must compile.)

- [ ] **Step 5: Commit**

```bash
git add agentruntime/turnstep.go agentruntime/turnstep_test.go
git commit -m "feat(agentruntime): durable per-session DBOS workflow, turn=step

Each turn is a DBOS step whose checkpointed return value is the turn's
session entries. On replay, completed turns return checkpoints without
re-calling the LLM or re-running tools; the workflow rebuilds session
state and republishes events deterministically from the entries."
```

---

## Task 4: agentruntime Config + validation

**Files:**
- Create: `runtime/agentruntime/config.go`
- Test: `runtime/agentruntime/config_test.go`

- [ ] **Step 1: Write the failing test**

Create `runtime/agentruntime/config_test.go`:

```go
package agentruntime

import (
	"testing"

	hrt "github.com/sausheong/harness/runtime"
)

func TestConfig_Validate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"missing spec id", Config{ListenAddr: ":0", PostgresDSN: "x"}, true},
		{"missing dsn", Config{Spec: hrt.AgentSpec{ID: "a", Model: "m"}, ListenAddr: ":0"}, true},
		{"ok", Config{Spec: hrt.AgentSpec{ID: "a", Model: "m"}, ListenAddr: ":0", PostgresDSN: "x"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.cfg.Validate()
			if (err != nil) != c.wantErr {
				t.Fatalf("Validate() err=%v wantErr=%v", err, c.wantErr)
			}
		})
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./agentruntime/ -run TestConfig -v`
Expected: compile error — `Config` undefined.

- [ ] **Step 3: Implement Config**

Create `runtime/agentruntime/config.go`:

```go
package agentruntime

import (
	"errors"

	"github.com/sausheong/harness/llm"
	hrt "github.com/sausheong/harness/runtime"
	"github.com/sausheong/harness/tool"
)

// Config is the entire surface an agent author provides to Serve.
type Config struct {
	// Spec is the harness agent specification (id, model, system prompt...).
	Spec hrt.AgentSpec
	// Provider is the resolved LLM provider for Spec.Model.
	Provider llm.LLMProvider
	// Tools is the agent's tool registry.
	Tools *tool.Registry
	// ListenAddr is the HTTP bind address for the agent contract (e.g. ":8081").
	ListenAddr string
	// PostgresDSN is the DBOS system database connection string.
	PostgresDSN string
}

// Validate checks required fields.
func (c Config) Validate() error {
	if c.Spec.ID == "" || c.Spec.Model == "" {
		return errors.New("agentruntime: Spec.ID and Spec.Model are required")
	}
	if c.PostgresDSN == "" {
		return errors.New("agentruntime: PostgresDSN is required")
	}
	if c.ListenAddr == "" {
		return errors.New("agentruntime: ListenAddr is required")
	}
	return nil
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./agentruntime/ -run TestConfig -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add agentruntime/config.go agentruntime/config_test.go
git commit -m "feat(agentruntime): Config struct and validation"
```

---

## Task 5: agentruntime — SSE writer + Manager + Serve

Wires everything: DBOS launch + recovery on boot, a session Manager that starts/holds workflows and fans out events to attached SSE clients + the store log, and the HTTP server implementing the agent contract.

**Files:**
- Create: `runtime/agentruntime/sse.go`
- Create: `runtime/agentruntime/server.go`
- Create: `runtime/agentruntime/serve.go`
- Test: `runtime/agentruntime/sse_test.go`
- Test: `runtime/agentruntime/server_test.go`

- [ ] **Step 1: Write the failing SSE test**

Create `runtime/agentruntime/sse_test.go`:

```go
package agentruntime

import (
	"bytes"
	"testing"
)

func TestWriteSSE(t *testing.T) {
	var buf bytes.Buffer
	writeSSE(&buf, WireEvent{Type: "text", Text: "hi"})
	got := buf.String()
	want := "data: {\"type\":\"text\",\"text\":\"hi\"}\n\n"
	if got != want {
		t.Fatalf("writeSSE = %q, want %q", got, want)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./agentruntime/ -run TestWriteSSE -v`
Expected: compile error — `writeSSE` undefined.

- [ ] **Step 3: Implement the SSE writer**

Create `runtime/agentruntime/sse.go`:

```go
package agentruntime

import (
	"encoding/json"
	"io"
)

// writeSSE encodes one WireEvent as a Server-Sent Event frame.
func writeSSE(w io.Writer, ev WireEvent) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	_, err = w.Write([]byte("data: " + string(b) + "\n\n"))
	return err
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./agentruntime/ -run TestWriteSSE -v`
Expected: PASS.

- [ ] **Step 5: Implement the Manager**

Create `runtime/agentruntime/serve.go`. The Manager owns the DBOS context, builds harness Runtimes, starts workflows with a stable id, and fans out events. Confirm DBOS calls against `go doc`; structure is fixed.

```go
package agentruntime

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/sausheong/harness/compaction"
	hrt "github.com/sausheong/harness/runtime"
	"github.com/sausheong/harness/session"
	"github.com/sausheong/runtime/internal/store"
)

// Manager owns per-session durable workflows and event fan-out.
type Manager struct {
	agentID  string
	cfg      Config
	dbosCtx  dbos.DBOSContext
	st       store.Store

	mu          sync.Mutex
	subscribers map[string][]chan WireEvent // workflowID -> live SSE subscribers
}

// buildRuntime constructs a fresh harness Runtime bound to sess. No
// compaction in M1 (nil) — durability correctness first.
func (m *Manager) buildRuntime(sess *session.Session) *hrt.Runtime {
	rt, _ := hrt.BuildRuntime(
		hrt.RuntimeDeps{},
		hrt.RuntimeInputs{
			Provider:   m.cfg.Provider,
			Tools:      m.cfg.Tools,
			Session:    sess,
			Compaction: (*compaction.Manager)(nil),
		},
		m.cfg.Spec,
	)
	return rt
}

// publish fans an event out to live subscribers and appends it to the store
// log for later re-attach/replay.
func (m *Manager) publish(workflowID string, ev WireEvent) {
	payload, _ := json.Marshal(ev)
	_ = m.st.AppendEvent(context.Background(), workflowID, ev.Type, payload)

	m.mu.Lock()
	subs := append([]chan WireEvent(nil), m.subscribers[workflowID]...)
	m.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- ev:
		default: // drop on slow consumer; events are durable in the store
		}
	}
}

// subscribe registers a live SSE channel for a workflow; returns an
// unsubscribe func.
func (m *Manager) subscribe(workflowID string) (<-chan WireEvent, func()) {
	ch := make(chan WireEvent, 64)
	m.mu.Lock()
	m.subscribers[workflowID] = append(m.subscribers[workflowID], ch)
	m.mu.Unlock()
	return ch, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		cur := m.subscribers[workflowID]
		for i, c := range cur {
			if c == ch {
				m.subscribers[workflowID] = append(cur[:i], cur[i+1:]...)
				break
			}
		}
	}
}

// startSession creates a store session row and launches the durable workflow
// with the platform session id as the stable DBOS workflow id.
func (m *Manager) startSession(ctx context.Context, userMsg string) (string, error) {
	// Use a pre-generated id so the store row and the DBOS workflow share it.
	id := "ses-" + dbos.NewUUID() // or uuid.NewString(); confirm helper
	if _, err := m.st.CreateSession(ctx, m.agentID, id); err != nil {
		return "", err
	}
	// Fire-and-forget; the workflow runs to completion or until process death,
	// after which DBOS recovery resumes it on next boot.
	_, err := dbos.RunWorkflow(m.dbosCtx, m.sessionWorkflow, turnInput{UserMsg: userMsg},
		dbos.WithWorkflowID(id))
	if err != nil {
		return "", err
	}
	return id, nil
}
```

Notes for the implementer:
- `dbos.NewUUID` / id helper: use whatever id generator is available; if none, add `github.com/google/uuid`. The ONLY requirement is the store session id and the DBOS workflow id are identical.
- If `dbos.RunWorkflow` blocks until completion in the installed version, launch it in a goroutine so the HTTP handler returns immediately after the session row is created. Confirm via `go doc`.
- `compaction.Manager` nil cast: if `BuildRuntime` requires non-nil, pass `nil` directly; the harness Run path already nil-guards Compaction.

- [ ] **Step 6: Implement the HTTP server (the contract)**

Create `runtime/agentruntime/server.go`:

```go
package agentruntime

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// newMux builds the agent contract HTTP handler.
func (m *Manager) newMux() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("GET /meta", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"agent_id":         m.agentID,
			"contract_version": "v1",
		})
	})

	// POST /sessions/{id}/invoke  — body {message}; if id=="new" create one.
	// For M1 we expose POST /sessions (create+invoke) returning the id, then
	// the caller streams GET /sessions/{id}/stream.
	mux.HandleFunc("POST /sessions", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Message string `json:"message"` }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		id, err := m.startSession(r.Context(), body.Message)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"session_id": id})
	})

	// GET /sessions/{id}/stream — replay buffered events (after ?since=) then
	// stream live ones via SSE.
	mux.HandleFunc("GET /sessions/{id}/stream", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		var since int64
		if s := r.URL.Query().Get("since"); s != "" {
			since, _ = strconv.ParseInt(s, 10, 64)
		}

		// Subscribe BEFORE replay to avoid missing events emitted in between.
		live, unsub := m.subscribe(id)
		defer unsub()

		// Replay durable events first.
		buffered, err := m.st.EventsSince(r.Context(), id, since)
		if err == nil {
			for _, e := range buffered {
				var ev WireEvent
				if json.Unmarshal(e.Payload, &ev) == nil {
					_ = writeSSE(w, ev)
				}
			}
			flusher.Flush()
		}

		// Then stream live until the client disconnects or a terminal event.
		for {
			select {
			case <-r.Context().Done():
				return
			case ev := <-live:
				_ = writeSSE(w, ev)
				flusher.Flush()
				if ev.Type == "done" || ev.Type == "error" {
					return
				}
			}
		}
	})

	// GET /sessions/{id} — status snapshot.
	mux.HandleFunc("GET /sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		row, err := m.st.GetSession(r.Context(), r.PathValue("id"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": row.ID, "status": row.Status, "turn_count": row.TurnCount,
		})
	})

	return mux
}

// Serve is the public entrypoint: validate config, launch DBOS (which runs
// recovery for any pending workflows), then serve the contract until ctx is
// cancelled.
func Serve(ctx context.Context, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	st, err := store.NewPGStore(ctx, cfg.PostgresDSN)
	if err != nil {
		return err
	}
	defer st.Close()

	dctx, err := dbos.NewDBOSContext(ctx, dbos.Config{
		AppName:     cfg.Spec.ID,
		DatabaseURL: cfg.PostgresDSN,
	})
	if err != nil {
		return err
	}

	m := &Manager{
		agentID:     cfg.Spec.ID,
		cfg:         cfg,
		dbosCtx:     dctx,
		st:          st,
		subscribers: map[string][]chan WireEvent{},
	}

	// Register the workflow BEFORE Launch so recovery can find it.
	dbos.RegisterWorkflow(dctx, m.sessionWorkflow)

	// Launch runs recovery: any workflow that was mid-flight when the process
	// died resumes from its last completed turn step.
	if err := dbos.Launch(dctx); err != nil {
		return err
	}
	defer dbos.Shutdown(dctx, 10*time.Second)

	srv := &http.Server{Addr: cfg.ListenAddr, Handler: m.newMux()}
	go func() { <-ctx.Done(); _ = srv.Shutdown(context.Background()) }()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
```

Add the missing imports the implementer will need: `store` (`github.com/sausheong/runtime/internal/store`), `dbos`, `time`. Resolve the exact DBOS `Config`, `NewDBOSContext`, `RegisterWorkflow`, `Launch`, `Shutdown`, `RunWorkflow`, `WithWorkflowID`, `GetWorkflowID` signatures via `go doc github.com/dbos-inc/dbos-transact-golang/dbos` and adjust call sites. **Do not change the ordering: NewPGStore → NewDBOSContext → RegisterWorkflow → Launch → ListenAndServe.**

- [ ] **Step 7: Write a server smoke test (no DBOS/PG)**

Create `runtime/agentruntime/server_test.go` exercising the pure handlers via a Manager with an in-memory store and a pre-seeded event log (bypassing DBOS):

```go
package agentruntime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sausheong/runtime/internal/store"
)

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	return &Manager{
		agentID:     "a",
		st:          store.NewMemStore(),
		subscribers: map[string][]chan WireEvent{},
	}
}

func TestHealthzAndMeta(t *testing.T) {
	m := newTestManager(t)
	srv := httptest.NewServer(m.newMux())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("healthz: %v status=%v", err, resp.StatusCode)
	}
}

func TestStreamReplaysBufferedEvents(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	id, _ := m.st.CreateSession(ctx, "a", "wf")
	_ = m.st.AppendEvent(ctx, id, "text", []byte(`{"type":"text","text":"a"}`))
	_ = m.st.AppendEvent(ctx, id, "done", []byte(`{"type":"done"}`))

	srv := httptest.NewServer(m.newMux())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/sessions/" + id + "/stream")
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 256)
	n, _ := resp.Body.Read(buf)
	if n == 0 {
		t.Fatal("expected replayed SSE data")
	}
	// The "done" event terminates the stream, so the read returns and closes.
}
```

Note: the stream handler returns once it sees the buffered `done` only if `done` also arrives on the live channel. For a pure-replay terminal in M1, adjust the handler so replay of a terminal event (`done`/`error`) also closes the stream — make that change in `server.go` (after the replay loop, if the last buffered event was terminal, return). Update the handler accordingly and keep this test asserting the stream closes.

- [ ] **Step 8: Run agentruntime unit tests + vet**

Run: `go test ./agentruntime/ && go vet ./agentruntime/`
Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add agentruntime/
git commit -m "feat(agentruntime): SSE, session Manager, DBOS-launched Serve contract"
```

---

## Task 6: The agent subprocess binary (`cmd/agentd`) + a deterministic test agent

`agentd` is what the supervisor spawns. It reads config from env (injected by the control plane), builds a harness agent, and calls `agentruntime.Serve`. For real use it would wire a real provider; for hermetic integration tests we need a deterministic, scriptable provider that also lets us trigger a crash mid-turn. We put that fake in a small importable package so both `agentd` (via an env flag) and tests can use it.

**Files:**
- Create: `runtime/testagent/provider.go` — deterministic scripted `llm.LLMProvider` with an optional "crash after first tool call" hook driven by an env var.
- Create: `runtime/testagent/tools.go` — a `marker` tool that writes a row to Postgres (so we can prove it ran exactly once across a crash).
- Create: `runtime/cmd/agentd/main.go`

- [ ] **Step 1: Implement the scripted provider**

Create `runtime/testagent/provider.go`. Match the real `llm.LLMProvider` interface exactly — inspect `harness/llm/*.go` for the method set (`ChatStream`, `NormalizeToolSchema`, possibly `ChatNonStreaming`). The provider yields, for the first turn, a tool call to `marker`, then on the second turn a final text. A `CRASH_AFTER_MARKER=1` env var makes the `marker` tool call `os.Exit(1)` *after* its DB write but while the turn step is still executing — simulating a mid-turn crash with a committed side effect.

```go
package testagent

import (
	"context"

	"github.com/sausheong/harness/llm"
)

// Scripted is a deterministic provider: turn 1 calls the marker tool, turn 2
// returns final text. Used by integration tests; no network.
type Scripted struct{ turn int }

func New() *Scripted { return &Scripted{} }

func (s *Scripted) ChatStream(_ context.Context, _ llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	ch := make(chan llm.StreamEvent, 8)
	s.turn++
	go func() {
		defer close(ch)
		if s.turn == 1 {
			tc := &llm.ToolCall{ID: "m1", Name: "marker", Input: []byte(`{}`)}
			ch <- llm.StreamEvent{Type: llm.EventToolCallStart, ToolCall: tc}
			ch <- llm.StreamEvent{Type: llm.EventToolCallDone, ToolCall: tc}
			ch <- llm.StreamEvent{Type: llm.EventDone}
			return
		}
		ch <- llm.StreamEvent{Type: llm.EventTextDelta, Text: "final answer"}
		ch <- llm.StreamEvent{Type: llm.EventDone}
	}()
	return ch, nil
}

// NormalizeToolSchema is a no-op for the scripted provider.
func (s *Scripted) NormalizeToolSchema(d []llm.ToolDef) ([]llm.ToolDef, []llm.SchemaDiagnostic) {
	return d, nil
}
```

Adjust to the real interface: if `llm.LLMProvider` requires more methods, implement them as minimal stubs. Confirm `llm.StreamEvent`, `llm.EventToolCallStart/Done/TextDelta/Done`, and `llm.SchemaDiagnostic` names against `harness/llm`.

**Important determinism caveat:** the scripted provider above keys off an instance counter, which is fine because each workflow builds a fresh Runtime+provider. But on DBOS replay the turn step is NOT re-run, so the provider is not re-invoked for completed turns — the counter only advances for genuinely new turns. Confirm the manager builds a fresh `testagent.New()` per workflow (it does, via `buildRuntime` → `cfg.Provider`)... NOTE: `cfg.Provider` is a single shared instance. For the test agent we instead need a per-session provider. Resolve by having `agentd` pass a provider *factory*; for M1 simplest fix: make `Scripted` derive its turn from the session's existing entry count rather than an internal counter:

```go
// TurnFromSession returns which scripted turn to emit based on how many
// assistant/tool entries already exist — deterministic across replay.
```

Implement `ChatStream` to count tool_result entries via a closure the manager sets, OR (simpler and recommended) drive the script from `len(req.Messages)` in the `ChatRequest`: turn 1 when no prior tool result is present, turn 2 otherwise. Use `req.Messages` content to decide. This makes the provider a pure function of input — exactly what determinism wants.

- [ ] **Step 2: Implement the marker tool**

Create `runtime/testagent/tools.go`. The marker tool inserts a row into a `markers` table keyed by tool-call id, using `INSERT ... ON CONFLICT DO NOTHING` semantics is NOT used — we want to detect double execution, so we insert and count. The test asserts exactly one row exists after a crash+resume.

```go
package testagent

import (
	"context"
	"database/sql"
	"os"

	"github.com/sausheong/harness/tool"
)

type MarkerTool struct{ DB *sql.DB }

func (MarkerTool) Name() string        { return "marker" }
func (MarkerTool) Description() string { return "records that it ran" }
func (MarkerTool) InputSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (MarkerTool) IsConcurrencySafe() bool { return false }

func (t MarkerTool) Execute(ctx context.Context, _ []byte) (tool.ToolResult, error) {
	_, err := t.DB.ExecContext(ctx, `INSERT INTO markers (ran_at) VALUES (now())`)
	if err != nil {
		return tool.ToolResult{Error: err.Error()}, nil
	}
	if os.Getenv("CRASH_AFTER_MARKER") == "1" {
		os.Exit(1) // simulate crash AFTER the committed side effect
	}
	return tool.ToolResult{Output: "marked"}, nil
}
```

Match the real `tool.Tool` interface exactly (verify method names/signatures in `harness/tool/tool.go`). Add `CREATE TABLE IF NOT EXISTS markers (id BIGSERIAL PRIMARY KEY, ran_at TIMESTAMPTZ)` to the integration test setup or `schema.sql`.

**Crash-timing note:** `os.Exit` inside the tool runs *after* the marker INSERT commits but *before* the turn step returns, so DBOS has NOT checkpointed the turn. On resume the turn step re-runs — and the marker INSERT runs **again**. This deliberately demonstrates the spec's documented **at-least-once** semantics: the test asserts there are now **2** marker rows, proving we honestly reproduce (not hide) at-least-once, while also asserting the *session/loop* still completes correctly and the final answer is produced exactly once. (If we instead crash AFTER step checkpoint, the marker would run once — that's a second test variant.)

- [ ] **Step 3: Implement `cmd/agentd/main.go`**

```go
package main

import (
	"context"
	"database/sql"
	"log"
	"os"
	"os/signal"
	"syscall"

	_ "github.com/jackc/pgx/v5/stdlib"
	hrt "github.com/sausheong/harness/runtime"
	"github.com/sausheong/harness/tool"
	"github.com/sausheong/runtime/agentruntime"
	"github.com/sausheong/runtime/testagent"
)

func main() {
	dsn := os.Getenv("RUNTIME_PG_DSN")
	addr := os.Getenv("RUNTIME_LISTEN_ADDR")
	agentID := os.Getenv("RUNTIME_AGENT_ID")
	if dsn == "" || addr == "" || agentID == "" {
		log.Fatal("RUNTIME_PG_DSN, RUNTIME_LISTEN_ADDR, RUNTIME_AGENT_ID required")
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		log.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(),
		`CREATE TABLE IF NOT EXISTS markers (id BIGSERIAL PRIMARY KEY, ran_at TIMESTAMPTZ)`); err != nil {
		log.Fatal(err)
	}

	reg := tool.NewRegistry()
	reg.Register(testagent.MarkerTool{DB: db})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	err = agentruntime.Serve(ctx, agentruntime.Config{
		Spec:        hrt.AgentSpec{ID: agentID, Name: agentID, Model: "test/scripted", MaxTurns: 10},
		Provider:    testagent.New(),
		Tools:       reg,
		ListenAddr:  addr,
		PostgresDSN: dsn,
	})
	if err != nil {
		log.Fatal(err)
	}
}
```

- [ ] **Step 4: Build it**

Run: `go build ./cmd/agentd`
Expected: compiles cleanly.

- [ ] **Step 5: Commit**

```bash
git add testagent/ cmd/agentd/
git commit -m "feat(agentd): agent subprocess binary + deterministic test agent"
```

---

## Task 7: Control plane — supervisor, proxy, API, binary

The control plane spawns one `agentd` subprocess, waits for `/healthz`, restarts it on crash, and proxies invoke/stream to it.

**Files:**
- Create: `runtime/controlplane/supervisor.go`
- Create: `runtime/controlplane/proxy.go`
- Create: `runtime/controlplane/api.go`
- Create: `runtime/cmd/runtimed/main.go`
- Test: `runtime/controlplane/supervisor_test.go`

- [ ] **Step 1: Write the failing supervisor test**

The supervisor should restart a process that exits. Test against an injectable spawn function so it's hermetic. Create `runtime/controlplane/supervisor_test.go`:

```go
package controlplane

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestSupervisor_RestartsOnExit(t *testing.T) {
	var spawns int32
	spawn := func(ctx context.Context) (waitErr <-chan error) {
		atomic.AddInt32(&spawns, 1)
		ch := make(chan error, 1)
		go func() {
			time.Sleep(10 * time.Millisecond)
			ch <- nil // process "exited"
		}()
		return ch
	}

	sup := &Supervisor{Spawn: spawn, Backoff: time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	go sup.Run(ctx)
	time.Sleep(60 * time.Millisecond)
	cancel()

	if got := atomic.LoadInt32(&spawns); got < 2 {
		t.Fatalf("expected >=2 spawns (restart), got %d", got)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./controlplane/ -run TestSupervisor -v`
Expected: compile error — `Supervisor` undefined.

- [ ] **Step 3: Implement the supervisor**

Create `runtime/controlplane/supervisor.go`:

```go
package controlplane

import (
	"context"
	"time"
)

// Supervisor keeps a single subprocess alive, restarting with backoff.
type Supervisor struct {
	// Spawn starts the process and returns a channel that receives its exit
	// error (nil on clean exit) when it terminates.
	Spawn   func(ctx context.Context) <-chan error
	Backoff time.Duration
}

// Run supervises until ctx is cancelled.
func (s *Supervisor) Run(ctx context.Context) {
	backoff := s.Backoff
	if backoff == 0 {
		backoff = time.Second
	}
	for {
		if ctx.Err() != nil {
			return
		}
		wait := s.Spawn(ctx)
		select {
		case <-ctx.Done():
			return
		case <-wait:
			// process exited; loop and restart after backoff
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
		}
	}
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./controlplane/ -run TestSupervisor -v`
Expected: PASS.

- [ ] **Step 5: Implement the real process spawn + proxy + API**

Create `runtime/controlplane/proxy.go` (spawns `agentd` via `os/exec`, builds a reverse-proxy/passthrough to its `ListenAddr`):

```go
package controlplane

import (
	"context"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
)

// AgentProcess describes the single hardcoded M1 agent.
type AgentProcess struct {
	AgentID  string
	Addr     string // e.g. "127.0.0.1:8081"
	BinPath  string // path to agentd
	PGDSN    string
}

// SpawnFunc returns a Supervisor-compatible spawn closure.
func (a AgentProcess) SpawnFunc() func(ctx context.Context) <-chan error {
	return func(ctx context.Context) <-chan error {
		cmd := exec.CommandContext(ctx, a.BinPath)
		cmd.Env = append(os.Environ(),
			"RUNTIME_PG_DSN="+a.PGDSN,
			"RUNTIME_LISTEN_ADDR="+a.Addr,
			"RUNTIME_AGENT_ID="+a.AgentID,
		)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		ch := make(chan error, 1)
		if err := cmd.Start(); err != nil {
			ch <- err
			return ch
		}
		go func() { ch <- cmd.Wait() }()
		return ch
	}
}

// reverseProxy builds a passthrough to the agent subprocess.
func reverseProxy(addr string) *httputil.ReverseProxy {
	target, _ := url.Parse("http://" + addr)
	return httputil.NewSingleHostReverseProxy(target)
}
```

Create `runtime/controlplane/api.go`. For M1 (single hardcoded agent) the control API is a transparent passthrough to the subprocess — the contract is identical to the agent's. Prefixed multi-agent routing (`/agents/{id}/...`) lands in M2.

```go
package controlplane

import (
	"net/http"
)

// NewAPI returns the control-plane HTTP handler. M1: a transparent
// passthrough proxy to the single agent subprocess at addr. The agent
// contract (POST /sessions, GET /sessions/{id}/stream, /healthz, /meta...)
// is served verbatim through this proxy.
func NewAPI(agentAddr string) *http.ServeMux {
	mux := http.NewServeMux()
	proxy := reverseProxy(agentAddr)
	mux.Handle("/", proxy)
	return mux
}
```

When M2 adds multiple agents, this becomes a router that strips an `/agents/{id}` prefix and selects the matching subprocess proxy; the `reverseProxy` helper is already prefix-rewrite-ready via a custom director.

- [ ] **Step 6: Implement `cmd/runtimed/main.go`**

It: builds the agentd path (`go build` output or `RUNTIME_AGENTD_BIN`), starts the Supervisor in a goroutine, waits for `/healthz`, then serves the control API.

```go
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sausheong/runtime/controlplane"
)

func main() {
	dsn := envOr("RUNTIME_PG_DSN", "postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable")
	agentAddr := envOr("RUNTIME_AGENT_ADDR", "127.0.0.1:8081")
	ctlAddr := envOr("RUNTIME_CTL_ADDR", ":8080")
	agentBin := envOr("RUNTIME_AGENTD_BIN", "./agentd")

	ap := controlplane.AgentProcess{AgentID: "default", Addr: agentAddr, BinPath: agentBin, PGDSN: dsn}
	sup := &controlplane.Supervisor{Spawn: ap.SpawnFunc(), Backoff: time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go sup.Run(ctx)

	mux := controlplane.NewAPI(agentAddr) // proxy to the subprocess
	srv := &http.Server{Addr: ctlAddr, Handler: mux}
	go func() { <-ctx.Done(); _ = srv.Shutdown(context.Background()) }()
	log.Printf("control plane on %s → agent %s", ctlAddr, agentAddr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
```

Add `controlplane.NewAPI(agentAddr) *http.ServeMux` in `api.go` that proxies all paths to the subprocess (single-agent M1).

- [ ] **Step 7: Build everything + vet + unit tests**

Run: `go build ./... && go vet ./... && go test ./controlplane/ ./agentruntime/ ./internal/store/`
Expected: all PASS.

- [ ] **Step 8: Commit**

```bash
git add controlplane/ cmd/runtimed/
git commit -m "feat(controlplane): supervisor, agentd spawn, proxy API, runtimed binary"
```

---

## Task 8: Flagship integration test — kill-mid-turn resume (the acceptance criterion)

This is the test the whole milestone exists to pass. It runs against real Postgres + real DBOS, drives a real `agentd` subprocess, kills it mid-turn, and asserts the session resumes and completes. Gated behind `//go:build integration` so `go test ./...` stays hermetic.

**Files:**
- Create: `runtime/test/resume_test.go`

- [ ] **Step 1: Write the integration test**

Create `runtime/test/resume_test.go`:

```go
//go:build integration

package test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

const dsn = "postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable"

// TestResumeAfterKill is the milestone's acceptance test.
//
// Flow: build agentd → start it with CRASH_AFTER_MARKER=1 → POST /sessions to
// start a turn that calls the marker tool then crashes → supervisor (here: the
// test itself) restarts agentd WITHOUT the crash flag → DBOS recovers the
// workflow → assert the session reaches a "done" event and produces the final
// answer exactly once.
func TestResumeAfterKill(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExec(t, db, `DROP TABLE IF EXISTS markers`)
	mustExec(t, db, `CREATE TABLE markers (id BIGSERIAL PRIMARY KEY, ran_at TIMESTAMPTZ)`)
	// Clean DBOS + control-plane state for a deterministic run.
	mustExec(t, db, `DROP TABLE IF EXISTS session_events, sessions, agents CASCADE`)

	bin := buildAgentd(t)
	addr := "127.0.0.1:8091"

	// Phase 1: run agentd that will crash after the marker side effect.
	p1 := startAgentd(t, bin, addr, map[string]string{"CRASH_AFTER_MARKER": "1"})
	waitHealthy(t, addr)

	sessionID := postSession(t, addr, "go") // starts the durable workflow
	// agentd should crash itself (os.Exit(1)) shortly after the marker write.
	waitExit(t, p1, 5*time.Second)

	// Sanity: the marker ran at least once before the crash.
	if n := count(t, db, `SELECT count(*) FROM markers`); n < 1 {
		t.Fatalf("marker did not run before crash (got %d)", n)
	}

	// Phase 2: restart agentd WITHOUT the crash flag. DBOS recovery resumes
	// the in-flight workflow from its last completed step.
	p2 := startAgentd(t, bin, addr, nil)
	defer func() { _ = p2.Process.Kill() }()
	waitHealthy(t, addr)

	// Re-attach to the session stream and assert it completes with the final
	// answer. since=0 replays the durable log + live tail.
	final := streamUntilDone(t, addr, sessionID, 20*time.Second)
	if !strings.Contains(final, "final answer") {
		t.Fatalf("session did not produce final answer; got events: %s", final)
	}

	// At-least-once semantics (spec §7): the marker may have run twice because
	// the crash happened after its side effect but before the turn checkpoint.
	// Assert the loop still completed correctly and produced the final answer
	// exactly once (one assistant 'done').
	if got := strings.Count(final, `"type":"done"`); got != 1 {
		t.Fatalf(`expected exactly one "done" event, got %d in: %s`, got, final)
	}
	t.Logf("marker rows after resume: %d (>=1 expected; 2 demonstrates documented at-least-once)",
		count(t, db, `SELECT count(*) FROM markers`))
}
```

Then add the helpers in the same file:

```go
func mustExec(t *testing.T, db *sql.DB, q string) {
	t.Helper()
	if _, err := db.Exec(q); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

func count(t *testing.T, db *sql.DB, q string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func buildAgentd(t *testing.T) string {
	t.Helper()
	out := t.TempDir() + "/agentd"
	cmd := exec.Command("go", "build", "-o", out, "../cmd/agentd")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build agentd: %v", err)
	}
	return out
}

func startAgentd(t *testing.T, bin, addr string, extraEnv map[string]string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		"RUNTIME_PG_DSN="+dsn,
		"RUNTIME_LISTEN_ADDR="+addr,
		"RUNTIME_AGENT_ID=default",
	)
	for k, v := range extraEnv {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start agentd: %v", err)
	}
	return cmd
}

func waitHealthy(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err == nil && resp.StatusCode == 200 {
			resp.Body.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("agentd at %s never became healthy", addr)
}

func waitExit(t *testing.T, cmd *exec.Cmd, d time.Duration) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("agentd did not exit within %s (expected crash)", d)
	}
}

func postSession(t *testing.T, addr, msg string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"message": msg})
	resp, err := http.Post("http://"+addr+"/sessions", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("post session: %v", err)
	}
	defer resp.Body.Close()
	var out struct{ SessionID string `json:"session_id"` }
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.SessionID == "" {
		t.Fatal("no session id returned")
	}
	return out.SessionID
}

func streamUntilDone(t *testing.T, addr, id string, d time.Duration) string {
	t.Helper()
	req, _ := http.NewRequest("GET", "http://"+addr+"/sessions/"+id+"/stream?since=0", nil)
	client := &http.Client{Timeout: d}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 4096)
	var sb strings.Builder
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			sb.Write(buf[:n])
			if strings.Contains(sb.String(), `"type":"done"`) {
				return sb.String()
			}
		}
		if err != nil {
			break
		}
	}
	return sb.String()
}
```

The implementer must reconcile two things discovered during earlier tasks and adjust this test if needed:
- If `RunWorkflow` does not auto-run on `Launch`-time recovery without an explicit re-attach call, add the documented DBOS recovery/retrieve call (`dbos.RetrieveWorkflow` / handle re-acquisition) inside `Serve` after `Launch`. Check `go doc`. The intent — completed turns are not re-run, pending workflow resumes — is non-negotiable; the exact call may differ.
- If the scripted provider was changed (Task 6) to derive its turn from `req.Messages`, the resume must naturally continue to "turn 2" because the recovered session already contains the marker tool_result. Confirm this end-to-end; it is the whole point.

- [ ] **Step 2: Run the integration test**

Ensure Postgres is up (`docker compose -f deploy/docker-compose.yml up -d`), then:

Run: `go test -tags integration ./test/ -run TestResumeAfterKill -v -count=1`
Expected: PASS — logs show the marker ran, agentd crashed, restarted, the workflow resumed, and the stream produced "final answer" with exactly one "done".

- [ ] **Step 3: Commit**

```bash
git add test/resume_test.go
git commit -m "test: flagship kill-mid-turn durable resume integration test"
```

---

## Task 9: CLI (`cmd/runtimectl`)

Minimal CLI proving the operator loop: `invoke` (start a session + stream) and `logs` (replay a session's events). `deploy` in M1 just prints the static config (real deploy lands in Milestone 2).

**Files:**
- Create: `runtime/cmd/runtimectl/main.go`

- [ ] **Step 1: Implement the CLI**

```go
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: runtimectl <invoke|logs|deploy> [args]")
		os.Exit(2)
	}
	base := envOr("RUNTIME_CTL_URL", "http://localhost:8080")
	switch os.Args[1] {
	case "invoke":
		invoke(base, os.Args[2:])
	case "logs":
		logs(base, os.Args[2:])
	case "deploy":
		fmt.Println("deploy: M1 uses a single statically-configured agent 'default' (see runtimed env)")
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		os.Exit(2)
	}
}

func invoke(base string, args []string) {
	msg := "hello"
	if len(args) > 0 {
		msg = args[0]
	}
	body, _ := json.Marshal(map[string]string{"message": msg})
	resp, err := http.Post(base+"/sessions", "application/json", bytes.NewReader(body))
	check(err)
	var out struct{ SessionID string `json:"session_id"` }
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	fmt.Println("session:", out.SessionID)
	stream(base, out.SessionID)
}

func logs(base string, args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: runtimectl logs <session-id>")
		os.Exit(2)
	}
	stream(base, args[0])
}

func stream(base, id string) {
	resp, err := http.Get(base + "/sessions/" + id + "/stream?since=0")
	check(err)
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if line != "" {
			fmt.Println(line)
		}
	}
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

var _ = io.EOF
```

- [ ] **Step 2: Build + manual smoke (optional, needs running stack)**

Run: `go build ./cmd/runtimectl`
Expected: compiles. With the stack up (`runtimed` + Postgres): `./runtimectl invoke "hi"` prints a session id then streams `data: {...}` lines ending in `done`.

- [ ] **Step 3: Commit**

```bash
git add cmd/runtimectl/
git commit -m "feat(cli): runtimectl invoke/logs against the control plane"
```

---

## Task 10: Milestone wrap-up — README + full verification

**Files:**
- Create: `runtime/README.md`

- [ ] **Step 1: Write a short README** documenting: architecture (one paragraph + link to the spec), how to run (`docker compose up`, `go build ./cmd/...`, start `runtimed`, `runtimectl invoke`), how to run tests (hermetic vs `-tags integration`), and the M1 scope/limitations (single static agent, at-least-once tools, no auth/console yet — those are M2/M3).

- [ ] **Step 2: Full hermetic verification**

Run:
```bash
cd /Users/sausheong/projects/runtime && go build ./... && go vet ./... && go test ./...
cd /Users/sausheong/projects/harness && go build ./... && go vet ./... && go test ./...
```
Expected: all PASS in both repos.

- [ ] **Step 3: Full integration verification**

Run:
```bash
cd /Users/sausheong/projects/runtime
docker compose -f deploy/docker-compose.yml up -d
go test -tags integration ./test/ -v -count=1
```
Expected: the flagship resume test PASSES.

- [ ] **Step 4: Commit**

```bash
git add README.md
git commit -m "docs: M1 README and runbook"
```

---

## Definition of Done (Milestone 1)

- [ ] `harness` has a tested `RunTurn` API; full harness suite green.
- [ ] `agentruntime.Serve` runs a harness agent as an HTTP/SSE contract server with a DBOS-backed per-session durable loop.
- [ ] The control plane spawns and restarts the agent subprocess and proxies the contract.
- [ ] `runtimectl invoke` drives a session end-to-end and streams events.
- [ ] **The flagship integration test passes: an agent killed mid-turn resumes, the loop completes, and at-least-once tool semantics are demonstrated honestly (not hidden).**
- [ ] All hermetic tests + vet green in both repos.
