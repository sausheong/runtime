package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestStore_TenantOwnershipAndExternalBinding(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	alpha, err := s.CreateSessionForIdentity(
		ctx, "alpha", "shared", "generation-1", 2)
	if err != nil {
		t.Fatal(err)
	}
	row, err := s.GetSession(ctx, alpha)
	if err != nil || row.TenantID != "alpha" || row.AgentID != "shared" ||
		row.AgentGeneration != "generation-1" || row.Replica != 2 {
		t.Fatalf("tenant session round-trip: row=%+v err=%v", row, err)
	}
	if rows, _ := s.ListSessionsForTenant(ctx, "beta", "shared"); len(rows) != 0 {
		t.Fatalf("cross-tenant list returned %+v", rows)
	}
	if err := s.BindSession(ctx, "external-1", "alpha", "shared", "generation-1", 1); err != nil {
		t.Fatal(err)
	}
	bound, err := s.GetSession(ctx, "external-1")
	if err != nil || bound.TenantID != "alpha" ||
		bound.AgentGeneration != "generation-1" ||
		bound.Replica != 1 || bound.Status != "external" {
		t.Fatalf("external binding: row=%+v err=%v", bound, err)
	}
	if err := s.BindSession(ctx, "external-1", "alpha", "shared", "generation-1", 1); err != nil {
		t.Fatalf("idempotent bind: %v", err)
	}
	if err := s.BindSession(ctx, "external-1", "beta", "shared", "generation-1", 1); err == nil {
		t.Fatal("conflicting binding accepted")
	}
}

func TestStore_MissingSessionUsesSentinel(t *testing.T) {
	_, err := NewMemStore().GetSession(context.Background(), "missing")
	if !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("GetSession error = %v, want ErrSessionNotFound", err)
	}
}

