package memory

import (
	"context"

	hrt "github.com/sausheong/harness/runtime"
)

// episodeStrategy extracts timestamped event records ("what happened") as a
// Strategy. Mode WriteAccumulate with Dedup()==false: each event is a distinct
// occurrence and always saved (kind='episode').
type episodeStrategy struct {
	ext     EpisodeExtractor
	minMsgs int
}

// NewEpisodeStrategy builds the episodic-extraction strategy.
func NewEpisodeStrategy(ext EpisodeExtractor, minMsgs int) *episodeStrategy {
	return &episodeStrategy{ext: ext, minMsgs: minMsgs}
}

func (e *episodeStrategy) Kind() string    { return KindEpisode }
func (e *episodeStrategy) Mode() WriteMode { return WriteAccumulate }
func (e *episodeStrategy) Dedup() bool     { return false }
func (e *episodeStrategy) ShouldRun(thread []hrt.Message) bool {
	return len(thread) >= e.minMsgs
}
func (e *episodeStrategy) Extract(ctx context.Context, thread []hrt.Message) ([]string, error) {
	return e.ext.Extract(ctx, thread)
}

var _ Strategy = (*episodeStrategy)(nil)
