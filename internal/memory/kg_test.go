package memory

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	hrt "github.com/sausheong/harness/runtime"
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
	k := newKGWithSearch(emb, 5, 0.5, func(_ context.Context, vec []float32, k int, floor float64, _ string) ([]hmem.Entry, error) {
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
	k := newKGWithSearch(emb, 5, 0.5, func(_ context.Context, _ []float32, _ int, _ float64, _ string) ([]hmem.Entry, error) {
		return nil, nil
	})
	if out := k.Recall(context.Background(), "q"); out != "" {
		t.Fatalf("no hits ⇒ empty recall, got %q", out)
	}
}

func TestKG_RecallEmptyOnEmbedError(t *testing.T) {
	emb := &kgFakeEmbedder{fail: true}
	k := newKGWithSearch(emb, 5, 0.5, func(_ context.Context, _ []float32, _ int, _ float64, _ string) ([]hmem.Entry, error) {
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

// fakeExtractor returns preset facts (or an error) regardless of input.
type fakeExtractor struct {
	facts []string
	err   error
}

func (f *fakeExtractor) Extract(_ context.Context, _ []hrt.Message) ([]string, error) {
	return f.facts, f.err
}

type blockingExtractor struct {
	started chan struct{}
	release <-chan struct{}
	useCtx  bool
}

func (b *blockingExtractor) Extract(ctx context.Context, _ []hrt.Message) ([]string, error) {
	close(b.started)
	if b.useCtx {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	<-b.release
	return []string{"done"}, nil
}

func TestKGCloseDrainsCooperativeWorker(t *testing.T) {
	release := make(chan struct{})
	ext := &blockingExtractor{started: make(chan struct{}), release: release}
	k := newKGWithIngest(&kgFakeEmbedder{}, ext, func(context.Context, []float32, int, float64, string) ([]hmem.Entry, error) {
		return nil, nil
	}, func(context.Context, hmem.Entry, string) error { return nil }, 0.85, 2, 1, nil)
	k.Ingest(context.Background(), twoMsgThread())
	<-ext.started
	closed := make(chan error, 1)
	go func() { closed <- k.Close(time.Second) }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned before accepted worker drained: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}

func TestKGCloseCancelsBlockedWorkerAndIsBounded(t *testing.T) {
	ext := &blockingExtractor{started: make(chan struct{}), useCtx: true}
	k := newKGWithIngest(&kgFakeEmbedder{}, ext, func(context.Context, []float32, int, float64, string) ([]hmem.Entry, error) {
		return nil, nil
	}, func(context.Context, hmem.Entry, string) error { return nil }, 0.85, 2, 1, nil)
	k.Ingest(context.Background(), twoMsgThread())
	<-ext.started
	start := time.Now()
	if err := k.Close(10 * time.Millisecond); err == nil {
		t.Fatal("timed-out graceful drain was not reported")
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("cancellable worker shutdown took %s", elapsed)
	}
}

func TestKGCloseDoesNotWaitForeverForNonCooperativeWorker(t *testing.T) {
	release := make(chan struct{})
	ext := &blockingExtractor{started: make(chan struct{}), release: release}
	k := newKGWithIngest(&kgFakeEmbedder{}, ext, func(context.Context, []float32, int, float64, string) ([]hmem.Entry, error) {
		return nil, nil
	}, func(context.Context, hmem.Entry, string) error { return nil }, 0.85, 2, 1, nil)
	k.Ingest(context.Background(), twoMsgThread())
	<-ext.started
	start := time.Now()
	if err := k.Close(10 * time.Millisecond); err == nil {
		t.Fatal("non-cooperative worker shutdown was not reported")
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("non-cooperative shutdown took %s", elapsed)
	}
	close(release)
}

func TestKGRejectsIngestAfterClose(t *testing.T) {
	ext := &fakeExtractor{facts: []string{"must not run"}}
	done := make(chan struct{}, 1)
	k := newKGWithIngest(&kgFakeEmbedder{}, ext, func(context.Context, []float32, int, float64, string) ([]hmem.Entry, error) {
		return nil, nil
	}, func(context.Context, hmem.Entry, string) error {
		t.Fatal("save called after Close")
		return nil
	}, 0.85, 2, 1, func() { done <- struct{}{} })
	if err := k.Close(time.Second); err != nil {
		t.Fatal(err)
	}
	k.Ingest(context.Background(), twoMsgThread())
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("closed-ingest rejection was not observable")
	}
}

func TestKGConcurrentCloseAndIngestIsRaceFree(t *testing.T) {
	const attempts = 64
	k := newKGWithIngest(
		&kgFakeEmbedder{},
		&fakeExtractor{facts: []string{"accepted fact"}},
		func(context.Context, []float32, int, float64, string) ([]hmem.Entry, error) {
			return nil, nil
		},
		func(context.Context, hmem.Entry, string) error { return nil },
		0.85,
		2,
		attempts,
		nil,
	)

	start := make(chan struct{})
	var callers sync.WaitGroup
	callers.Add(attempts + 2)
	for range attempts {
		go func() {
			defer callers.Done()
			<-start
			k.Ingest(context.Background(), twoMsgThread())
		}()
	}
	for range 2 {
		go func() {
			defer callers.Done()
			<-start
			if err := k.Close(time.Second); err != nil {
				t.Errorf("concurrent Close: %v", err)
			}
		}()
	}
	close(start)
	callers.Wait()

	if err := k.Close(time.Second); err != nil {
		t.Fatalf("idempotent final Close: %v", err)
	}
}

// recordingSaver records saved entries; optionally fails on the first call.
type recordingSaver struct {
	mu        sync.Mutex
	saved     []hmem.Entry
	failFirst bool
	calls     int
}

func (r *recordingSaver) save(_ context.Context, e hmem.Entry, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.failFirst && r.calls == 1 {
		return fmt.Errorf("save boom")
	}
	r.saved = append(r.saved, e)
	return nil
}

func (r *recordingSaver) snapshot() []hmem.Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]hmem.Entry(nil), r.saved...)
}

func twoMsgThread() []hrt.Message {
	return []hrt.Message{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "yo"}}
}

