package memory

import (
	"context"
	"testing"

	hrt "github.com/sausheong/harness/runtime"
)

type fakeEpisodeExtractor struct{ events []string }

func (f *fakeEpisodeExtractor) Extract(_ context.Context, _ []hrt.Message) ([]string, error) {
	return f.events, nil
}

func TestEpisodeStrategy_Shape(t *testing.T) {
	es := NewEpisodeStrategy(&fakeEpisodeExtractor{events: []string{"did x"}}, 2)
	if es.Kind() != KindEpisode {
		t.Fatalf("Kind = %q, want episode", es.Kind())
	}
	if es.Mode() != WriteAccumulate {
		t.Fatalf("Mode = %v, want WriteAccumulate", es.Mode())
	}
	if es.Dedup() {
		t.Fatal("episodes must NOT dedup")
	}
	if es.ShouldRun([]hrt.Message{{Role: "user", Content: "hi"}}) {
		t.Fatal("ShouldRun must be false below minMsgs")
	}
	two := []hrt.Message{{Role: "user", Content: "a"}, {Role: "assistant", Content: "b"}}
	if !es.ShouldRun(two) {
		t.Fatal("ShouldRun must be true at/above minMsgs")
	}
	got, err := es.Extract(context.Background(), two)
	if err != nil || len(got) != 1 || got[0] != "did x" {
		t.Fatalf("Extract must delegate: %v %v", got, err)
	}
}
