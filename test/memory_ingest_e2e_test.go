//go:build integration

package test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	hrt "github.com/sausheong/harness/runtime"

	"github.com/sausheong/runtime/internal/memory"
)

// ingestEmbedder maps known content→deterministic vectors (dim 3); unknown→far.
type ingestEmbedder struct{ vecs map[string][]float32 }

func (e ingestEmbedder) Dim() int { return 3 }
func (e ingestEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	if v, ok := e.vecs[text]; ok {
		return v, nil
	}
	return []float32{0, 0, 1}, nil
}

// fixedExtractor returns preset facts regardless of input.
type fixedExtractor struct{ facts []string }

func (f fixedExtractor) Extract(_ context.Context, _ []hrt.Message) ([]string, error) {
	return f.facts, nil
}

func waitForContent(t *testing.T, st *memory.Store, content string) {
	t.Helper()
	for i := 0; i < 100; i++ { // up to ~2s
		list, err := st.List(context.Background(), "")
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range list {
			if e.Content == content {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("content %q never appeared", content)
}

func countContent(t *testing.T, st *memory.Store, content string) int {
	t.Helper()
	list, err := st.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range list {
		if e.Content == content {
			n++
		}
	}
	return n
}

func TestMemoryIngestE2E(t *testing.T) {
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

	const fact = "the user lives in Singapore"
	emb := ingestEmbedder{vecs: map[string][]float32{
		fact:                        {1, 0, 0},
		"where does the user live?": {1, 0, 0}, // query ~ the fact
	}}
	st, err := memory.NewStore(ctx, db, "alpha", memory.WithEmbedder(emb))
	if err != nil {
		t.Fatal(err)
	}
	ext := fixedExtractor{facts: []string{fact}}
	kg := memory.NewKG(st, 5, 0.5, memory.WithIngest(ext, 0.85, 2, 4))

	thread := []hrt.Message{
		{Role: "user", Content: "I live in Singapore"},
		{Role: "assistant", Content: "Noted!"},
	}

	// 1. Ingest → the fact is saved (async; poll) and carries ingest origin/tag.
	kg.Ingest(ctx, thread)
	waitForContent(t, st, fact)
	auto, _ := st.List(ctx, "auto")
	if len(auto) != 1 || auto[0].Origin != "ingest" {
		t.Fatalf("ingested entry must carry auto tag + ingest origin: %+v", auto)
	}

	// 2. It is now recallable (M3 → M2 loop closes).
	out := kg.Recall(ctx, "where does the user live?")
	if !strings.Contains(out, "Singapore") {
		t.Fatalf("ingested fact should be recallable:\n%s", out)
	}

	// 3. Re-ingesting the same thread saves no duplicate (semantic dedup).
	kg.Ingest(ctx, thread)
	time.Sleep(300 * time.Millisecond) // let the second goroutine run
	if n := countContent(t, st, fact); n != 1 {
		t.Fatalf("dedup failed: want 1 live copy of the fact, got %d", n)
	}

	// 4. Cross-tenant isolation: beta recalls nothing.
	beta, err := memory.NewStore(ctx, db, "beta", memory.WithEmbedder(emb))
	if err != nil {
		t.Fatal(err)
	}
	bkg := memory.NewKG(beta, 5, 0.5)
	if out := bkg.Recall(ctx, "where does the user live?"); out != "" {
		t.Fatalf("beta must recall nothing: %q", out)
	}
}