func TestKG_IngestGateSkipsShortThread(t *testing.T) {
	ext := &fakeExtractor{facts: []string{"should not run"}}
	saver := &recordingSaver{}
	done := make(chan struct{}, 1)
	emb := &kgFakeEmbedder{}
	search := func(_ context.Context, _ []float32, _ int, _ float64, _ string) ([]hmem.Entry, error) {
		return nil, nil
	}
	k := newKGWithIngest(emb, ext, search, saver.save, 0.85, 2, 4, func() { done <- struct{}{} })

	k.Ingest(context.Background(), []hrt.Message{{Role: "user", Content: "hi"}}) // len 1 < minMsgs 2
	select {
	case <-done:
		t.Fatal("ingest must not run for a sub-threshold thread")
	default:
	}
	if len(saver.snapshot()) != 0 {
		t.Fatal("nothing should be saved")
	}
}

func TestKG_IngestSavesNewFacts(t *testing.T) {
	ext := &fakeExtractor{facts: []string{"alpha fact", "beta fact"}}
	saver := &recordingSaver{}
	done := make(chan struct{}, 1)
	emb := &kgFakeEmbedder{}
	// search returns no hits → nothing is a duplicate.
	search := func(_ context.Context, _ []float32, _ int, _ float64, _ string) ([]hmem.Entry, error) {
		return nil, nil
	}
	k := newKGWithIngest(emb, ext, search, saver.save, 0.85, 2, 4, func() { done <- struct{}{} })

	k.Ingest(context.Background(), twoMsgThread())
	<-done
	got := saver.snapshot()
	if len(got) != 2 || got[0].Content != "alpha fact" || got[1].Content != "beta fact" {
		t.Fatalf("want both facts saved in order: %+v", got)
	}
	if got[0].Origin != "ingest" || len(got[0].Tags) != 1 || got[0].Tags[0] != "auto" {
		t.Fatalf("saved entry must carry ingest origin + auto tag: %+v", got[0])
	}
}

