//go:build integration

package test

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	hrt "github.com/sausheong/harness/runtime"
	hmem "github.com/sausheong/harness/tool/memory"

	"github.com/sausheong/runtime/internal/memory"
)

// freshMemoryDB opens the live Postgres, skips when unreachable, drops and (via
// t.Cleanup) re-drops memory_events so each test starts from a clean table, and
// returns the handle. Mirrors the setup block in memory_summary_e2e_test.go.
func freshMemoryDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Ping(); err != nil {
		t.Skipf("postgres not reachable: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE IF EXISTS memory_events CASCADE`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DROP TABLE IF EXISTS memory_events CASCADE`) })
	return db
}

// contents extracts entry contents in order, for order-independent set asserts.
func contents(es []hmem.Entry) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.Content)
	}
	return out
}

func hasContent(es []hmem.Entry, want string) bool {
	for _, e := range es {
		if e.Content == want {
			return true
		}
	}
	return false
}

// TestSubjectForwarding_FactIsolation proves strict actor namespacing on the
// direct Store fact path (List works with no embedder, so this is the primary
// isolation assertion and needs none): a fact saved under actor "alice" is
// visible ONLY to alice's actor-scoped read, a fact under "bob" ONLY to bob, and
// a fact saved under the tenant-wide bucket (actor="") is visible ONLY to the ""
// reader — strict both ways, no shared+private union.
func TestSubjectForwarding_FactIsolation(t *testing.T) {
	ctx := context.Background()
	db := freshMemoryDB(t)
	defer db.Close()

	st, err := memory.NewStore(ctx, db, "acme")
	if err != nil {
		t.Fatal(err)
	}

	// Save one fact per bucket: alice, bob, and the tenant-wide "" bucket.
	if _, err := st.Save(memory.WithActor(ctx, "alice"), hmem.Entry{Content: "alice-fact"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Save(memory.WithActor(ctx, "bob"), hmem.Entry{Content: "bob-fact"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Save(ctx, hmem.Entry{Content: "shared-fact"}); err != nil { // actor=""
		t.Fatal(err)
	}

	// alice sees ONLY alice-fact.
	aliceList, err := st.List(memory.WithActor(ctx, "alice"), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(aliceList) != 1 || aliceList[0].Content != "alice-fact" {
		t.Fatalf("alice List = %v, want exactly [alice-fact]", contents(aliceList))
	}
	if hasContent(aliceList, "bob-fact") || hasContent(aliceList, "shared-fact") {
		t.Fatalf("alice List leaked another actor's/shared fact: %v", contents(aliceList))
	}

	// bob sees ONLY bob-fact.
	bobList, err := st.List(memory.WithActor(ctx, "bob"), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(bobList) != 1 || bobList[0].Content != "bob-fact" {
		t.Fatalf("bob List = %v, want exactly [bob-fact]", contents(bobList))
	}

	// The tenant-wide "" reader sees ONLY the shared fact — NOT alice's or bob's
	// (strict isolation both ways: actor-scoped rows are invisible to "").
	sharedList, err := st.List(ctx, "") // actor=""
	if err != nil {
		t.Fatal(err)
	}
	if len(sharedList) != 1 || sharedList[0].Content != "shared-fact" {
		t.Fatalf("tenant-wide List = %v, want exactly [shared-fact]", contents(sharedList))
	}
	if hasContent(sharedList, "alice-fact") || hasContent(sharedList, "bob-fact") {
		t.Fatalf("tenant-wide List leaked an actor-scoped fact: %v", contents(sharedList))
	}
}

// summaryActorID returns the actor_id of the single live summary row for
// (acme, sessionID) via direct SQL, mirroring the liveness predicate used by
// countLiveSummaries. Fails if there is not exactly one such row.
func summaryActorID(t *testing.T, db *sql.DB, sessionID string) string {
	t.Helper()
	const q = `
SELECT e.actor_id FROM memory_events e
 WHERE e.tenant_id='acme' AND e.session_id=$1 AND e.kind='summary' AND e.op IN ('create','update')
   AND NOT EXISTS (SELECT 1 FROM memory_events s WHERE s.tenant_id='acme' AND s.supersedes=e.entry_id)
   AND NOT EXISTS (SELECT 1 FROM memory_events d WHERE d.tenant_id='acme' AND d.op='delete' AND d.entry_id=e.entry_id)`
	var actor string
	if err := db.QueryRow(q, sessionID).Scan(&actor); err != nil {
		t.Fatalf("summaryActorID(%q): %v", sessionID, err)
	}
	return actor
}

// TestSubjectForwarding_SummaryActorStamp proves the summary write path stamps
// the actor: PutSessionSummary under WithActor(ctx,"alice") writes a live summary
// row with actor_id='alice' (direct SQL), while the session-keyed read
// GetSessionSummary is unaffected by the actor and still returns the digest.
func TestSubjectForwarding_SummaryActorStamp(t *testing.T) {
	ctx := context.Background()
	db := freshMemoryDB(t)
	defer db.Close()

	st, err := memory.NewStore(ctx, db, "acme")
	if err != nil {
		t.Fatal(err)
	}

	if err := st.PutSessionSummary(memory.WithActor(ctx, "alice"), "Sa", "digest-a"); err != nil {
		t.Fatal(err)
	}

	// The live summary row for session Sa is stamped with alice's actor_id.
	if got := summaryActorID(t, db, "Sa"); got != "alice" {
		t.Fatalf("summary actor_id = %q, want alice", got)
	}

	// The read is session-keyed, not actor-keyed: it returns the digest whether
	// or not the reading ctx carries the actor.
	got, ok, err := st.GetSessionSummary(memory.WithActor(ctx, "alice"), "Sa")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || got != "digest-a" {
		t.Fatalf("GetSessionSummary(alice) = (%q,%v), want (digest-a,true)", got, ok)
	}
	// Same read with a bare ctx (session-keyed read is actor-independent).
	got, ok, err = st.GetSessionSummary(ctx, "Sa")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || got != "digest-a" {
		t.Fatalf("GetSessionSummary(bare) = (%q,%v), want (digest-a,true)", got, ok)
	}
}

// TestSubjectForwarding_KGPathActorStamp proves the KG ingest path carries the
// actor end-to-end: driving a summary Ingest via ForSession(sid,"alice") lands
// (asynchronously) a summary row stamped actor_id='alice'. This exercises the
// detached ingest-write path (StrategyContext.Actor → re-attached background ctx
// → Store's actorFrom), the one path where the actor travels as data rather than
// on the live turn ctx. Uses the sibling countingSummarizer / waitForSummary
// helpers (same test package). No embedder needed — summaries are embedder-
// independent.
//
// NOTE on anti-spoof (plan assertion #5): the strip-then-set edge behavior lives
// in controlplane.forwardSubject, which is unexported (package controlplane) and
// therefore unreachable from package test. It is proven directly by
// controlplane/api_subject_test.go (TestForwardSubject_StripsThenSets,
// _NoPrincipalStripsAll, _OffIsInert). Re-testing it here would duplicate those
// internals, so it is referenced rather than duplicated.
func TestSubjectForwarding_KGPathActorStamp(t *testing.T) {
	ctx := context.Background()
	db := freshMemoryDB(t)
	defer db.Close()

	st, err := memory.NewStore(ctx, db, "acme")
	if err != nil {
		t.Fatal(err)
	}
	sum := &countingSummarizer{}
	kg := memory.NewKG(st, 5, 0.5, memory.WithStrategies(memory.NewSummaryStrategy(sum, 2)))

	// A 2-message thread satisfies the strategy's minMsgs=2 gate.
	thread := []hrt.Message{
		{Role: "user", Content: "tell me something"},
		{Role: "assistant", Content: "sure"},
	}

	// Drive the ingest through the session+actor-bound view. Ingest is async;
	// poll for the summary write to land (mirrors the sibling e2e).
	kg.ForSession("Sa", "alice").Ingest(ctx, thread)
	waitForSummary(t, st, "Sa", "summary #1")

	// The resulting live summary row is stamped with alice's actor — the actor
	// survived the KGFn→ingest→StrategyContext→Store write path.
	if got := summaryActorID(t, db, "Sa"); got != "alice" {
		t.Fatalf("KG-path summary actor_id = %q, want alice", got)
	}
}
