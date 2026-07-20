//go:build integration

package store

import (
	"context"
	"os"
	"testing"
)

// dsn resolves the test DSN from the env with the standard fallback.
func dsn() string {
	if v := os.Getenv("RUNTIME_TEST_PG_DSN"); v != "" {
		return v
	}
	return pgTestDSN
}

func newTranscriptTestStore(t *testing.T) Store {
	t.Helper()
	ctx := context.Background()
	st, err := NewPGStore(ctx, dsn())
	if err != nil {
		t.Fatalf("NewPGStore (is postgres running at %s?): %v", dsn(), err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestTranscriptRoundTripAndUpsert(t *testing.T) {
	st := newTranscriptTestStore(t)
	ctx := context.Background()
	const agentID = "transcript-roundtrip-agent"

	// FK to sessions(id): must create a real session first.
	sid, err := st.CreateSession(ctx, agentID, 0)
	if err != nil {
		t.Fatal(err)
	}
	p := st.(*pgStore)
	t.Cleanup(func() {
		_, _ = p.db.ExecContext(context.Background(), `DELETE FROM session_transcripts WHERE session_id=$1`, sid)
		_, _ = p.db.ExecContext(context.Background(), `DELETE FROM sessions WHERE id=$1`, sid)
	})

	if err := st.AppendTranscript(ctx, sid, 0, "t1", "alice", []byte(`[{"role":"user","x":1}]`), "end_turn", "completed"); err != nil {
		t.Fatal(err)
	}

	// JSONB round-trip.
	var raw string
	if err := p.db.QueryRowContext(ctx,
		`SELECT entries::text FROM session_transcripts WHERE session_id=$1 AND turn_index=0`, sid).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw == "" {
		t.Fatalf("entries empty after round-trip")
	}

	// Re-append same (session,turn) upserts, no dup.
	if err := st.AppendTranscript(ctx, sid, 0, "t1", "alice", []byte(`[{"role":"user","x":2}]`), "end_turn", "completed"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := p.db.QueryRowContext(ctx,
		`SELECT count(*) FROM session_transcripts WHERE session_id=$1`, sid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("re-append created dup rows: got %d want 1", n)
	}
	var updated string
	if err := p.db.QueryRowContext(ctx,
		`SELECT entries->0->>'x' FROM session_transcripts WHERE session_id=$1 AND turn_index=0`, sid).Scan(&updated); err != nil {
		t.Fatal(err)
	}
	if updated != "2" {
		t.Fatalf("upsert did not replace entries: got x=%q want 2", updated)
	}
}

func TestOnlineResultRoundTripUpsertAndByTenant(t *testing.T) {
	st := newTranscriptTestStore(t)
	ctx := context.Background()
	const tenant = "online-result-test-tenant"
	const otherTenant = "online-result-test-other"
	// Use distinct synthetic session ids (no FK on online_eval_results).
	const s1 = "online-res-s1"
	const s2 = "online-res-s2"

	p := st.(*pgStore)
	t.Cleanup(func() {
		_, _ = p.db.ExecContext(context.Background(),
			`DELETE FROM online_eval_results WHERE session_id IN ($1,$2)`, s1, s2)
	})

	// Round-trip + upsert on (session, criterion).
	if err := st.PutOnlineResult(ctx, s1, "polite", tenant, "alice", "judge", true, "ok"); err != nil {
		t.Fatal(err)
	}
	if err := st.PutOnlineResult(ctx, s1, "polite", tenant, "alice", "judge", false, "changed"); err != nil {
		t.Fatal(err)
	}
	res, err := st.ListOnlineResults(ctx, s1)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Passed != false || res[0].Detail != "changed" {
		t.Fatalf("upsert wrong: %+v", res)
	}

	// Second criterion → two rows, ordered by criterion_name.
	if err := st.PutOnlineResult(ctx, s1, "fmt", tenant, "alice", "regex", true, ""); err != nil {
		t.Fatal(err)
	}
	res, err = st.ListOnlineResults(ctx, s1)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 || res[0].Criterion != "fmt" || res[1].Criterion != "polite" {
		t.Fatalf("list order/count wrong: %+v", res)
	}

	// A different tenant's row for isolation.
	if err := st.PutOnlineResult(ctx, s2, "polite", otherTenant, "bob", "judge", true, ""); err != nil {
		t.Fatal(err)
	}

	// By-tenant filter: only our tenant's two rows.
	byT, err := st.ListOnlineResultsByTenant(ctx, tenant, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(byT) != 2 {
		t.Fatalf("by-tenant filter: got %d want 2", len(byT))
	}
	for _, r := range byT {
		if r.Tenant != tenant {
			t.Fatalf("cross-tenant leak: %+v", r)
		}
	}

	// Limit is honored.
	lim, err := st.ListOnlineResultsByTenant(ctx, tenant, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(lim) != 1 {
		t.Fatalf("limit not honored: got %d want 1", len(lim))
	}
}