func TestKG_IngestSkipsDuplicates(t *testing.T) {
	ext := &fakeExtractor{facts: []string{"dup fact", "new fact"}}
	saver := &recordingSaver{}
	done := make(chan struct{}, 1)
	emb := &kgFakeEmbedder{}
	emb.vecs = map[string][]float32{"dup fact": {1, 0, 0}, "new fact": {0, 1, 0}}
	// "dup fact" → a hit (duplicate); "new fact" → no hit.
	search := func(_ context.Context, vec []float32, _ int, _ float64, _ string) ([]hmem.Entry, error) {
		if vec[0] == 1 { // dup fact's vector
			return []hmem.Entry{{Content: "already stored"}}, nil
		}
		return nil, nil
	}
	k := newKGWithIngest(emb, ext, search, saver.save, 0.85, 2, 4, func() { done <- struct{}{} })

	k.Ingest(context.Background(), twoMsgThread())
	<-done
	got := saver.snapshot()
	if len(got) != 1 || got[0].Content != "new fact" {
		t.Fatalf("duplicate must be skipped, only new fact saved: %+v", got)
	}
}

func TestKG_IngestExtractErrorDegrades(t *testing.T) {
	ext := &fakeExtractor{err: fmt.Errorf("extract boom")}
	saver := &recordingSaver{}
	done := make(chan struct{}, 1)
	search := func(_ context.Context, _ []float32, _ int, _ float64, _ string) ([]hmem.Entry, error) {
		return nil, nil
	}
	k := newKGWithIngest(&kgFakeEmbedder{}, ext, search, saver.save, 0.85, 2, 4, func() { done <- struct{}{} })

	k.Ingest(context.Background(), twoMsgThread())
	<-done
	if len(saver.snapshot()) != 0 {
		t.Fatal("extractor error ⇒ nothing saved")
	}
}

func TestKG_IngestSaveErrorContinues(t *testing.T) {
	ext := &fakeExtractor{facts: []string{"first", "second"}}
	saver := &recordingSaver{failFirst: true}
	done := make(chan struct{}, 1)
	search := func(_ context.Context, _ []float32, _ int, _ float64, _ string) ([]hmem.Entry, error) {
		return nil, nil
	}
	k := newKGWithIngest(&kgFakeEmbedder{}, ext, search, saver.save, 0.85, 2, 4, func() { done <- struct{}{} })

	k.Ingest(context.Background(), twoMsgThread())
	<-done
	got := saver.snapshot()
	if len(got) != 1 || got[0].Content != "second" {
		t.Fatalf("save error on first ⇒ second still saved: %+v", got)
	}
}

func TestKG_IngestEmbedFailSavesAnyway(t *testing.T) {
	ext := &fakeExtractor{facts: []string{"fact"}}
	saver := &recordingSaver{}
	done := make(chan struct{}, 1)
	emb := &kgFakeEmbedder{fail: true} // dedup embed fails → cannot dedup → save anyway
	searchCalled := false
	search := func(_ context.Context, _ []float32, _ int, _ float64, _ string) ([]hmem.Entry, error) {
		searchCalled = true
		return nil, nil
	}
	k := newKGWithIngest(emb, ext, search, saver.save, 0.85, 2, 4, func() { done <- struct{}{} })

	k.Ingest(context.Background(), twoMsgThread())
	<-done
	if searchCalled {
		t.Fatal("search must be skipped when dedup embed fails")
	}
	if got := saver.snapshot(); len(got) != 1 || got[0].Content != "fact" {
		t.Fatalf("embed-fail dedup ⇒ save anyway: %+v", got)
	}
}

