//go:build integration

package test

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	hrt "github.com/sausheong/harness/runtime"

	"github.com/sausheong/runtime/internal/memory"
)

// countingSummarizer is a fake memory.Summarizer whose Summarize returns a
// per-call-incrementing digest ("summary #1", "summary #2", ...). It ignores
// the thread content entirely, so the assertions below turn on which call
// produced the live row — making supersede-per-turn observable and
// deterministic (no network, no real LLM).
type countingSummarizer struct {
	mu sync.Mutex
	n  int
}

func (c *countingSummarizer) Summarize(_ context.Context, _ []hrt.Message) (string, error) {
	c.mu.Lock()
	c.n++
	n := c.n
	c.mu.Unlock()
	return "summary #" + itoa(n), nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// waitForSummary polls GetSessionSummary until it equals want or times out
// (~2s), mirroring the sibling ingest e2e's waitForContent — Ingest is async
// (spawns a goroutine), so we poll for the write to land rather than sleep.
func waitForSummary(t *testing.T, st *memory.Store, sessionID, want string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		got, ok, err := st.GetSessionSummary(context.Background(), sessionID)
		if err != nil {
			t.Fatal(err)
		}
		if ok && got == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	got, _, _ := st.GetSessionSummary(context.Background(), sessionID)
	t.Fatalf("summary for session %q never became %q (last=%q)", sessionID, want, got)
}

// countLiveSummaries counts live (non-superseded, non-deleted) kind='summary'
// rows for (acme, sessionID) directly via the DSN, using the liveness predicate.
func countLiveSummaries(t *testing.T, db *sql.DB, sessionID string) int {
	t.Helper()
	const q = `
SELECT count(*) FROM memory_events e
 WHERE e.tenant_id='acme' AND e.session_id=$1 AND e.kind='summary' AND e.op IN ('create','update')
   AND NOT EXISTS (SELECT 1 FROM memory_events s WHERE s.tenant_id='acme' AND s.supersedes=e.entry_id)
   AND NOT EXISTS (SELECT 1 FROM memory_events d WHERE d.tenant_id='acme' AND d.op='delete' AND d.entry_id=e.entry_id)`
	var n int
	if err := db.QueryRow(q, sessionID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestSummaryStrategyEndToEnd drives the rolling per-session summary through the
// real session-bound ingest path (ForSession→Ingest→runStrategies→
// WriteSupersede→PutSessionSummary) against real Postgres, with NO embedder —
// proving the summary is embedder-independent (NewStore takes no WithEmbedder,
// yet every assertion below still passes).
func TestSummaryStrategyEndToEnd(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Skipf("postgres not reachable: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE IF EXISTS memory_events CASCADE`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DROP TABLE IF EXISTS memory_events CASCADE`) })

	// No embedder: summary is embedder-independent (assertion #4).
	st, err := memory.NewStore(ctx, db, "acme")
	if err != nil {
		t.Fatal(err)
	}
	sum := &countingSummarizer{}
	kg := memory.NewKG(st, 5, 0.5, memory.WithStrategies(memory.NewSummaryStrategy(sum, 2)))

	// A 2-message thread satisfies the strategy's minMsgs=2 gate. Content is
	// irrelevant — the fake summarizer returns "summary #N" regardless.
	thread := func(turn string) []hrt.Message {
		return []hrt.Message{
			{Role: "user", Content: "turn " + turn + ": tell me something"},
			{Role: "assistant", Content: "sure, here is turn " + turn},
		}
	}

	// Drive 3 completed turns in one session S, polling after each so the async
	// write lands before the next turn (deterministic supersede ordering).
	kg.ForSession("S", "").Ingest(ctx, thread("1"))
	waitForSummary(t, st, "S", "summary #1")
	kg.ForSession("S", "").Ingest(ctx, thread("2"))
	waitForSummary(t, st, "S", "summary #2")
	kg.ForSession("S", "").Ingest(ctx, thread("3"))
	waitForSummary(t, st, "S", "summary #3")

	// Assertion 1: supersede, not accumulate — exactly ONE live summary row.
	if n := countLiveSummaries(t, db, "S"); n != 1 {
		t.Fatalf("want exactly 1 live summary row for session S after 3 turns, got %d", n)
	}

	// Assertion 2: latest content wins — GetSessionSummary returns "summary #3".
	got, ok, err := st.GetSessionSummary(ctx, "S")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || got != "summary #3" {
		t.Fatalf("latest summary should win: got (%q, %v), want (\"summary #3\", true)", got, ok)
	}

	// Assertion 3: per-session isolation — S2 gets its OWN single summary; S is
	// unchanged. Driving S2 once produces the next digest ("summary #4").
	kg.ForSession("S2", "").Ingest(ctx, thread("s2"))
	waitForSummary(t, st, "S2", "summary #4")
	if n := countLiveSummaries(t, db, "S2"); n != 1 {
		t.Fatalf("want exactly 1 live summary row for session S2, got %d", n)
	}
	if n := countLiveSummaries(t, db, "S"); n != 1 {
		t.Fatalf("session S must still have exactly 1 live summary after S2 write, got %d", n)
	}
	s2Got, s2OK, err := st.GetSessionSummary(ctx, "S2")
	if err != nil {
		t.Fatal(err)
	}
	if !s2OK || s2Got != "summary #4" {
		t.Fatalf("S2 should have its own latest digest: got (%q, %v), want (\"summary #4\", true)", s2Got, s2OK)
	}
	// S and S2 are distinct rows keyed on distinct session_ids with distinct content.
	if got == s2Got {
		t.Fatalf("S and S2 must hold distinct summaries, both = %q", got)
	}

	// Assertion 5: recall injects the summary on resume. The in-memory thread is
	// empty (a fresh attach), yet recallForSession surfaces the session's own
	// stored summary. ShouldRecall requires >=3 words, so use a >=3-word query.
	// No embedder ⇒ the fact block is empty; the summary block is what appears.
	resume := kg.ForSession("S", "").Recall(ctx, "what did we discuss earlier")
	if !strings.Contains(resume, "summary #3") {
		t.Fatalf("recall on resume must surface the session's latest summary, got %q", resume)
	}
}
