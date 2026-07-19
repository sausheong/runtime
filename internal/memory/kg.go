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

// saver wraps the one Store method the ingest path needs (func type for testability).
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

	// Strategy pipeline (P2.2). When non-empty it supersedes the single hardwired
	// extractor path. putSummary/onSummaryWrite are the WriteSupersede sink+metric,
	// wired in Task 4's NewKG change; nil is fine here (the runner guards them).
	strategies     []Strategy
	putSummary     func(ctx context.Context, sessionID, content string) error
	onSummaryWrite func()
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

// WithStrategies configures the strategy pipeline. Each strategy runs at
// end-of-turn; records persist per the strategy's WriteMode. When set, it replaces
// the single hardwired fact path in runIngest.
func WithStrategies(ss ...Strategy) KGOption {
	return func(g *KG) { g.strategies = ss }
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

// ForSession returns a KnowledgeGraph view bound to one session id, so summary
// write (WriteSupersede) and summary recall key on the calling session. The
// underlying KG is process-shared; the wrapper carries only the id, passed as a
// parameter through the ingest goroutine — never stored on the shared KG, so
// concurrent sessions cannot clobber each other. The harness KnowledgeGraph
// interface is unchanged; this adapts to it.
func (g *KG) ForSession(sessionID string) hrt.KnowledgeGraph {
	return sessionKG{kg: g, sid: sessionID}
}

// sessionKG is the per-session wrapper: one instance per ForSession call binds a
// single session id to the shared KG's recall/ingest.
type sessionKG struct {
	kg  *KG
	sid string
}

func (w sessionKG) ShouldRecall(query string) bool { return w.kg.ShouldRecall(query) }
func (w sessionKG) Recall(ctx context.Context, query string) string {
	return w.kg.recallForSession(ctx, query, w.sid)
}
func (w sessionKG) Ingest(ctx context.Context, thread []hrt.Message) {
	w.kg.ingestForSession(ctx, thread, w.sid)
}

// ingestForSession runs ingest with the session id bound (for WriteSupersede),
// routing through the same gated, race-free path as the legacy Ingest — the sctx
// is threaded as a parameter, never stored on the shared KG.
func (g *KG) ingestForSession(_ context.Context, thread []hrt.Message, sessionID string) {
	g.ingestWith(StrategyContext{SessionID: sessionID}, thread)
}

// recallForSession returns the recall block for the session. In this task it is
// exactly today's fact-similarity recall; the summary block is added in Task 4
// (the sessionID param is kept now so Task 4 only edits the body).
func (g *KG) recallForSession(ctx context.Context, query, _ string) string {
	return g.Recall(ctx, query)
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
	g.ingestWith(StrategyContext{}, thread)
}

// ingestWith is the session-aware ingest entrypoint: it applies the enable gate,
// the growth gate, and the inflight cap, then detaches the background body with
// sctx threaded through as a parameter (never stored on the process-shared KG, so
// concurrent sessions cannot clobber each other's SessionID). The public Ingest is
// a thin wrapper passing an empty sctx (legacy/no-session path); Task 3 adds a
// session-aware caller that passes a real sctx.
func (g *KG) ingestWith(sctx StrategyContext, thread []hrt.Message) {
	if (g.extractor == nil && len(g.strategies) == 0) || g.save == nil {
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
	go g.runIngest(sctx, thread)
}

// runIngest is the background body: it holds one sem slot (releasing it and
// recovering any panic on exit) and delegates to the strategy pipeline when
// configured, else the legacy single-extractor path (extract → per-fact dedup →
// save).
func (g *KG) runIngest(sctx StrategyContext, thread []hrt.Message) {
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("memory: ingest goroutine panic recovered", "panic", r)
		}
		<-g.sem
		if g.ingestDone != nil {
			g.ingestDone()
		}
	}()
	if len(g.strategies) > 0 {
		g.runStrategies(sctx, thread)
		return
	}
	// Legacy single-extractor path (unchanged).
	// Fresh context: the request ctx is typically cancelled by the time the
	// harness fires Ingest in its end-of-Run defer. The extractor's own client
	// timeout (30s) bounds how long a stuck extraction can hold its sem slot.
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
