package memory

import (
	"context"
	"log/slog"
	"strings"

	hrt "github.com/sausheong/harness/runtime"
	hmem "github.com/sausheong/harness/tool/memory"
)

// WriteMode selects how the pipeline persists a strategy's extracted records.
type WriteMode int

const (
	// WriteAccumulate saves each record as its own live row, skipping any that
	// duplicate existing memory (fact behavior).
	WriteAccumulate WriteMode = iota
	// WriteSupersede keeps exactly one live row per session, replacing the prior
	// (summary behavior). Requires a non-empty StrategyContext.SessionID.
	WriteSupersede
)

// StrategyContext carries per-turn dimensions beyond the thread. SessionID is
// captured before the ingest goroutine detaches (threaded through as a parameter,
// never stored on the process-shared KG) so WriteSupersede/summary recall can key
// on it.
type StrategyContext struct {
	SessionID string
}

// Strategy is one memory-extraction kind (fact, summary, …). Extract turns a
// finished thread into zero or more records; the pipeline persists them per Mode.
type Strategy interface {
	Kind() string
	Mode() WriteMode
	ShouldRun(thread []hrt.Message) bool
	Extract(ctx context.Context, thread []hrt.Message) ([]string, error)
}

// runStrategies runs every configured strategy over the thread and persists its
// records per write mode. Best-effort throughout: any strategy failure is logged
// and skipped, never surfaced (ingest must never break a turn). Uses a fresh
// context.Background (the caller detaches from the request ctx before invoking).
func (g *KG) runStrategies(sctx StrategyContext, thread []hrt.Message) {
	ctx := context.Background()
	for _, st := range g.strategies {
		if !st.ShouldRun(thread) {
			continue
		}
		records, err := st.Extract(ctx, thread)
		if err != nil {
			slog.Warn("memory: strategy extract failed", "kind", st.Kind(), "err", err)
			continue
		}
		switch st.Mode() {
		case WriteAccumulate:
			for _, r := range records {
				r = strings.TrimSpace(r)
				if r == "" || g.isDuplicate(ctx, r) {
					continue
				}
				if err := g.save(ctx, hmem.Entry{Content: r, Origin: ingestOrigin, Tags: ingestTags}); err != nil {
					slog.Warn("memory: strategy save failed", "kind", st.Kind(), "err", err)
				}
			}
		case WriteSupersede:
			if len(records) == 0 {
				continue
			}
			content := strings.TrimSpace(records[0])
			if content == "" || sctx.SessionID == "" || g.putSummary == nil {
				continue
			}
			if err := g.putSummary(ctx, sctx.SessionID, content); err != nil {
				slog.Warn("memory: summary write failed", "session", sctx.SessionID, "err", err)
				continue
			}
			if g.onSummaryWrite != nil {
				g.onSummaryWrite()
			}
		}
	}
}
