//go:build integration

package memory

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	hmem "github.com/sausheong/harness/tool/memory"
)

const dsn = "postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable"

// freshStore opens the DB, drops + recreates memory_events, and returns a Store
// pinned to tenant. t.Cleanup drops the table so sibling tests don't see it.
func freshStore(t *testing.T, tenant string) (*Store, *sql.DB) {
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
	st, err := NewStore(context.Background(), db, tenant)
	if err != nil {
		t.Fatal(err)
	}
	return st, db
}

// compile-time proof the Store satisfies the harness interface.
var _ hmem.MemoryStore = (*Store)(nil)

func TestStore_SaveGetRoundTrip(t *testing.T) {
	st, db := freshStore(t, "alpha")
	defer db.Close()
	ctx := context.Background()

	saved, err := st.Save(ctx, hmem.Entry{Content: "hello", Tags: []string{"x"}, Origin: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	if saved.ID == "" || saved.Content != "hello" {
		t.Fatalf("bad saved entry: %+v", saved)
	}
	if !saved.CreatedAt.Equal(saved.UpdatedAt) {
		t.Fatalf("fresh entry CreatedAt != UpdatedAt: %+v", saved)
	}
	got, ok, err := st.Get(ctx, saved.ID)
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.Content != "hello" || got.Origin != "agent" || len(got.Tags) != 1 || got.Tags[0] != "x" {
		t.Fatalf("get mismatch: %+v", got)
	}
}

func TestStore_ListOrderingAndTagFilter(t *testing.T) {
	st, db := freshStore(t, "alpha")
	defer db.Close()
	ctx := context.Background()
	a, _ := st.Save(ctx, hmem.Entry{Content: "first", Tags: []string{"k"}})
	b, _ := st.Save(ctx, hmem.Entry{Content: "second"})
	all, err := st.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].ID != a.ID || all[1].ID != b.ID {
		t.Fatalf("list order wrong: %+v", all)
	}
	tagged, _ := st.List(ctx, "k")
	if len(tagged) != 1 || tagged[0].ID != a.ID {
		t.Fatalf("tag filter wrong: %+v", tagged)
	}
}

func TestStore_UpdateSupersedes(t *testing.T) {
	st, db := freshStore(t, "alpha")
	defer db.Close()
	ctx := context.Background()
	orig, _ := st.Save(ctx, hmem.Entry{Content: "v1", Tags: []string{"t"}, Origin: "agent"})
	upd, err := st.Update(ctx, orig.ID, "v2")
	if err != nil {
		t.Fatal(err)
	}
	if upd.ID == orig.ID {
		t.Fatal("Update must mint a fresh id")
	}
	if !upd.CreatedAt.Equal(orig.CreatedAt) {
		t.Fatalf("Update must preserve birth CreatedAt: orig=%v new=%v", orig.CreatedAt, upd.CreatedAt)
	}
	if upd.UpdatedAt.Before(orig.UpdatedAt) {
		t.Fatalf("UpdatedAt not advanced: %v", upd.UpdatedAt)
	}
	if len(upd.Tags) != 1 || upd.Tags[0] != "t" || upd.Origin != "agent" {
		t.Fatalf("Update must carry tags+origin: %+v", upd)
	}
	if _, ok, _ := st.Get(ctx, orig.ID); ok {
		t.Fatal("old id must be invalid after update")
	}
	all, _ := st.List(ctx, "")
	if len(all) != 1 || all[0].ID != upd.ID || all[0].Content != "v2" {
		t.Fatalf("after update want one live row v2: %+v", all)
	}
	if _, err := st.Update(ctx, "mem_2000-01-01_deadbeef", "x"); err != hmem.ErrNotFound {
		t.Fatalf("update unknown id: want ErrNotFound, got %v", err)
	}
}

func TestStore_RemoveTombstone(t *testing.T) {
	st, db := freshStore(t, "alpha")
	defer db.Close()
	ctx := context.Background()
	e, _ := st.Save(ctx, hmem.Entry{Content: "doomed"})
	if err := st.Remove(ctx, e.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.Get(ctx, e.ID); ok {
		t.Fatal("removed entry must be gone")
	}
	all, _ := st.List(ctx, "")
	if len(all) != 0 {
		t.Fatalf("list after remove must be empty: %+v", all)
	}
	if err := st.Remove(ctx, "mem_2000-01-01_deadbeef"); err != nil {
		t.Fatalf("remove unknown id must be nil: %v", err)
	}
}

func TestStore_OriginPersistedVerbatim(t *testing.T) {
	st, db := freshStore(t, "alpha")
	defer db.Close()
	ctx := context.Background()
	e, _ := st.Save(ctx, hmem.Entry{Content: "r", Origin: "review"})
	got, _, _ := st.Get(ctx, e.ID)
	if got.Origin != "review" {
		t.Fatalf("origin not persisted verbatim: %q", got.Origin)
	}
}

func TestStore_CrossTenantIsolation(t *testing.T) {
	alpha, db := freshStore(t, "alpha")
	defer db.Close()
	beta, err := NewStore(context.Background(), db, "beta")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ae, _ := alpha.Save(ctx, hmem.Entry{Content: "alpha-secret"})

	if list, _ := beta.List(ctx, ""); len(list) != 0 {
		t.Fatalf("beta must not see alpha's entries: %+v", list)
	}
	if _, ok, _ := beta.Get(ctx, ae.ID); ok {
		t.Fatal("beta must not Get alpha's entry")
	}
	if _, err := beta.Update(ctx, ae.ID, "hijack"); err != hmem.ErrNotFound {
		t.Fatalf("beta update of alpha id: want ErrNotFound, got %v", err)
	}
	if err := beta.Remove(ctx, ae.ID); err != nil {
		t.Fatalf("beta remove of alpha id must no-op nil: %v", err)
	}
	if _, ok, _ := alpha.Get(ctx, ae.ID); !ok {
		t.Fatal("alpha lost its entry after beta's no-op remove")
	}
}

// fixedEmbedder maps content→a deterministic vector for hermetic-but-real pgvector
// math. Unknown content embeds to a far-away vector. Dim is 3 for tests.
type fixedEmbedder struct {
	vecs map[string][]float32
	fail bool
}

func (f *fixedEmbedder) Dim() int { return 3 }
func (f *fixedEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	if f.fail {
		return nil, fmt.Errorf("embed failed")
	}
	if v, ok := f.vecs[text]; ok {
		return v, nil
	}
	return []float32{0, 0, 1}, nil
}

func freshStoreEmbedded(t *testing.T, tenant string, emb Embedder) (*Store, *sql.DB) {
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
	st, err := NewStore(context.Background(), db, tenant, WithEmbedder(emb))
	if err != nil {
		t.Fatal(err)
	}
	return st, db
}

func TestStore_SaveWritesEmbedding(t *testing.T) {
	emb := &fixedEmbedder{vecs: map[string][]float32{"cats": {1, 0, 0}}}
	st, db := freshStoreEmbedded(t, "alpha", emb)
	defer db.Close()
	ctx := context.Background()
	e, err := st.Save(ctx, hmem.Entry{Content: "cats"})
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(1) FROM memory_events WHERE entry_id=$1 AND embedding IS NOT NULL`, e.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("embedding not written: n=%d", n)
	}
}

func TestStore_SaveDegradesToNullOnEmbedError(t *testing.T) {
	emb := &fixedEmbedder{fail: true}
	st, db := freshStoreEmbedded(t, "alpha", emb)
	defer db.Close()
	ctx := context.Background()
	e, err := st.Save(ctx, hmem.Entry{Content: "x"})
	if err != nil {
		t.Fatalf("save must succeed despite embed failure: %v", err)
	}
	if _, ok, _ := st.Get(ctx, e.ID); !ok {
		t.Fatal("entry must be retrievable after embed-fail degrade")
	}
	var nullCount int
	db.QueryRow(`SELECT count(1) FROM memory_events WHERE entry_id=$1 AND embedding IS NULL`, e.ID).Scan(&nullCount)
	if nullCount != 1 {
		t.Fatalf("expected NULL embedding row, got %d", nullCount)
	}
}

func TestStore_SearchSimilar(t *testing.T) {
	emb := &fixedEmbedder{vecs: map[string][]float32{
		"cats are great": {1, 0, 0},
		"felines rule":   {0.9, 0.1, 0},
		"stock prices":   {0, 1, 0},
	}}
	st, db := freshStoreEmbedded(t, "alpha", emb)
	defer db.Close()
	ctx := context.Background()
	st.Save(ctx, hmem.Entry{Content: "cats are great"})
	st.Save(ctx, hmem.Entry{Content: "felines rule"})
	st.Save(ctx, hmem.Entry{Content: "stock prices"})

	hits, err := st.SearchSimilar(ctx, []float32{1, 0, 0}, 5, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("want 2 hits above floor, got %d: %+v", len(hits), hits)
	}
	if hits[0].Content != "cats are great" {
		t.Fatalf("nearest should be exact match, got %q", hits[0].Content)
	}
	hits2, _ := st.SearchSimilar(ctx, []float32{1, 0, 0}, 1, 0.0)
	if len(hits2) != 1 {
		t.Fatalf("K=1 cap failed: %d", len(hits2))
	}
}

func TestStore_SearchSimilarSkipsNullAndRespectsLiveness(t *testing.T) {
	emb := &fixedEmbedder{vecs: map[string][]float32{"keep": {1, 0, 0}, "gone": {1, 0, 0}}}
	st, db := freshStoreEmbedded(t, "alpha", emb)
	defer db.Close()
	ctx := context.Background()
	keep, _ := st.Save(ctx, hmem.Entry{Content: "keep"})
	gone, _ := st.Save(ctx, hmem.Entry{Content: "gone"})
	st.Remove(ctx, gone.ID)
	emb.fail = true
	st.Save(ctx, hmem.Entry{Content: "nullrow"})
	emb.fail = false

	hits, _ := st.SearchSimilar(ctx, []float32{1, 0, 0}, 10, 0.0)
	if len(hits) != 1 || hits[0].ID != keep.ID {
		t.Fatalf("want only the live, embedded entry; got %+v", hits)
	}
}

func TestStore_SearchSimilarCrossTenantIsolation(t *testing.T) {
	emb := &fixedEmbedder{vecs: map[string][]float32{"alpha-secret": {1, 0, 0}}}
	alpha, db := freshStoreEmbedded(t, "alpha", emb)
	defer db.Close()
	beta, err := NewStore(context.Background(), db, "beta", WithEmbedder(emb))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	alpha.Save(ctx, hmem.Entry{Content: "alpha-secret"})
	hits, _ := beta.SearchSimilar(ctx, []float32{1, 0, 0}, 10, 0.0)
	if len(hits) != 0 {
		t.Fatalf("beta must not recall alpha's memory: %+v", hits)
	}
}

func TestStore_SessionSummaryUpsertAndGet(t *testing.T) {
	st, db := freshStore(t, "acme")
	defer db.Close()
	ctx := context.Background()

	// No summary yet.
	if _, ok, err := st.GetSessionSummary(ctx, "sess-1"); err != nil || ok {
		t.Fatalf("expected no summary, got ok=%v err=%v", ok, err)
	}
	// First write creates it.
	if err := st.PutSessionSummary(ctx, "sess-1", "digest v1"); err != nil {
		t.Fatal(err)
	}
	got, ok, err := st.GetSessionSummary(ctx, "sess-1")
	if err != nil || !ok || got != "digest v1" {
		t.Fatalf("after first put: got=%q ok=%v err=%v", got, ok, err)
	}
	// Second write supersedes — still exactly one live row, updated content.
	if err := st.PutSessionSummary(ctx, "sess-1", "digest v2"); err != nil {
		t.Fatal(err)
	}
	got, ok, err = st.GetSessionSummary(ctx, "sess-1")
	if err != nil || !ok || got != "digest v2" {
		t.Fatalf("after second put: got=%q ok=%v err=%v", got, ok, err)
	}
	// Exactly one live summary row for the session (supersede chain, not growth).
	var live int
	if err := db.QueryRow(
		`SELECT count(1) FROM memory_events e
		 WHERE e.tenant_id='acme' AND e.session_id='sess-1' AND e.kind='summary'
		   AND e.op IN ('create','update')
		   AND NOT EXISTS (SELECT 1 FROM memory_events s
		                   WHERE s.tenant_id='acme' AND s.supersedes = e.entry_id)`).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if live != 1 {
		t.Fatalf("want exactly one live summary row, got %d", live)
	}
	// Isolation: a different session has no summary.
	if _, ok, _ := st.GetSessionSummary(ctx, "sess-2"); ok {
		t.Fatal("sess-2 should have no summary")
	}
}

func TestStore_DedupFloorSeparation(t *testing.T) {
	// Synthetic vectors with known cosine similarities to the query {1,0,0}.
	// (The 0.7/0.85 thresholds below are illustrative test values for proving
	// floor separation, not the product defaults.)
	//   near    = {0.97, 0.243, 0}  → cosine ≈ 0.970 (>= 0.85 dedup floor)
	//   related = {0.8, 0.6, 0}     → cosine = 0.800 (between a 0.7 floor and 0.85 dedup)
	emb := &fixedEmbedder{vecs: map[string][]float32{
		"near":    {0.97, 0.243, 0},
		"related": {0.8, 0.6, 0},
	}}
	st, db := freshStoreEmbedded(t, "alpha", emb)
	defer db.Close()
	ctx := context.Background()
	st.Save(ctx, hmem.Entry{Content: "near"})
	st.Save(ctx, hmem.Entry{Content: "related"})

	// At the dedup floor, only "near" is a duplicate of the query.
	dupHits, err := st.SearchSimilar(ctx, []float32{1, 0, 0}, 1, 0.85)
	if err != nil {
		t.Fatal(err)
	}
	if len(dupHits) != 1 || dupHits[0].Content != "near" {
		t.Fatalf("dedup floor 0.85 should match only 'near': %+v", dupHits)
	}

	// "related" (0.80) sits above a 0.7 recall-style floor but below the 0.85
	// dedup floor: it would be recalled, but is NOT treated as a duplicate.
	recallHits, _ := st.SearchSimilar(ctx, []float32{1, 0, 0}, 5, 0.7)
	var sawRelated bool
	for _, h := range recallHits {
		if h.Content == "related" {
			sawRelated = true
		}
	}
	if !sawRelated {
		t.Fatalf("'related' should clear the recall floor 0.7: %+v", recallHits)
	}
}

// TestStore_SessionSummaryConcurrent defends the exact gap the final review
// found: PutSessionSummary is a non-atomic read-then-write, and same-session
// summary writes overlap (turn N+1's goroutine races turn N's). Without the
// per-session lock, concurrent writers read the same prevID and both go live,
// forking the supersede chain into multiple live rows. With it, exactly one
// live kind='summary' row survives for the (tenant, session).
func TestStore_SessionSummaryConcurrent(t *testing.T) {
	st, db := freshStore(t, "acme")
	defer db.Close()
	ctx := context.Background()

	const n = 20
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			if err := st.PutSessionSummary(ctx, "S", fmt.Sprintf("digest-%d", i)); err != nil {
				t.Errorf("put %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	// Exactly one live summary row: a create|update row for the session that is
	// neither superseded nor tombstoned (mirrors liveSummaryLiveness's predicates).
	var live int
	if err := db.QueryRow(
		`SELECT count(1) FROM memory_events e
		 WHERE e.tenant_id='acme' AND e.session_id='S' AND e.kind='summary'
		   AND e.op IN ('create','update')
		   AND NOT EXISTS (SELECT 1 FROM memory_events s
		                   WHERE s.tenant_id='acme' AND s.supersedes = e.entry_id)
		   AND NOT EXISTS (SELECT 1 FROM memory_events d
		                   WHERE d.tenant_id='acme' AND d.op='delete' AND d.entry_id = e.entry_id)`).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if live != 1 {
		t.Fatalf("want exactly one live summary row after %d concurrent writes, got %d", n, live)
	}
	// And it is still retrievable.
	if _, ok, err := st.GetSessionSummary(ctx, "S"); err != nil || !ok {
		t.Fatalf("summary should still be retrievable: ok=%v err=%v", ok, err)
	}
}

// TestStore_ListGetExcludeSummaries proves summaries do not leak into the
// agent-facing fact queries (List/Get, built on liveSelect) while remaining
// retrievable via the dedicated summary path (GetSessionSummary).
func TestStore_ListGetExcludeSummaries(t *testing.T) {
	st, db := freshStore(t, "acme")
	defer db.Close()
	ctx := context.Background()

	fact, err := st.Save(ctx, hmem.Entry{Content: "a real fact", Tags: []string{"t"}, Origin: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutSessionSummary(ctx, "S", "the summary"); err != nil {
		t.Fatal(err)
	}

	// List returns only the fact, never the summary.
	got, err := st.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("List should return exactly the one fact, got %d: %+v", len(got), got)
	}
	if got[0].Content != "a real fact" {
		t.Fatalf("List returned wrong entry: %+v", got[0])
	}
	for _, e := range got {
		if e.Content == "the summary" || e.Origin == "summary" {
			t.Fatalf("summary leaked into List: %+v", e)
		}
	}

	// Get on the fact id still works.
	if _, ok, err := st.Get(ctx, fact.ID); err != nil || !ok {
		t.Fatalf("Get on fact: ok=%v err=%v", ok, err)
	}

	// The summary is still retrievable via the dedicated path (not broken, just hidden).
	if s, ok, err := st.GetSessionSummary(ctx, "S"); err != nil || !ok || s != "the summary" {
		t.Fatalf("GetSessionSummary: got=%q ok=%v err=%v", s, ok, err)
	}
}

// backdate ages every row in the tenant's table so grace-window tests can run
// without waiting. Direct SQL: the Store has no API to set created_at.
func backdate(t *testing.T, db *sql.DB, interval string) {
	t.Helper()
	if _, err := db.Exec(`UPDATE memory_events SET created_at = now() - $1::interval`, interval); err != nil {
		t.Fatal(err)
	}
}

// countRows returns the total row count (live + dead) for the tenant.
func countRows(t *testing.T, db *sql.DB, tenant string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM memory_events WHERE tenant_id=$1`, tenant).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestGCOnce_ReapsSupersededFacts(t *testing.T) {
	st, db := freshStore(t, "alpha")
	defer db.Close()
	ctx := context.Background()
	e, _ := st.Save(ctx, hmem.Entry{Content: "v1"})
	e2, _ := st.Update(ctx, e.ID, "v2")
	_, _ = st.Update(ctx, e2.ID, "v3")
	// 3 rows: create(v1, dead) + update(v2, dead) + update(v3, live).
	if got := countRows(t, db, "alpha"); got != 3 {
		t.Fatalf("pre-GC rows = %d, want 3", got)
	}
	n, err := st.GCOnce(ctx, 0, 1000) // grace=0: reap regardless of age
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("GCOnce deleted %d, want 2", n)
	}
	if got := countRows(t, db, "alpha"); got != 1 {
		t.Fatalf("post-GC rows = %d, want 1 (live only)", got)
	}
	// Live entry still readable, content intact.
	live, ok, err := st.Get(ctx, findLiveID(t, db, "alpha"))
	if err != nil || !ok || live.Content != "v3" {
		t.Fatalf("live entry lost: ok=%v content=%q err=%v", ok, live.Content, err)
	}
}

// findLiveID returns the single live fact entry_id for the tenant (test helper).
func findLiveID(t *testing.T, db *sql.DB, tenant string) string {
	t.Helper()
	var id string
	err := db.QueryRow(`
SELECT e.entry_id FROM memory_events e
WHERE e.tenant_id=$1 AND e.kind<>'summary' AND e.op IN ('create','update')
  AND NOT EXISTS (SELECT 1 FROM memory_events s WHERE s.tenant_id=$1 AND s.supersedes=e.entry_id)
  AND NOT EXISTS (SELECT 1 FROM memory_events d WHERE d.tenant_id=$1 AND d.op='delete' AND d.entry_id=e.entry_id)
LIMIT 1`, tenant).Scan(&id)
	if err != nil {
		t.Fatalf("findLiveID: %v", err)
	}
	return id
}

func TestGCOnce_ReapsTombstonedFacts(t *testing.T) {
	st, db := freshStore(t, "alpha")
	defer db.Close()
	ctx := context.Background()
	e, _ := st.Save(ctx, hmem.Entry{Content: "doomed"})
	if err := st.Remove(ctx, e.ID); err != nil {
		t.Fatal(err)
	}
	// 2 rows: create(dead, tombstoned) + delete(tombstone, retained).
	n, err := st.GCOnce(ctx, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("GCOnce deleted %d, want 1 (the dead create row only)", n)
	}
	// The delete tombstone row is retained.
	if got := countRows(t, db, "alpha"); got != 1 {
		t.Fatalf("post-GC rows = %d, want 1 (tombstone retained)", got)
	}
	// Entry stays absent.
	if _, ok, _ := st.Get(ctx, e.ID); ok {
		t.Fatal("tombstoned entry resurrected")
	}
}

func TestGCOnce_GraceRespected(t *testing.T) {
	st, db := freshStore(t, "alpha")
	defer db.Close()
	ctx := context.Background()
	e, _ := st.Save(ctx, hmem.Entry{Content: "v1"})
	_, _ = st.Update(ctx, e.ID, "v2") // 1 dead + 1 live, both created "now"
	n, err := st.GCOnce(ctx, time.Hour, 1000) // grace=1h; rows are seconds old
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("GCOnce deleted %d within grace, want 0", n)
	}
	// Backdate past grace, then it reaps.
	backdate(t, db, "48 hours")
	n, err = st.GCOnce(ctx, time.Hour, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("GCOnce after backdate deleted %d, want 1", n)
	}
}