func TestForSession_BindsSessionIDToIngest(t *testing.T) {
	var gotSession string
	g := &KG{
		save: func(_ context.Context, _ hmem.Entry, _ string) error { return nil },
	}
	// A summary strategy that records the session id it was asked to write under.
	g.putSummary = func(_ context.Context, sessionID, _ string) error { gotSession = sessionID; return nil }
	g.strategies = []Strategy{&fakeStrategy{kind: "summary", mode: WriteSupersede, shouldRun: true, records: []string{"digest"}}}
	done := make(chan struct{})
	g.ingestDone = func() { close(done) }
	g.sem = make(chan struct{}, 1)

	kg := g.ForSession("sess-42", "")
	kg.Ingest(context.Background(), []hrt.Message{{Role: "user", Content: "one two three"}, {Role: "assistant", Content: "ok"}})
	<-done
	if gotSession != "sess-42" {
		t.Fatalf("summary written under session %q, want sess-42", gotSession)
	}
}

func TestKG_IngestDropsOverCapacity(t *testing.T) {
	ext := &fakeExtractor{facts: []string{"x"}}
	saver := &recordingSaver{}
	done := make(chan struct{}, 1)
	search := func(_ context.Context, _ []float32, _ int, _ float64, _ string) ([]hmem.Entry, error) {
		return nil, nil
	}
	// maxInflight 1; pre-fill the slot so the next Ingest is dropped.
	k := newKGWithIngest(&kgFakeEmbedder{}, ext, search, saver.save, 0.85, 2, 1, func() { done <- struct{}{} })
	k.sem <- struct{}{} // occupy the only slot

	k.Ingest(context.Background(), twoMsgThread()) // must drop, not block
	<-done                                         // drop path still fires ingestDone
	if len(saver.snapshot()) != 0 {
		t.Fatal("over-capacity ingest must drop (no extract/save)")
	}
}

func TestRecallForSession_ThreeBlocks(t *testing.T) {
	emb := &kgFakeEmbedder{}
	search := func(_ context.Context, _ []float32, _ int, _ float64, kind string) ([]hmem.Entry, error) {
		switch kind {
		case KindFact:
			return []hmem.Entry{{Content: "user prefers dark mode"}}, nil
		case KindEpisode:
			return []hmem.Entry{{Content: "deployed staging on tuesday"}}, nil
		}
		return nil, nil
	}
	g := newKGWithSearch(emb, 5, 0.0, search)
	g.episodicK = 3
	g.getSummary = func(_ context.Context, _ string) (string, bool, error) {
		return "we discussed the deploy plan", true, nil
	}
	out := g.recallForSession(context.Background(), "what did we do", "sess-1")
	for _, want := range []string{
		"Relevant memories:", "user prefers dark mode",
		"Relevant past events:", "deployed staging on tuesday",
		"Conversation summary so far:", "we discussed the deploy plan",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("recall missing %q in:\n%s", want, out)
		}
	}
	// Order: facts, then events, then summary.
	fi := strings.Index(out, "Relevant memories:")
	ei := strings.Index(out, "Relevant past events:")
	si := strings.Index(out, "Conversation summary so far:")
	if !(fi < ei && ei < si) {
		t.Fatalf("blocks out of order: facts=%d events=%d summary=%d\n%s", fi, ei, si, out)
	}
}

func TestRecallEpisodes_EmptyOmitsBlock(t *testing.T) {
	emb := &kgFakeEmbedder{}
	search := func(_ context.Context, _ []float32, _ int, _ float64, _ string) ([]hmem.Entry, error) {
		return nil, nil // nothing for any kind
	}
	g := newKGWithSearch(emb, 5, 0.0, search)
	g.episodicK = 3
	out := g.recallForSession(context.Background(), "anything", "s1")
	if strings.Contains(out, "Relevant past events:") {
		t.Fatalf("empty episode search must omit the block, got:\n%s", out)
	}
}
