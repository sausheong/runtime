//go:build integration

package test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	hmem "github.com/sausheong/harness/tool/memory"

	"github.com/sausheong/runtime/internal/memory"
)

// e2eEmbedder maps known content to deterministic vectors (dim 3).
type e2eEmbedder struct{ vecs map[string][]float32 }

func (e e2eEmbedder) Dim() int { return 3 }
func (e e2eEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	if v, ok := e.vecs[text]; ok {
		return v, nil
	}
	return []float32{0, 0, 1}, nil
}

func TestMemoryRecallE2E(t *testing.T) {
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

	emb := e2eEmbedder{vecs: map[string][]float32{
		"the db schema uses an append-only event log": {1, 0, 0},
		"the user prefers dark mode":                  {0, 1, 0},
		"tell me about the database design":           {1, 0, 0}, // query ~ schema memory
	}}
	st, err := memory.NewStore(ctx, db, "alpha", memory.WithEmbedder(emb))
	if err != nil {
		t.Fatal(err)
	}
	st.Save(ctx, hmem.Entry{Content: "the db schema uses an append-only event log"})
	st.Save(ctx, hmem.Entry{Content: "the user prefers dark mode"})

	kg := memory.NewKG(st, 5, 0.5)
	if !kg.ShouldRecall("tell me about the database design") {
		t.Fatal("query should trigger recall")
	}
	out := kg.Recall(ctx, "tell me about the database design")
	if !strings.Contains(out, "append-only event log") {
		t.Fatalf("recall should surface the schema memory:\n%s", out)
	}
	if strings.Contains(out, "dark mode") {
		t.Fatalf("recall should NOT surface the unrelated memory:\n%s", out)
	}

	// cross-tenant: beta recalls nothing
	beta, _ := memory.NewStore(ctx, db, "beta", memory.WithEmbedder(emb))
	bkg := memory.NewKG(beta, 5, 0.5)
	if out := bkg.Recall(ctx, "tell me about the database design"); out != "" {
		t.Fatalf("beta must recall nothing: %q", out)
	}
}
