// Package testagent provides a deterministic, network-free LLM provider and a
// marker tool used by the flagship resume integration test. The provider is a
// pure function of the request — it never uses an internal call counter —
// because on DBOS replay/resume the durable workflow rebuilds session state and
// re-drives turns. A counter would desync on replay; deriving the next event
// purely from req.Messages stays correct no matter how many times a turn is
// re-driven.
package testagent

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/sausheong/harness/llm"
)

// Scripted is a deterministic llm.LLMProvider. Its behavior is selected by
// the TESTAGENT_MODE env var (read at each call), and the decision remains a
// pure function of (env, req.Messages) — never an internal counter — so it
// stays correct under DBOS replay/resume:
//
//	""      — the classic 2-turn script (marker tool, then final answer):
//	  - First turn (history contains NO tool result): emit a tool call to the
//	    "marker" tool, then EventDone.
//	  - Continuation turn (history ALREADY contains a tool result): emit a
//	    final text answer "final answer", then EventDone.
//	"loop"  — a marker tool call EVERY turn; the session never finishes on
//	          its own. Each turn reports Usage{100 in, 50 out} so token
//	          budgets are exercised deterministically.
//	"sleep" — like the classic script but the first turn calls the "sleep"
//	          tool (which blocks for TESTAGENT_SLEEP_MS) — used to trip
//	          turn_timeout.
type Scripted struct{}

// New returns a ready-to-use Scripted provider.
func New() *Scripted { return &Scripted{} }

// historyHasToolResult reports whether the assembled request history already
// contains a tool result. In harness, assembleMessages (runtime/context.go)
// represents a tool result as a message with a non-empty ToolCallID (Role
// "user", carrying the tool output). Detecting it this way makes the decision a
// pure function of the request and therefore stable under DBOS replay/resume.
func historyHasToolResult(msgs []llm.Message) bool {
	for _, m := range msgs {
		if m.ToolCallID != "" {
			return true
		}
	}
	return false
}

// ChatStream returns a buffered channel of scripted events. A goroutine emits
// the events for the chosen turn and then closes the channel. Context
// cancellation is honored so the goroutine never blocks forever.
func (s *Scripted) ChatStream(ctx context.Context, req llm.ChatRequest) (<-chan llm.ChatEvent, error) {
	ch := make(chan llm.ChatEvent, 4)

	go func() {
		defer close(ch)

		send := func(ev llm.ChatEvent) bool {
			select {
			case ch <- ev:
				return true
			case <-ctx.Done():
				return false
			}
		}

		mode := os.Getenv("TESTAGENT_MODE")
		if mode == "loop" {
			// Loop mode: a marker tool call EVERY turn — the session never
			// finishes on its own, so lifecycle limits (max_turns, max_tokens)
			// are what terminate it. The tool-call id is derived from the count
			// of tool results already in history (a pure function of req), so
			// replay re-derives the identical id.
			//
			// TESTAGENT_LOOP_TURN_MS optionally slows each turn down (pure
			// timing, never a decision input) so a test can deterministically
			// land a kill mid-session instead of racing millisecond turns.
			if v := os.Getenv("TESTAGENT_LOOP_TURN_MS"); v != "" {
				if ms, err := strconv.Atoi(v); err == nil && ms > 0 {
					select {
					case <-time.After(time.Duration(ms) * time.Millisecond):
					case <-ctx.Done():
						return
					}
				}
			}
			n := 0
			for _, m := range req.Messages {
				if m.ToolCallID != "" {
					n++
				}
			}
			tc := &llm.ToolCall{ID: fmt.Sprintf("m%d", n+1), Name: "marker", Input: []byte("{}")}
			if !send(llm.ChatEvent{Type: llm.EventToolCallStart, ToolCall: tc}) {
				return
			}
			if !send(llm.ChatEvent{Type: llm.EventToolCallDone, ToolCall: tc}) {
				return
			}
			send(llm.ChatEvent{Type: llm.EventDone, Usage: &llm.Usage{InputTokens: 100, OutputTokens: 50}})
			return
		}
		if mode == "sleep" && !historyHasToolResult(req.Messages) {
			// Sleep mode, first turn: call the "sleep" tool, which blocks for
			// TESTAGENT_SLEEP_MS — used to trip turn_timeout. Continuation
			// turns (if the sleep ever completes) fall through to the classic
			// final-answer branch below.
			tc := &llm.ToolCall{ID: "s1", Name: "sleep", Input: []byte("{}")}
			if !send(llm.ChatEvent{Type: llm.EventToolCallStart, ToolCall: tc}) {
				return
			}
			if !send(llm.ChatEvent{Type: llm.EventToolCallDone, ToolCall: tc}) {
				return
			}
			send(llm.ChatEvent{Type: llm.EventDone, Usage: &llm.Usage{InputTokens: 1, OutputTokens: 1}})
			return
		}

		if historyHasToolResult(req.Messages) {
			// Continuation turn: the marker has run; produce the final answer.
			if !send(llm.ChatEvent{Type: llm.EventTextDelta, Text: "final answer"}) {
				return
			}
			send(llm.ChatEvent{Type: llm.EventDone, Usage: &llm.Usage{InputTokens: 1, OutputTokens: 1}})
			return
		}

		// First turn: request the marker tool.
		tc := &llm.ToolCall{ID: "m1", Name: "marker", Input: []byte("{}")}
		if !send(llm.ChatEvent{Type: llm.EventToolCallStart, ToolCall: tc}) {
			return
		}
		if !send(llm.ChatEvent{Type: llm.EventToolCallDone, ToolCall: tc}) {
			return
		}
		send(llm.ChatEvent{Type: llm.EventDone, Usage: &llm.Usage{InputTokens: 1, OutputTokens: 1}})
	}()

	return ch, nil
}

// Models reports the single synthetic model this provider serves.
func (s *Scripted) Models() []llm.ModelInfo {
	return []llm.ModelInfo{{ID: "scripted", Name: "scripted", Provider: "test"}}
}

// NormalizeToolSchema is a no-op: the scripted provider accepts any schema.
func (s *Scripted) NormalizeToolSchema(tools []llm.ToolDef) ([]llm.ToolDef, []llm.Diagnostic) {
	return tools, nil
}
