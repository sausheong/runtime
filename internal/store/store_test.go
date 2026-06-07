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
