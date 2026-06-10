package gateway

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/sausheong/harness/tool"
)

// fakeEmbedder returns deterministic vectors per registered text and counts
// calls. Texts in failFor error; unknown texts get an orthogonal default.
type fakeEmbedder struct {
	vecs    map[string][]float32
	calls   atomic.Int64
	failFor map[string]bool
}

func (f *fakeEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	f.calls.Add(1)
	if f.failFor[text] {
		return nil, errors.New("scripted embed failure")
	}
	if v, ok := f.vecs[text]; ok {
		return v, nil
	}
	return []float32{0, 0, 1}, nil
}
func (f *fakeEmbedder) Dim() int { return 3 }

// tt mirrors index.go's toolText so tests register vectors under the right key.
func tt(name, desc string) string { return name + "\n" + desc }

func indexTools() []tool.Tool {
	return []tool.Tool{
		fakeTool{name: "fs__read", out: "x"},
		fakeTool{name: "fs__write", out: "x"},
		fakeTool{name: "web__fetch", out: "x"},
	}
}

func TestIndexSearchRanksAndFloors(t *testing.T) {
	emb := &fakeEmbedder{vecs: map[string][]float32{
		"read a file":                       {1, 0, 0},
		tt("fs__read", "fake fs__read"):     {0.9, 0.1, 0},
		tt("fs__write", "fake fs__write"):   {0.5, 0.5, 0},
		tt("web__fetch", "fake web__fetch"): {0, 1, 0},
	}}
	idx := NewIndex(emb, 0.3, 5)
	ms, err := idx.Search(context.Background(), indexTools(), "read a file", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 {
		t.Fatalf("want 2 above floor 0.3, got %d: %+v", len(ms), ms)
	}
	if ms[0].Name != "fs__read" || ms[1].Name != "fs__write" {
		t.Fatalf("wrong order: %+v", ms)
	}
	if ms[0].Score <= ms[1].Score {
		t.Fatalf("scores not descending: %+v", ms)
	}
	if len(ms[0].InputSchema) == 0 {
		t.Fatal("match missing input schema")
	}
}

func TestIndexKAndCap(t *testing.T) {
	emb := &fakeEmbedder{vecs: map[string][]float32{
		"q":                                 {1, 0, 0},
		tt("fs__read", "fake fs__read"):     {1, 0, 0},
		tt("fs__write", "fake fs__write"):   {0.99, 0.01, 0},
		tt("web__fetch", "fake web__fetch"): {0.98, 0.02, 0},
	}}
	idx := NewIndex(emb, 0.1, 5)
	ms, err := idx.Search(context.Background(), indexTools(), "q", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 {
		t.Fatalf("k=2 not respected: got %d", len(ms))
	}
	ms, err = idx.Search(context.Background(), indexTools(), "q", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 3 {
		t.Fatalf("k=0 should use default(5)→all 3, got %d", len(ms))
	}
	ms, _ = idx.Search(context.Background(), indexTools(), "q", 1000)
	if len(ms) != 3 {
		t.Fatalf("clamped k should still return all 3, got %d", len(ms))
	}
}

func TestIndexVectorCacheReuse(t *testing.T) {
	emb := &fakeEmbedder{vecs: map[string][]float32{"q": {1, 0, 0}}}
	idx := NewIndex(emb, 0.0, 5)
	ts := indexTools()
	if _, err := idx.Search(context.Background(), ts, "q", 5); err != nil {
		t.Fatal(err)
	}
	first := emb.calls.Load() // 3 tools + 1 query
	if first != 4 {
		t.Fatalf("want 4 embed calls on first search, got %d", first)
	}
	if _, err := idx.Search(context.Background(), ts, "q", 5); err != nil {
		t.Fatal(err)
	}
	if got := emb.calls.Load(); got != first+1 {
		t.Fatalf("tool vectors not cached: %d calls after second search", got)
	}
}

func TestIndexQueryEmbedFailure(t *testing.T) {
	emb := &fakeEmbedder{failFor: map[string]bool{"q": true}}
	idx := NewIndex(emb, 0.0, 5)
	if _, err := idx.Search(context.Background(), indexTools(), "q", 5); err == nil {
		t.Fatal("want error on query embed failure")
	}
}

func TestIndexToolEmbedFailureDegrades(t *testing.T) {
	emb := &fakeEmbedder{
		vecs: map[string][]float32{
			"q":                                 {1, 0, 0},
			tt("fs__read", "fake fs__read"):     {1, 0, 0},
			tt("web__fetch", "fake web__fetch"): {0.9, 0.1, 0},
		},
		failFor: map[string]bool{tt("fs__write", "fake fs__write"): true},
	}
	idx := NewIndex(emb, 0.0, 5)
	ms, err := idx.Search(context.Background(), indexTools(), "q", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 {
		t.Fatalf("failed-embed tool should be skipped, got %d matches", len(ms))
	}
	for _, m := range ms {
		if m.Name == "fs__write" {
			t.Fatal("failed-embed tool surfaced in results")
		}
	}
	delete(emb.failFor, tt("fs__write", "fake fs__write"))
	emb.vecs[tt("fs__write", "fake fs__write")] = []float32{0.8, 0.2, 0}
	ms, _ = idx.Search(context.Background(), indexTools(), "q", 5)
	if len(ms) != 3 {
		t.Fatalf("failed tool not retried on next search: %d", len(ms))
	}
}
