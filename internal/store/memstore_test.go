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

func TestMemInitialFailureCategoryIsMonotonicAndReplaySafe(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	id, _ := s.CreateSession(ctx, "agent-x", 0)
	changed, err := s.SetInitialFailureCategory(ctx, id, "none")
	if err != nil || !changed {
		t.Fatalf("initial classification changed=%v err=%v", changed, err)
	}
	if err := s.SetFailureCategory(ctx, id, "quality_fail"); err != nil {
		t.Fatal(err)
	}
	changed, err = s.SetInitialFailureCategory(ctx, id, "none")
	if err != nil || changed {
		t.Fatalf("replay classification changed=%v err=%v", changed, err)
	}
	row, _ := s.GetSession(ctx, id)
	if row.FailureCategory != "quality_fail" {
		t.Fatalf("replay downgraded quality refinement to %q", row.FailureCategory)
	}
	refined, err := s.RefineFailureCategory(ctx, id, "quality_fail", "tool_error")
	if err != nil || !refined {
		t.Fatalf("refine category changed=%v err=%v", refined, err)
	}
	refined, err = s.RefineFailureCategory(ctx, id, "quality_fail", "none")
	if err != nil || refined {
		t.Fatalf("replayed/wrong-source refinement changed=%v err=%v", refined, err)
	}
}

func TestMemOnlineResultReportsFirstDurableWrite(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	id, _ := s.CreateSession(ctx, "agent-x", 0)
	inserted, authoritative, err := s.PutOnlineResultIfNew(
		ctx, id, "quality", "default", "actor", "contains", true, "")
	if err != nil || !inserted || !authoritative {
		t.Fatalf("first write inserted=%v authoritative=%v err=%v", inserted, authoritative, err)
	}
	inserted, authoritative, err = s.PutOnlineResultIfNew(
		ctx, id, "quality", "default", "actor", "contains", false, "changed")
	if err != nil || inserted || !authoritative {
		t.Fatalf("replay write inserted=%v authoritative=%v err=%v", inserted, authoritative, err)
	}
	results, err := s.ListOnlineResults(ctx, id)
	if err != nil || len(results) != 1 || !results[0].Passed || results[0].Detail != "" {
		t.Fatalf("immutable result=%+v err=%v", results, err)
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
	idOtherTenant, _ := s.CreateSessionForTenant(ctx, "other", "agent-a", 0)
	_ = s.SetFailureCategory(ctx, idOtherTenant, "tool_error")

	got, err := s.FailureBreakdownByAgent(ctx, "default", "agent-a", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"tool_error": 2, "none": 1}
	if len(got) != len(want) || got["tool_error"] != 2 || got["none"] != 1 {
		t.Fatalf("breakdown=%v, want %v (unclassified '' must be omitted)", got, want)
	}
}
