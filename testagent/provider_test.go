package testagent

import (
	"context"
	"testing"

	"github.com/sausheong/harness/llm"
)

// drain collects all events from a ChatStream channel.
func drain(t *testing.T, ch <-chan llm.ChatEvent) []llm.ChatEvent {
	t.Helper()
	var out []llm.ChatEvent
	for ev := range ch {
		out = append(out, ev)
	}
	return out
}

func TestScripted_FirstTurnEmitsMarkerToolCall(t *testing.T) {
	s := New()
	// First turn: only a user seed message, no tool result in history.
	req := llm.ChatRequest{Messages: []llm.Message{{Role: "user", Content: "go"}}}

	ch, err := s.ChatStream(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	events := drain(t, ch)

	var sawToolStart, sawToolDone, sawDone bool
	for _, ev := range events {
		switch ev.Type {
		case llm.EventToolCallStart:
			sawToolStart = true
			if ev.ToolCall == nil || ev.ToolCall.Name != "marker" {
				t.Fatalf("expected marker tool call, got %+v", ev.ToolCall)
			}
		case llm.EventToolCallDone:
			sawToolDone = true
		case llm.EventTextDelta:
			t.Fatalf("unexpected text on first turn: %q", ev.Text)
		case llm.EventDone:
			sawDone = true
		}
	}
	if !sawToolStart || !sawToolDone || !sawDone {
		t.Fatalf("missing events: start=%v done=%v eventDone=%v", sawToolStart, sawToolDone, sawDone)
	}
}

func TestScripted_ContinuationEmitsFinalText(t *testing.T) {
	s := New()
	// Continuation turn: history contains a tool result (ToolCallID set).
	req := llm.ChatRequest{Messages: []llm.Message{
		{Role: "user", Content: "go"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "m1", Name: "marker", Input: []byte("{}")}}},
		{Role: "user", Content: "marked", ToolCallID: "m1"},
	}}

	ch, err := s.ChatStream(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	events := drain(t, ch)

	var text string
	var sawDone bool
	for _, ev := range events {
		switch ev.Type {
		case llm.EventTextDelta:
			text += ev.Text
		case llm.EventToolCallStart, llm.EventToolCallDone:
			t.Fatalf("unexpected tool call on continuation turn: %+v", ev)
		case llm.EventDone:
			sawDone = true
		}
	}
	if text != "final answer" {
		t.Fatalf("expected %q, got %q", "final answer", text)
	}
	if !sawDone {
		t.Fatal("missing EventDone")
	}
}
