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
type searcher func(ctx context.Context, queryVec []float32, k int, floor float64, kind string) ([]hmem.Entry, error)

// saver wraps the one Store method the ingest path needs (func type for testability).
type saver func(ctx context.Context, e hmem.Entry, kind string) error

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

	episodicK int // episodes injected per turn (0 ⇒ episode recall disabled)

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
	getSummary     func(ctx context.Context, sessionID string) (string, bool, error)
	onSummaryWrite func()
	onEpisodeWrite func()
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

// WithSummaryMetric sets a callback invoked after each successful summary write
// (WriteSupersede). Nil is fine; the runner nil-guards it.
func WithSummaryMetric(f func()) KGOption { return func(g *KG) { g.onSummaryWrite = f } }

// WithEpisodeMetric sets a callback invoked after each successful episode write.
// Nil is fine; the pipeline nil-guards it.
func WithEpisodeMetric(f func()) KGOption { return func(g *KG) { g.onEpisodeWrite = f } }

// SetMetrics wires the summary + episode write callbacks after construction.
// The agentd metrics registry is built (in Serve) after the KG, so these hooks
// cannot be set at NewKG time; Serve supplies them via Config.SetMemoryMetrics.
// Either func may be nil (nil-guarded at the call sites).
func (g *KG) SetMetrics(summary, episode func()) {
	g.onSummaryWrite = summary
	g.onEpisodeWrite = episode
}

// WithEpisodicRecall sets how many episodes recall injects per turn (its own
// "Relevant past events:" block). Zero leaves episode recall off.
func WithEpisodicRecall(k int) KGOption { return func(g *KG) { g.episodicK = k } }

// NewKG builds a KnowledgeGraph backed by a tenant-pinned Store. Without any
// KGOption the Ingest path is a no-op (M2 semantic-recall-only behavior).
func NewKG(st *Store, k int, floor float64, opts ...KGOption) *KG {
	g := &KG{
		embedder: st.embedder,
		search:   st.SearchSimilar,
		k:        k,
		floor:    floor,
		save: func(ctx context.Context, e hmem.Entry, kind string) error {
			_, err := st.SaveKind(ctx, e, kind)
			return err
		},
		putSummary: st.PutSessionSummary,
		getSummary: st.GetSessionSummary,
	}
	for _, o := range opts {
		o(g)
	}
	// The summary-only path (WithStrategies without WithIngest) leaves sem nil;
	// a nil-channel send in ingestWith always hits the drop branch, so every turn
	// would be dropped ("ingest at capacity"). Default an inflight bound whenever
	// any ingest work is configured but no option set one. WithIngest, when used,
	// has already sized sem from its maxInflight, so this only fills the gap.
	if g.sem == nil && (len(g.strategies) > 0 || g.extractor != nil) {
		g.sem = make(chan struct{}, defaultMaxInflight)
	}
	return g
}

// defaultMaxInflight bounds concurrent ingest goroutines when no explicit
// maxInflight is configured (e.g. the summary-only KG). Matches WithIngest's
// registry default (RUNTIME_INGEST_MAX_INFLIGHT).
const defaultMaxInflight = 4

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
func (g *KG) ForSession(sessionID, actor string) hrt.KnowledgeGraph {
	return sessionKG{kg: g, sid: sessionID, actor: actor}
}

// sessionKG is the per-session wrapper: one instance per ForSession call binds a
// single session id (and caller actor) to the shared KG's recall/ingest.
type sessionKG struct {
	kg    *KG
	sid   string
	actor string
}

func (w sessionKG) ShouldRecall(query string) bool { return w.kg.ShouldRecall(query) }
func (w sessionKG) Recall(ctx context.Context, query string) string {
	// Wrap the live turn ctx with the actor BEFORE recall so the Store's
	// SearchSimilar (Task 6) scopes the read to (tenant, actor_id=actor).
	return w.kg.recallForSession(WithActor(ctx, w.actor), query, w.sid)
}
func (w sessionKG) Ingest(ctx context.Context, thread []hrt.Message) {
	w.kg.ingestForSession(ctx, thread, w.sid, w.actor)
}

// ingestForSession runs ingest with the session id + actor bound (for
// WriteSupersede and actor-scoped writes), routing through the same gated,
// race-free path as the legacy Ingest — the sctx is threaded as a parameter,
// never stored on the shared KG.
func (g *KG) ingestForSession(_ context.Context, thread []hrt.Message, sessionID, actor string) {
	g.ingestWith(StrategyContext{SessionID: sessionID, Actor: actor}, thread)
}

// recallForSession returns the recall block for the session, composed of up to
// three parts: the fact-similarity block (unchanged from Recall), a "Relevant
// past events" episode block when episodic recall is enabled, and this session's
// rolling summary when present. Best-effort throughout — a summary lookup error
// degrades to fact-only, never breaking a turn.
func (g *KG) recallForSession(ctx context.Context, query, sessionID string) string {
	fact := g.Recall(ctx, query)
	events := g.recallEpisodes(ctx, query)
	var summary string
	if g.getSummary != nil && sessionID != "" {
		if s, ok, err := g.getSummary(ctx, sessionID); err == nil && ok && strings.TrimSpace(s) != "" {
			summary = "Conversation summary so far:\n" + strings.TrimSpace(s) + "\n"
		}
	}
	blocks := make([]string, 0, 3)
	for _, b := range []string{fact, events, summary} {
		if strings.TrimSpace(b) != "" {
			blocks = append(blocks, b)
		}
	}
	return strings.Join(blocks, "\n")
}

// recallEpisodes returns the "Relevant past events:" block for the query, or ""
// when episode recall is off, there is no embedder, nothing relevant, or any
// error (best-effort).
func (g *KG) recallEpisodes(ctx context.Context, query string) string {
	if g.episodicK <= 0 || g.embedder == nil || g.search == nil {
		return ""
	}
	vec, err := g.embedder.Embed(ctx, query)
	if err != nil {
		return ""
	}
	hits, err := g.search(ctx, vec, g.episodicK, g.floor, KindEpisode)
	if err != nil || len(hits) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Relevant past events:\n")
	for _, h := range hits {
		fmt.Fprintf(&b, "- %s\n", h.Content)
	}
	return b.String()
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
	hits, err := g.search(ctx, vec, g.k, g.floor, KindFact)
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
	// Re-attach the actor (carried as data on sctx, since the background ctx has
	// none) so isDuplicate's search + save are actor-scoped.
	ctx := WithActor(context.Background(), sctx.Actor)
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
		if err := g.save(ctx, hmem.Entry{Content: f, Origin: ingestOrigin, Tags: ingestTags}, KindFact); err != nil {
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
	hits, err := g.search(ctx, vec, 1, g.dedupFloor, KindFact)
	if err != nil {
		return false
	}
	return len(hits) > 0
}
