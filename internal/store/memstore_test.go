package store

import (
	"context"
	"testing"
	"time"
)

func TestMemStoreSetSessionUsage(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	id, err := s.CreateSession(ctx, "a1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetSessionUsage(ctx, id, 1500, 0.42); err != nil {
		t.Fatal(err)
	}
	row, err := s.GetSession(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.TokensTotal != 1500 || row.CostUSD != 0.42 {
		t.Fatalf("got tokens=%d cost=%v, want 1500/0.42", row.TokensTotal, row.CostUSD)
	}
	// Idempotent absolute-set: calling again with the same value is stable.
	if err := s.SetSessionUsage(ctx, id, 1500, 0.42); err != nil {
		t.Fatal(err)
	}
	row, _ = s.GetSession(ctx, id)
	if row.TokensTotal != 1500 || row.CostUSD != 0.42 {
		t.Fatalf("absolute-set not idempotent: tokens=%d cost=%v", row.TokensTotal, row.CostUSD)
	}
}

func TestMemFailureCategory(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	id, _ := s.CreateSession(ctx, "agent-x", 0)

	// Default is empty (unclassified).
	row, _ := s.GetSession(ctx, id)
	if row.FailureCategory != "" {
		t.Fatalf("new session category=%q, want empty", row.FailureCategory)
	}

	// Set is idempotent (absolute set, replay-safe).
	if err := s.SetFailureCategory(ctx, id, "tool_error"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetFailureCategory(ctx, id, "tool_error"); err != nil {
		t.Fatal(err)
	}
	row, _ = s.GetSession(ctx, id)
	if row.FailureCategory != "tool_error" {
		t.Fatalf("category=%q, want tool_error", row.FailureCategory)
	}

	// Round-trips through ListSessions too.
	list, _ := s.ListSessions(ctx, "agent-x")
	if len(list) != 1 || list[0].FailureCategory != "tool_error" {
		t.Fatalf("ListSessions category not round-tripped: %+v", list)
	}
}

func TestMemFailureBreakdownByAgent(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	// agent-a: two tool_error, one none, one unclassified.
	for _, cat := range []string{"tool_error", "tool_error", "none", ""} {
		id, _ := s.CreateSession(ctx, "agent-a", 0)
		if cat != "" {
			_ = s.SetFailureCategory(ctx, id, cat)
		}
	}
	// agent-b: one none (must not leak into agent-a's breakdown).
	idB, _ := s.CreateSession(ctx, "agent-b", 0)
	_ = s.SetFailureCategory(ctx, idB, "none")

	got, err := s.FailureBreakdownByAgent(ctx, "agent-a", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"tool_error": 2, "none": 1}
	if len(got) != len(want) || got["tool_error"] != 2 || got["none"] != 1 {
		t.Fatalf("breakdown=%v, want %v (unclassified '' must be omitted)", got, want)
	}
}
