//go:build integration

package store

import (
	"context"
	"testing"
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
