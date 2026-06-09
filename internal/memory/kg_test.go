package memory

import (
	"context"
	"fmt"
	"strings"
	"testing"

	hmem "github.com/sausheong/harness/tool/memory"
)

// kgFakeEmbedder is a hermetic fake (store_test.go's fixedEmbedder is
// integration-tagged and not visible here).
type kgFakeEmbedder struct {
	vecs map[string][]float32
	fail bool
}

func (f *kgFakeEmbedder) Dim() int { return 3 }
func (f *kgFakeEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	if f.fail {
		return nil, fmt.Errorf("embed failed")
	}
	if v, ok := f.vecs[text]; ok {
		return v, nil
	}
	return []float32{0, 0, 1}, nil
}

func TestKG_ShouldRecall(t *testing.T) {
	k := &KG{}
	if k.ShouldRecall("") || k.ShouldRecall("  ") || k.ShouldRecall("ok") {
		t.Fatal("trivial inputs must not trigger recall")
	}
	if !k.ShouldRecall("what did we decide about the database schema?") {
		t.Fatal("a real question should trigger recall")
	}
}

func TestKG_RecallFormatsHits(t *testing.T) {
	emb := &kgFakeEmbedder{vecs: map[string][]float32{"query": {1, 0, 0}}}
	searched := false
	k := newKGWithSearch(emb, 5, 0.5, func(_ context.Context, vec []float32, k int, floor float64) ([]hmem.Entry, error) {
		searched = true
		if vec[0] != 1 {
			t.Fatalf("query not embedded: %v", vec)
		}
		return []hmem.Entry{{Content: "cats are great"}, {Content: "felines rule"}}, nil
	})
	out := k.Recall(context.Background(), "query")
	if !searched {
		t.Fatal("Recall must embed + search")
	}
	if !strings.Contains(out, "cats are great") || !strings.Contains(out, "felines rule") {
		t.Fatalf("recall block missing memories: %q", out)
	}
}

func TestKG_RecallEmptyWhenNoHits(t *testing.T) {
	emb := &kgFakeEmbedder{vecs: map[string][]float32{"q": {1, 0, 0}}}
	k := newKGWithSearch(emb, 5, 0.5, func(_ context.Context, _ []float32, _ int, _ float64) ([]hmem.Entry, error) {
		return nil, nil
	})
	if out := k.Recall(context.Background(), "q"); out != "" {
		t.Fatalf("no hits ⇒ empty recall, got %q", out)
	}
}

func TestKG_RecallEmptyOnEmbedError(t *testing.T) {
	emb := &kgFakeEmbedder{fail: true}
	k := newKGWithSearch(emb, 5, 0.5, func(_ context.Context, _ []float32, _ int, _ float64) ([]hmem.Entry, error) {
		t.Fatal("search must not be called when embed fails")
		return nil, nil
	})
	if out := k.Recall(context.Background(), "q"); out != "" {
		t.Fatalf("embed error ⇒ empty recall, got %q", out)
	}
}

func TestKG_IngestIsNoop(t *testing.T) {
	k := &KG{}
	k.Ingest(context.Background(), nil) // must not panic
}
