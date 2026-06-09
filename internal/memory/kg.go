package memory

import (
	"context"
	"fmt"
	"strings"

	hrt "github.com/sausheong/harness/runtime"
	hmem "github.com/sausheong/harness/tool/memory"
)

// searcher is the slice of *Store the KG needs (declared as a func type so the
// KG is unit-testable without Postgres).
type searcher func(ctx context.Context, queryVec []float32, k int, floor float64) ([]hmem.Entry, error)

// KG implements harness's runtime.KnowledgeGraph over the tenant-pinned Store:
// Recall embeds the query, finds the nearest live memories, and formats them for
// the prompt. Ingest is a no-op this milestone (memories come from the explicit
// memory tool).
type KG struct {
	embedder Embedder
	search   searcher
	k        int
	floor    float64
}

var _ hrt.KnowledgeGraph = (*KG)(nil)

// NewKG builds a KnowledgeGraph backed by a tenant-pinned Store.
func NewKG(st *Store, k int, floor float64) *KG {
	return &KG{embedder: st.embedder, search: st.SearchSimilar, k: k, floor: floor}
}

// newKGWithSearch is the test seam: inject a fake embedder + search.
func newKGWithSearch(emb Embedder, k int, floor float64, s searcher) *KG {
	return &KG{embedder: emb, search: s, k: k, floor: floor}
}

// ShouldRecall is a cheap gate: skip empty/whitespace/very short inputs where
// recall would not help. Called synchronously at Run start.
func (g *KG) ShouldRecall(query string) bool {
	return len(strings.Fields(query)) >= 3
}

// Recall embeds the query and returns a formatted block of the nearest live
// memories, or "" when there is no embedder, nothing relevant, or any error
// (best-effort: recall never breaks a turn).
func (g *KG) Recall(ctx context.Context, query string) string {
	if g.embedder == nil || g.search == nil {
		return ""
	}
	vec, err := g.embedder.Embed(ctx, query)
	if err != nil {
		return ""
	}
	hits, err := g.search(ctx, vec, g.k, g.floor)
	if err != nil || len(hits) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Relevant memories:\n")
	for _, h := range hits {
		fmt.Fprintf(&b, "- %s\n", h.Content)
	}
	return b.String()
}

// Ingest is a no-op in M2 (auto-extraction is a later milestone).
func (g *KG) Ingest(_ context.Context, _ []hrt.Message) {}
