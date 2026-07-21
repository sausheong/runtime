//go:build integration

package store

import (
	"context"
	"testing"
	"time"
)

// Matches the DSN convention used by the integration tests in test/.
const pgTestDSN = "postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable"

func newPGTestStore(t *testing.T) Store {
	t.Helper()
	ctx := context.Background()
	st, err := NewPGStore(ctx, pgTestDSN)
	if err != nil {
		t.Fatalf("NewPGStore (is postgres running at %s?): %v", pgTestDSN, err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestPGLimitExceededIsTerminalForActiveCount(t *testing.T) {
	// A limit_exceeded session must NOT count as active load — otherwise the
	// autoscaler can never drain a replica that hosted a breached session.
	st := newPGTestStore(t)
	ctx := context.Background()
	const agentID = "pg-limit-terminal-test"
	id, err := st.CreateSession(ctx, agentID, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		p := st.(*pgStore)
		_, _ = p.db.ExecContext(context.Background(), `DELETE FROM sessions WHERE id=$1`, id)
	})
	if err := st.SetSessionStatus(ctx, id, "limit_exceeded"); err != nil {
		t.Fatal(err)
	}
	m, err := st.ActiveSessionsByReplica(ctx, agentID)
	if err != nil {
		t.Fatal(err)
	}
	if m[0] != 0 {
		t.Errorf("limit_exceeded counted as active: %v", m)
	}
}

func TestPGFailureCategory(t *testing.T) {
	st := newPGTestStore(t)
	ctx := context.Background()
	const agentID = "pg-failcat-test"
	// Clean any prior rows for a repeatable run.
	p := st.(*pgStore)
	t.Cleanup(func() {
		_, _ = p.db.ExecContext(context.Background(), `DELETE FROM sessions WHERE agent_id=$1`, agentID)
	})
	_, _ = p.db.ExecContext(ctx, `DELETE FROM sessions WHERE agent_id=$1`, agentID)

	mk := func(cat string) string {
		id, err := st.CreateSession(ctx, agentID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if cat != "" {
			if err := st.SetFailureCategory(ctx, id, cat); err != nil {
				t.Fatal(err)
			}
			// Idempotent re-set (replay).
			if err := st.SetFailureCategory(ctx, id, cat); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	id1 := mk("tool_error")
	mk("tool_error")
	mk("none")
	mk("") // unclassified — must be omitted from the breakdown

	// GetSession round-trips the column.
	row, err := st.GetSession(ctx, id1)
	if err != nil {
		t.Fatal(err)
	}
	if row.FailureCategory != "tool_error" {
		t.Fatalf("GetSession category=%q, want tool_error", row.FailureCategory)
	}

	// Breakdown groups and omits ''.
	got, err := st.FailureBreakdownByAgent(ctx, agentID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if got["tool_error"] != 2 || got["none"] != 1 || len(got) != 2 {
		t.Fatalf("breakdown=%v, want {tool_error:2, none:1}", got)
	}

	// since in the future ⇒ empty (all rows are older than a far-future cutoff).
	future := time.Now().Add(24 * time.Hour)
	got2, err := st.FailureBreakdownByAgent(ctx, agentID, future)
	if err != nil {
		t.Fatal(err)
	}
	if len(got2) != 0 {
		t.Fatalf("future-since breakdown=%v, want empty", got2)
	}
}
