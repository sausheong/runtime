package memory

import (
	"context"
	"strings"
	"testing"

	hrt "github.com/sausheong/harness/runtime"
)

type fakeSummarizer struct {
	out string
	err error
}

func (f fakeSummarizer) Summarize(_ context.Context, _ []hrt.Message) (string, error) {
	return f.out, f.err
}

func TestSummaryStrategy_ExtractReturnsOneDigest(t *testing.T) {
	s := NewSummaryStrategy(fakeSummarizer{out: "the digest"}, 2)
	if s.Kind() != KindSummary || s.Mode() != WriteSupersede {
		t.Fatalf("kind/mode = %s/%d", s.Kind(), s.Mode())
	}
	recs, err := s.Extract(context.Background(), []hrt.Message{{Role: "user", Content: "hello there"}})
	if err != nil || len(recs) != 1 || recs[0] != "the digest" {
		t.Fatalf("Extract = %v, %v", recs, err)
	}
}

func TestSummaryStrategy_EmptyDigestYieldsNoRecord(t *testing.T) {
	s := NewSummaryStrategy(fakeSummarizer{out: "   "}, 2)
	recs, err := s.Extract(context.Background(), []hrt.Message{{Role: "user", Content: "hi"}})
	if err != nil || len(recs) != 0 {
		t.Fatalf("empty digest should yield no records, got %v %v", recs, err)
	}
}

func TestRecallForSession_AppendsSummaryBlock(t *testing.T) {
	g := &KG{
		getSummary: func(_ context.Context, sid string) (string, bool, error) {
			if sid == "s1" {
				return "prior digest", true, nil
			}
			return "", false, nil
		},
	}
	out := g.recallForSession(context.Background(), "any query", "s1")
	if !strings.Contains(out, "prior digest") {
		t.Fatalf("recall should include the session summary, got %q", out)
	}
}
