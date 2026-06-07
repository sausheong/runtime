package store

import (
	"context"
	"testing"
)

func TestStore_SessionLifecycle(t *testing.T) {
	s := NewMemStore()
	ctx := context.Background()

	id, err := s.CreateSession(ctx, "agent1")
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
	if got.WorkflowID != id || got.AgentID != "agent1" {
		t.Fatalf("session mismatch: %+v", got)
	}
	if got.Status != "created" {
		t.Fatalf("status = %q, want created", got.Status)
	}
}

func TestStore_CreateSessionPopulatesWorkflowID(t *testing.T) {
	s := NewMemStore()
	ctx := context.Background()
	id, err := s.CreateSession(ctx, "agentA")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	got, _ := s.GetSession(ctx, id)
	if got.WorkflowID != id {
		t.Fatalf("workflow_id = %q, want = session id %q", got.WorkflowID, id)
	}
	if got.AgentID != "agentA" || got.Status != "created" {
		t.Fatalf("unexpected row: %+v", got)
	}
}

func TestStore_SetSessionStatus(t *testing.T) {
	s := NewMemStore()
	ctx := context.Background()
	id, _ := s.CreateSession(ctx, "a")
	if err := s.SetSessionStatus(ctx, id, "running"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSessionStatus(ctx, id, "completed"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetSession(ctx, id)
	if got.Status != "completed" {
		t.Fatalf("got status=%q, want completed", got.Status)
	}
}

func TestStore_SetTurnCount(t *testing.T) {
	s := NewMemStore()
	ctx := context.Background()
	id, _ := s.CreateSession(ctx, "a")
	if err := s.SetTurnCount(ctx, id, 5); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetSession(ctx, id)
	if got.TurnCount != 5 {
		t.Fatalf("turn_count = %d, want 5", got.TurnCount)
	}
}

func TestStore_ListSessionsByAgent(t *testing.T) {
	s := NewMemStore()
	ctx := context.Background()
	a1, _ := s.CreateSession(ctx, "agentA")
	_, _ = s.CreateSession(ctx, "agentB")
	a2, _ := s.CreateSession(ctx, "agentA")
	rows, err := s.ListSessions(ctx, "agentA")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("ListSessions(agentA) = %d rows, want 2", len(rows))
	}
	ids := map[string]bool{rows[0].ID: true, rows[1].ID: true}
	if !ids[a1] || !ids[a2] {
		t.Fatalf("missing expected ids; got %+v", rows)
	}
}

func TestStore_EventLogAppendAndReplay(t *testing.T) {
	s := NewMemStore()
	ctx := context.Background()
	id, _ := s.CreateSession(ctx, "agent1")

	for i, typ := range []string{"text_delta", "text_delta", "done"} {
		if _, err := s.AppendEvent(ctx, id, typ, []byte(`{"i":`+itoa(i)+`}`)); err != nil {
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

	tail, _ := s.EventsSince(ctx, id, 2)
	if len(tail) != 1 || tail[0].Type != "done" {
		t.Fatalf("tail replay wrong: %+v", tail)
	}
}

func itoa(i int) string { return string(rune('0' + i)) }
