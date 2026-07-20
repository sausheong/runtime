package memory

import (
	"context"
	"testing"

	hrt "github.com/sausheong/harness/runtime"
	hmem "github.com/sausheong/harness/tool/memory"
)

// fakeStrategy is a hermetic Strategy: returns preset records and reports whether
// Extract ran. shouldRun gates ShouldRun.
type fakeStrategy struct {
	kind      string
	mode      WriteMode
	records   []string
	ran       bool
	shouldRun bool
	dedup     bool
}

func (f *fakeStrategy) Kind() string    { return f.kind }
func (f *fakeStrategy) Mode() WriteMode { return f.mode }
func (f *fakeStrategy) Dedup() bool     { return f.dedup }
func (f *fakeStrategy) ShouldRun(_ []hrt.Message) bool {
	return f.shouldRun
}
func (f *fakeStrategy) Extract(_ context.Context, _ []hrt.Message) ([]string, error) {
	f.ran = true
	return f.records, nil
}

func TestRunStrategies_AccumulateSavesEachNonDuplicate(t *testing.T) {
	var saved []string
	g := &KG{
		save:       func(_ context.Context, e hmem.Entry, _ string) error { saved = append(saved, e.Content); return nil },
		dedupFloor: 0.85,
		// no embedder ⇒ isDuplicate returns false ⇒ everything saves
	}
	g.strategies = []Strategy{&fakeStrategy{kind: KindFact, mode: WriteAccumulate, shouldRun: true, records: []string{"a", "b"}}}
	g.runStrategies(StrategyContext{SessionID: "s1"}, []hrt.Message{{Role: "user", Content: "hi there friend"}})
	if len(saved) != 2 || saved[0] != "a" || saved[1] != "b" {
		t.Fatalf("accumulate should save 2 in order, got %v", saved)
	}
}

func TestRunStrategies_AccumulateSkipsEmptyAndTrims(t *testing.T) {
	var saved []string
	g := &KG{
		save:       func(_ context.Context, e hmem.Entry, _ string) error { saved = append(saved, e.Content); return nil },
		dedupFloor: 0.85,
	}
	g.strategies = []Strategy{&fakeStrategy{kind: KindFact, mode: WriteAccumulate, shouldRun: true, records: []string{"  spaced  ", "  ", ""}}}
	g.runStrategies(StrategyContext{}, []hrt.Message{{Role: "user", Content: "hello"}})
	if len(saved) != 1 || saved[0] != "spaced" {
		t.Fatalf("empty/whitespace records must be skipped, content trimmed, got %v", saved)
	}
}

func TestRunStrategies_AccumulateCarriesIngestOriginAndTags(t *testing.T) {
	var got hmem.Entry
	g := &KG{
		save: func(_ context.Context, e hmem.Entry, _ string) error { got = e; return nil },
	}
	g.strategies = []Strategy{&fakeStrategy{kind: KindFact, mode: WriteAccumulate, shouldRun: true, records: []string{"fact"}}}
	g.runStrategies(StrategyContext{}, nil)
	if got.Origin != ingestOrigin || len(got.Tags) != 1 || got.Tags[0] != "auto" {
		t.Fatalf("accumulate must carry ingest origin + auto tag: %+v", got)
	}
}

func TestRunStrategies_ShouldRunGatesStrategy(t *testing.T) {
	var saved []string
	g := &KG{
		save: func(_ context.Context, e hmem.Entry, _ string) error { saved = append(saved, e.Content); return nil },
	}
	fs := &fakeStrategy{kind: KindFact, mode: WriteAccumulate, shouldRun: false, records: []string{"nope"}}
	g.strategies = []Strategy{fs}
	g.runStrategies(StrategyContext{SessionID: "s1"}, nil)
	if fs.ran {
		t.Fatal("Extract must not run when ShouldRun is false")
	}
	if len(saved) != 0 {
		t.Fatalf("gated strategy must save nothing, got %v", saved)
	}
}

func TestRunStrategies_SupersedeSkipsWithoutSession(t *testing.T) {
	putCalls := 0
	g := &KG{
		putSummary: func(_ context.Context, _, _ string) error { putCalls++; return nil },
	}
	g.strategies = []Strategy{&fakeStrategy{kind: KindSummary, mode: WriteSupersede, shouldRun: true, records: []string{"digest"}}}
	// Empty SessionID ⇒ supersede branch must skip (no summary without a session).
	g.runStrategies(StrategyContext{}, nil)
	if putCalls != 0 {
		t.Fatalf("supersede must skip when SessionID is empty, putSummary called %d times", putCalls)
	}
}