func TestGCOnce_LiveSetUntouched(t *testing.T) {
	st, db := freshStore(t, "alpha")
	defer db.Close()
	ctx := context.Background()
	_, _ = st.Save(ctx, hmem.Entry{Content: "a"})
	_, _ = st.Save(ctx, hmem.Entry{Content: "b"})
	n, err := st.GCOnce(ctx, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("GCOnce deleted %d live rows, want 0", n)
	}
	if got := countRows(t, db, "alpha"); got != 2 {
		t.Fatalf("post-GC rows = %d, want 2", got)
	}
}

func TestGCOnce_NoResurrectionAcrossBatches(t *testing.T) {
	st, db := freshStore(t, "alpha")
	defer db.Close()
	ctx := context.Background()
	e, _ := st.Save(ctx, hmem.Entry{Content: "v1"})
	e2, _ := st.Update(ctx, e.ID, "v2")
	_, _ = st.Update(ctx, e2.ID, "v3") // chain: v1<-v2<-v3 (v3 live)
	liveBefore, _ := st.List(ctx, "")
	// batch=1 forces multiple passes over the dead ancestors.
	n, err := st.GCOnce(ctx, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("GCOnce(batch=1) deleted %d, want 2", n)
	}
	liveAfter, _ := st.List(ctx, "")
	if len(liveBefore) != 1 || len(liveAfter) != 1 || liveBefore[0].Content != "v3" || liveAfter[0].Content != "v3" {
		t.Fatalf("live set changed: before=%v after=%v", liveBefore, liveAfter)
	}
}

