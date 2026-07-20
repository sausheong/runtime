package memory

import (
	"context"
	"strings"

	hrt "github.com/sausheong/harness/runtime"
)

// summaryStrategy is the rolling per-session digest as a Strategy. Mode
// WriteSupersede: exactly one live summary row per session, replaced each turn.
// Embedder-independent — it needs only a Summarizer and the session-keyed store.
type summaryStrategy struct {
	sum     Summarizer
	minMsgs int
}

// NewSummaryStrategy builds the summary strategy. Exported so the registry can
// assemble the pipeline.
func NewSummaryStrategy(sum Summarizer, minMsgs int) *summaryStrategy {
	return &summaryStrategy{sum: sum, minMsgs: minMsgs}
}

func (s *summaryStrategy) Kind() string    { return KindSummary }
func (s *summaryStrategy) Mode() WriteMode { return WriteSupersede }
func (s *summaryStrategy) Dedup() bool     { return false } // unused (WriteSupersede)
func (s *summaryStrategy) ShouldRun(thread []hrt.Message) bool {
	return len(thread) >= s.minMsgs
}

// Extract summarizes the thread into a single digest. An empty digest yields no
// record (fail-open: no write, turn unaffected); a summarizer error is returned
// so the pipeline logs and skips it.
func (s *summaryStrategy) Extract(ctx context.Context, thread []hrt.Message) ([]string, error) {
	digest, err := s.sum.Summarize(ctx, thread)
	if err != nil {
		return nil, err
	}
	digest = strings.TrimSpace(digest)
	if digest == "" {
		return nil, nil
	}
	return []string{digest}, nil
}
