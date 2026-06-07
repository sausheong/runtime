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

	"github.com/sausheong/harness/llm"
)

// Scripted is a deterministic llm.LLMProvider. It emits one of two scripted
// turns, chosen solely by inspecting the assembled request history:
//
//   - First turn (history contains NO tool result): emit a tool call to the
//     "marker" tool, then EventDone.
//   - Continuation turn (history ALREADY contains a tool result): emit a final
//     text answer "final answer", then EventDone.
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