func TestGCOnce_ReapsSummaryChain(t *testing.T) {
	st, db := freshStore(t, "alpha")
	defer db.Close()
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := st.PutSessionSummary(ctx, "sess-1", fmt.Sprintf("digest %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	// 5 summary rows: 4 dead (superseded) + 1 live.
	n, err := st.GCOnce(ctx, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Fatalf("GCOnce deleted %d summary rows, want 4", n)
	}
	got, ok, err := st.GetSessionSummary(ctx, "sess-1")
	if err != nil || !ok || got != "digest 4" {
		t.Fatalf("live summary lost: ok=%v got=%q err=%v", ok, got, err)
	}
}

func TestGCOnce_ActorIsolation(t *testing.T) {
	st, db := freshStore(t, "alpha")
	defer db.Close()
	ctxA := WithActor(context.Background(), "actorA")
	ctxB := WithActor(context.Background(), "actorB")
	ea, _ := st.Save(ctxA, hmem.Entry{Content: "a1"})
	_, _ = st.Update(ctxA, ea.ID, "a2") // A: 1 dead + 1 live
	eb, _ := st.Save(ctxB, hmem.Entry{Content: "b1"})
	_, _ = st.Update(ctxB, eb.ID, "b2") // B: 1 dead + 1 live
	n, err := st.GCOnce(context.Background(), 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("GCOnce deleted %d, want 2 (one dead per actor)", n)
	}
	if a, _ := st.List(ctxA, ""); len(a) != 1 || a[0].Content != "a2" {
		t.Fatalf("actorA live set wrong: %v", a)
	}
	if b, _ := st.List(ctxB, ""); len(b) != 1 || b[0].Content != "b2" {
		t.Fatalf("actorB live set wrong: %v", b)
	}
}

func TestGCOnce_BatchLoopDrainsBacklog(t *testing.T) {
	st, db := freshStore(t, "alpha")
	defer db.Close()
	ctx := context.Background()
	e, _ := st.Save(ctx, hmem.Entry{Content: "v0"})
	cur := e.ID
	for i := 1; i <= 10; i++ { // 10 updates ⇒ 10 dead + 1 live
		nx, _ := st.Update(ctx, cur, fmt.Sprintf("v%d", i))
		cur = nx.ID
	}
	n, err := st.GCOnce(ctx, 0, 3) // batch=3 forces ~4 passes
	if err != nil {
		t.Fatal(err)
	}
	if n != 10 {
		t.Fatalf("GCOnce(batch=3) deleted %d, want 10", n)
	}
	if got := countRows(t, db, "alpha"); got != 1 {
		t.Fatalf("post-GC rows = %d, want 1", got)
	}
}

func TestSaveKind_StampsKindAndListExcludesEpisodes(t *testing.T) {
	st, db := freshStore(t, "alpha")
	defer db.Close()
	ctx := context.Background()
	// A fact via the frozen Save path.
	if _, err := st.Save(ctx, hmem.Entry{Content: "user likes go"}); err != nil {
		t.Fatal(err)
	}
	// An episode via SaveKind.
	if _, err := st.SaveKind(ctx, hmem.Entry{Content: "deployed staging", Origin: "ingest"}, KindEpisode); err != nil {
		t.Fatal(err)
	}
	// The kind column is stamped correctly.
	var episodeKinds int
	if err := db.QueryRow(`SELECT count(*) FROM memory_events WHERE tenant_id='alpha' AND kind='episode'`).Scan(&episodeKinds); err != nil {
		t.Fatal(err)
	}
	if episodeKinds != 1 {
		t.Fatalf("episode rows = %d, want 1", episodeKinds)
	}
	// List (memory{list} tool) returns facts ONLY, not the episode.
	all, err := st.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Content != "user likes go" {
		t.Fatalf("List must return facts only (not episodes), got %v", all)
	}
}

func TestSaveDelegatesToFactKind(t *testing.T) {
	st, db := freshStore(t, "alpha")
	defer db.Close()
	ctx := context.Background()
	e, err := st.Save(ctx, hmem.Entry{Content: "f1"})
	if err != nil {
		t.Fatal(err)
	}
	var kind string
	if err := db.QueryRow(`SELECT kind FROM memory_events WHERE entry_id=$1`, e.ID).Scan(&kind); err != nil {
		t.Fatal(err)
	}
	if kind != KindFact {
		t.Fatalf("Save must stamp kind=fact, got %q", kind)
	}
}
