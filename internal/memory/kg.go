package memory

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	hrt "github.com/sausheong/harness/runtime"
	hmem "github.com/sausheong/harness/tool/memory"
)

// searcher is the slice of *Store the KG needs (declared as a func type so the
// KG is unit-testable without Postgres).
type searcher func(ctx context.Context, queryVec []float32, k int, floor float64) ([]hmem.Entry, error)

// saver is the slice of *Store the ingest path needs (func type for testability).
type saver func(ctx context.Context, e hmem.Entry) error

// ingestOrigin / ingestTags mark auto-captured memories so they are
// distinguishable from tool-saved ones (List/audits, a future GC pass).
const ingestOrigin = "ingest"

var ingestTags = []string{"auto"}

// KG implements harness's runtime.KnowledgeGraph over the tenant-pinned Store.
// Recall embeds the query, finds the nearest live memories, and formats them for
// the prompt. Ingest (optional, enabled via WithIngest) extracts durable facts
// from a finished turn and saves the new ones in a background goroutine.
type KG struct {
	embedder Embedder
	search   searcher
	k        int
	floor    float64

	// Ingest path. Nil extractor ⇒ Ingest is a no-op (M2 behavior).
	extractor  Extractor
	save       saver
	dedupFloor float64
	minMsgs    int
	sem        chan struct{}
	ingestDone func() // test hook; nil in production
}

var _ hrt.KnowledgeGraph = (*KG)(nil)

// KGOption configures the optional ingest path on a KG.
type KGOption func(*KG)

// WithIngest enables auto-ingestion: after each chat turn, extract durable facts,
// dedup them against existing memory (skip when an entry is >= dedupFloor
// similar), and save the new ones. minMsgs is the growth gate (threads shorter
// than this are skipped); maxInflight bounds concurrent extractions (excess turns
// are dropped, not queued).
func WithIngest(ext Extractor, dedupFloor float64, minMsgs, maxInflight int) KGOption {
	return func(g *KG) {
		if maxInflight < 1 {
			maxInflight = 1
		}
		g.extractor = ext
		g.dedupFloor = dedupFloor
		g.minMsgs = minMsgs
		g.sem = make(chan struct{}, maxInflight)
	}
}

// NewKG builds a KnowledgeGraph backed by a tenant-pinned Store. Without any
// KGOption the Ingest path is a no-op (M2 semantic-recall-only behavior).
func NewKG(st *Store, k int, floor float64, opts ...KGOption) *KG {
	g := &KG{
		embedder: st.embedder,
		search:   st.SearchSimilar,
		k:        k,
		floor:    floor,
		save: func(ctx context.Context, e hmem.Entry) error {
			_, err := st.Save(ctx, e)
			return err
		},
	}
	for _, o := range opts {
		o(g)
	}
	return g
}

// newKGWithSearch is the recall test seam: inject a fake embedder + search.
func newKGWithSearch(emb Embedder, k int, floor float64, s searcher) *KG {
	return &KG{embedder: emb, search: s, k: k, floor: floor}
}

// newKGWithIngest is the ingest test seam: inject fakes for every dependency so
// Ingest is unit-testable without Postgres or a live proxy. done (if non-nil) is
// called when an Ingest goroutine finishes or a turn is dropped.
func newKGWithIngest(emb Embedder, ext Extractor, s searcher, sv saver, dedupFloor float64, minMsgs, maxInflight int, done func()) *KG {
	if maxInflight < 1 {
		maxInflight = 1
	}
	return &KG{
		embedder:   emb,
		search:     s,
		save:       sv,
		extractor:  ext,
		dedupFloor: dedupFloor,
		minMsgs:    minMsgs,
		sem:        make(chan struct{}, maxInflight),
		ingestDone: done,
	}
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

// Ingest extracts durable facts from a finished chat turn and saves the new ones
// in a background goroutine, so the turn never waits. A no-op when the ingest
// path is unconfigured (M2 behavior). Best-effort throughout: every failure
// degrades silently — ingestion never affects a turn. The growth gate and the
// inflight cap bound cost; over capacity, the turn's ingest is dropped.
func (g *KG) Ingest(_ context.Context, thread []hrt.Message) {
	if g.extractor == nil || g.save == nil {
		return
	}
	if len(thread) < g.minMsgs {
		return
	}
	select {
	case g.sem <- struct{}{}:
	default:
		slog.Warn("memory: ingest at capacity, dropping turn")
		if g.ingestDone != nil {
			g.ingestDone()
		}
		return
	}
	go g.runIngest(thread)
}

// runIngest is the background body: extract → per-fact dedup → save. Holds one
// sem slot; releases it (and recovers any panic) on exit.
func (g *KG) runIngest(thread []hrt.Message) {
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("memory: ingest goroutine panic recovered", "panic", r)
		}
		<-g.sem
		if g.ingestDone != nil {
			g.ingestDone()
		}
	}()
	// Fresh context: the request ctx is typically cancelled by the time the
	// harness fires Ingest in its end-of-Run defer.
	ctx := context.Background()
	facts, err := g.extractor.Extract(ctx, thread)
	if err != nil {
		slog.Warn("memory: ingest extract failed", "err", err)
		return
	}
	for _, f := range facts {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if g.isDuplicate(ctx, f) {
			continue
		}
		if err := g.save(ctx, hmem.Entry{Content: f, Origin: ingestOrigin, Tags: ingestTags}); err != nil {
			slog.Warn("memory: ingest save failed", "err", err)
		}
	}
}

// isDuplicate reports whether a memory at least dedupFloor-similar to fact
// already exists. On any embed/search failure it returns false (save anyway —
// degrade rather than silently drop a fact).
func (g *KG) isDuplicate(ctx context.Context, fact string) bool {
	if g.embedder == nil || g.search == nil {
		return false
	}
	vec, err := g.embedder.Embed(ctx, fact)
	if err != nil {
		return false
	}
	hits, err := g.search(ctx, vec, 1, g.dedupFloor)
	if err != nil {
		return false
	}
	return len(hits) > 0
}