func TestRunStrategies_SupersedeWritesSummaryAndFiresHook(t *testing.T) {
	var (
		gotSession string
		gotContent string
		hookFired  bool
	)
	g := &KG{
		putSummary: func(_ context.Context, session, content string) error {
			gotSession, gotContent = session, content
			return nil
		},
		onSummaryWrite: func() { hookFired = true },
	}
	g.strategies = []Strategy{&fakeStrategy{kind: KindSummary, mode: WriteSupersede, shouldRun: true, records: []string{"  the digest  ", "ignored"}}}
	g.runStrategies(StrategyContext{SessionID: "s9"}, nil)
	if gotSession != "s9" || gotContent != "the digest" {
		t.Fatalf("supersede must write records[0] trimmed for the session: session=%q content=%q", gotSession, gotContent)
	}
	if !hookFired {
		t.Fatal("onSummaryWrite hook must fire on a successful summary write")
	}
}

func TestRunStrategies_EpisodeSkipsDedupAndStampsKind(t *testing.T) {
	type saved struct {
		content string
		kind    string
	}
	var got []saved
	g := &KG{
		embedder: &kgFakeEmbedder{}, // any non-nil embedder
		search: func(_ context.Context, _ []float32, _ int, _ float64, _ string) ([]hmem.Entry, error) {
			return []hmem.Entry{{Content: "existing"}}, nil // isDuplicate ⇒ true for facts
		},
		save: func(_ context.Context, e hmem.Entry, kind string) error {
			got = append(got, saved{e.Content, kind})
			return nil
		},
		dedupFloor: 0.85,
	}
	// A dedup=false (episode) strategy: BOTH records must save despite the dup hit.
	g.strategies = []Strategy{&fakeStrategy{
		kind: KindEpisode, mode: WriteAccumulate, dedup: false, shouldRun: true,
		records: []string{"deployed staging", "deployed staging"},
	}}
	g.runStrategies(StrategyContext{Actor: "a1"}, []hrt.Message{{Role: "user", Content: "hi there"}})
	if len(got) != 2 {
		t.Fatalf("episode (Dedup=false) must save every record incl. duplicates, got %d: %v", len(got), got)
	}
	if got[0].kind != KindEpisode || got[1].kind != KindEpisode {
		t.Fatalf("episodes must be stamped kind=episode, got %v", got)
	}
}

func TestRunStrategies_FactStillDedups(t *testing.T) {
	var count int
	g := &KG{
		embedder: &kgFakeEmbedder{},
		search: func(_ context.Context, _ []float32, _ int, _ float64, _ string) ([]hmem.Entry, error) {
			return []hmem.Entry{{Content: "existing"}}, nil // isDuplicate ⇒ true
		},
		save:       func(_ context.Context, _ hmem.Entry, _ string) error { count++; return nil },
		dedupFloor: 0.85,
	}
	g.strategies = []Strategy{&fakeStrategy{
		kind: KindFact, mode: WriteAccumulate, dedup: true, shouldRun: true,
		records: []string{"dup fact"},
	}}
	g.runStrategies(StrategyContext{}, []hrt.Message{{Role: "user", Content: "hi there"}})
	if count != 0 {
		t.Fatalf("fact (Dedup=true) must skip a duplicate, saved %d", count)
	}
}

func TestNewFactStrategy_ShouldRunGate(t *testing.T) {
	fs := NewFactStrategy(&fakeExtractor{facts: []string{"x"}}, 2)
	if fs.Kind() != KindFact || fs.Mode() != WriteAccumulate {
		t.Fatalf("fact strategy must be kind=fact accumulate: kind=%q mode=%v", fs.Kind(), fs.Mode())
	}
	if fs.ShouldRun([]hrt.Message{{Role: "user", Content: "hi"}}) {
		t.Fatal("ShouldRun must be false below minMsgs")
	}
	if !fs.ShouldRun(twoMsgThread()) {
		t.Fatal("ShouldRun must be true at/above minMsgs")
	}
	got, err := fs.Extract(context.Background(), twoMsgThread())
	if err != nil || len(got) != 1 || got[0] != "x" {
		t.Fatalf("Extract must delegate to the extractor: %v %v", got, err)
	}
}
