//go:build integration

package memory

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

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