func TestStore_ReapSessionsRetainsActiveAndCascades(t *testing.T) {
	ctx := context.Background()
	st := NewMemStore()
	terminal, _ := st.CreateSessionForTenant(ctx, "t", "a", 0)
	active, _ := st.CreateSessionForTenant(ctx, "t", "a", 0)
	if err := st.SetSessionStatus(ctx, terminal, "completed"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendEvent(ctx, terminal, "done", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendTranscript(ctx, terminal, 0, "t", "u", []byte(`[]`), "completed", "completed"); err != nil {
		t.Fatal(err)
	}
	if err := st.PutOnlineResult(ctx, terminal, "quality", "t", "u", "contains", true, ""); err != nil {
		t.Fatal(err)
	}
	before := time.Now().UTC().Add(time.Minute)
	if n, err := st.ReapSessions(ctx, before, 10, true); err != nil || n != 1 {
		t.Fatalf("dry run n=%d err=%v", n, err)
	}
	if _, err := st.GetSession(ctx, terminal); err != nil {
		t.Fatalf("dry run deleted terminal session: %v", err)
	}
	if n, err := st.ReapSessions(ctx, before, 10, false); err != nil || n != 1 {
		t.Fatalf("reap n=%d err=%v", n, err)
	}
	if _, err := st.GetSession(ctx, terminal); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("terminal session still present: %v", err)
	}
	if _, err := st.GetSession(ctx, active); err != nil {
		t.Fatalf("active session deleted: %v", err)
	}
	if events, _ := st.EventsSince(ctx, terminal, 0); len(events) != 0 {
		t.Fatalf("events not cascaded: %+v", events)
	}
	if results, _ := st.ListOnlineResults(ctx, terminal); len(results) != 0 {
		t.Fatalf("online results not cascaded: %+v", results)
	}
}

func TestExternalBindingsAreTouchedExcludedFromLoadAndExpired(t *testing.T) {
	ctx := context.Background()
	st := NewMemStore()
	if err := st.BindSession(ctx, "external", "t", "a", "generation-1", 2); err != nil {
		t.Fatal(err)
	}
	if active, err := st.ActiveSessionsByReplica(ctx, "t", "a"); err != nil {
		t.Fatal(err)
	} else if active[2] != 0 {
		t.Fatalf("external affinity counted as active load: %v", active)
	}
	cutoff := time.Now().UTC()
	time.Sleep(time.Millisecond)
	if err := st.TouchSession(ctx, "external"); err != nil {
		t.Fatal(err)
	}
	if n, err := st.ReapSessions(ctx, cutoff, 10, false); err != nil || n != 0 {
		t.Fatalf("recently touched external binding reaped: n=%d err=%v", n, err)
	}
	if n, err := st.ReapSessions(ctx, time.Now().UTC().Add(time.Minute), 10, false); err != nil || n != 1 {
		t.Fatalf("inactive external binding not reaped: n=%d err=%v", n, err)
	}
}

func TestStore_SessionLifecycle(t *testing.T) {
	s := NewMemStore()
	ctx := context.Background()

	id, err := s.CreateSession(ctx, "agent1", 0)
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
	id, err := s.CreateSession(ctx, "agentA", 0)
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
	id, _ := s.CreateSession(ctx, "a", 0)
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
	id, _ := s.CreateSession(ctx, "a", 0)
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
	a1, _ := s.CreateSession(ctx, "agentA", 0)
	_, _ = s.CreateSession(ctx, "agentB", 0)
	a2, _ := s.CreateSession(ctx, "agentA", 0)
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
	id, _ := s.CreateSession(ctx, "agent1", 0)

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

func TestStore_AppendEventOnceIsIdempotent(t *testing.T) {
	s := NewMemStore()
	ctx := context.Background()
	id, _ := s.CreateSession(ctx, "a", 0)
	first, err := s.AppendEventOnce(ctx, id, "turn:0:event:0", "text", []byte(`{"text":"first"}`))
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := s.AppendEventOnce(ctx, id, "turn:0:event:0", "text", []byte(`{"text":"different replay payload"}`))
	if err != nil {
		t.Fatal(err)
	}
	if replayed != first {
		t.Fatalf("replayed seq=%d want original %d", replayed, first)
	}
	events, err := s.EventsSince(ctx, id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || string(events[0].Payload) != `{"text":"first"}` {
		t.Fatalf("events=%+v, want one original event", events)
	}
}

func TestStore_CreateSessionPersistsReplica(t *testing.T) {
	s := NewMemStore()
	ctx := context.Background()
	id, err := s.CreateSession(ctx, "agentA", 2)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	r, err := s.SessionReplica(ctx, id)
	if err != nil {
		t.Fatalf("SessionReplica: %v", err)
	}
	if r != 2 {
		t.Fatalf("replica: got %d, want 2", r)
	}
	row, _ := s.GetSession(ctx, id)
	if row.Replica != 2 {
		t.Fatalf("GetSession replica: got %d, want 2", row.Replica)
	}
}

func TestStore_SessionReplicaNotFound(t *testing.T) {
	s := NewMemStore()
	if _, err := s.SessionReplica(context.Background(), "nope"); err == nil {
		t.Fatal("expected error for unknown session")
	}
}

func TestActiveSessionsByReplica(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	id0a, _ := s.CreateSession(ctx, "ag", 0)
	id0b, _ := s.CreateSession(ctx, "ag", 0)
	_ = s.SetSessionStatus(ctx, id0b, "running")
	id1a, _ := s.CreateSession(ctx, "ag", 1)
	id1done, _ := s.CreateSession(ctx, "ag", 1)
	_ = s.SetSessionStatus(ctx, id1done, "completed")
	_, _ = s.CreateSession(ctx, "other", 0)
	_, _ = s.CreateSessionForTenant(ctx, "other-tenant", "ag", 0)
	_ = id0a
	_ = id1a

	m, err := s.ActiveSessionsByReplica(ctx, "default", "ag")
	if err != nil {
		t.Fatalf("ActiveSessionsByReplica: %v", err)
	}
	if m[0] != 2 {
		t.Fatalf("replica 0 active = %d, want 2", m[0])
	}
	if m[1] != 1 {
		t.Fatalf("replica 1 active = %d, want 1 (terminal excluded)", m[1])
	}
}

func TestLimitExceededIsTerminalForActiveCount(t *testing.T) {
	// A limit_exceeded session must NOT count as active load — otherwise the
	// autoscaler can never drain a replica that hosted a breached session.
	st := NewMemStore()
	ctx := context.Background()
	id, err := st.CreateSession(ctx, "a1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSessionStatus(ctx, id, "limit_exceeded"); err != nil {
		t.Fatal(err)
	}
	m, err := st.ActiveSessionsByReplica(ctx, "default", "a1")
	if err != nil {
		t.Fatal(err)
	}
	if m[0] != 0 {
		t.Errorf("limit_exceeded counted as active: %v", m)
	}
}

func itoa(i int) string { return string(rune('0' + i)) }
