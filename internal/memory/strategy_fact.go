package memory

import (
	"context"

	hrt "github.com/sausheong/harness/runtime"
)

// factStrategy is the durable-fact extractor as a Strategy. Mode WriteAccumulate:
// each fact is dedup'd and saved as its own live row (today's behavior).
type factStrategy struct {
	ext     Extractor
	minMsgs int
}

// NewFactStrategy builds the fact-extraction strategy. Exported so the registry
// (and Task 4's summary wiring) can assemble the pipeline.
func NewFactStrategy(ext Extractor, minMsgs int) *factStrategy {
	return &factStrategy{ext: ext, minMsgs: minMsgs}
}

func (f *factStrategy) Kind() string    { return KindFact }
func (f *factStrategy) Mode() WriteMode { return WriteAccumulate }
func (f *factStrategy) Dedup() bool     { return true }
func (f *factStrategy) ShouldRun(thread []hrt.Message) bool {
	return len(thread) >= f.minMsgs
}
func (f *factStrategy) Extract(ctx context.Context, thread []hrt.Message) ([]string, error) {
	return f.ext.Extract(ctx, thread)
}
