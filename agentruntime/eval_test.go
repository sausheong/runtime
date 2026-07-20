package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/sausheong/harness/session"
	"github.com/sausheong/runtime/internal/eval"
)

func TestSampledDeterministic(t *testing.T) {
	// same id ⇒ same decision, regardless of repeat calls
	id := "sess-abc"
	first := sampled(id, 50)
	for i := 0; i < 5; i++ {
		if sampled(id, 50) != first {
			t.Fatal("sampled not deterministic for same id")
		}
	}
	// bounds
	if sampled(id, 0) {
		t.Error("rate 0 must never sample")
	}
	if !sampled(id, 100) {
		t.Error("rate 100 must always sample")
	}
	// rough distribution: over many ids, count should be within a wide band of 50%
	n, hit := 2000, 0
	for i := 0; i < n; i++ {
		if sampled("id-"+string(rune(i))+"-x", 50) {
			hit++
		}
	}
	if hit < n/4 || hit > 3*n/4 {
		t.Errorf("distribution off: %d/%d", hit, n)
	}
}

func msgEntry(role, text string) session.SessionEntry {
	d, _ := json.Marshal(session.MessageData{Text: text})
	return session.SessionEntry{Type: session.EntryTypeMessage, Role: role, Data: d}
}

func TestFinalAssistantText(t *testing.T) {
	entries := []session.SessionEntry{
		msgEntry("user", "hi"),
		msgEntry("assistant", "first"),
		msgEntry("assistant", "final answer"),
	}
	if got := finalAssistantText(entries); got != "final answer" {
		t.Errorf("got %q want %q", got, "final answer")
	}
	if got := finalAssistantText([]session.SessionEntry{msgEntry("user", "x")}); got != "" {
		t.Errorf("no-assistant want empty, got %q", got)
	}
}

// erroringJudge always fails transport — mirrors M1's fake-judge pattern
// (internal/eval/scorer_test.go). eval.Score turns this into a failed criterion
// with a detail, never a propagated error, so scoreOnto's loop continues.
type erroringJudge struct{}

func (erroringJudge) Grade(_ context.Context, _, _, _ string) (bool, string, error) {
	return false, "", errors.New("judge boom")
}

// fakeResultStore records the criteria persisted via PutOnlineResult.
type fakeResultStore struct{ puts []string }

func (f *fakeResultStore) PutOnlineResult(_ context.Context, s, c, t, a, sc string, p bool, d string) error {
	f.puts = append(f.puts, c)
	return nil
}

// newTestManagerForScoring builds a Manager with just the scoring deps set: the
// policy + judge. metrics is left nil (nil-safe) and st is unused because the
// test drives scoreOnto with an injected resultPutter.
func newTestManagerForScoring(pol *eval.Policy, j eval.Judge) *Manager {
	return &Manager{evalPolicy: pol, evalJudge: j}
}

func TestScoreSessionAllCriteriaFailClosed(t *testing.T) {
	pol := &eval.Policy{Tenant: "t1", AgentID: "a1", SampleRate: 100, Criteria: []eval.Criterion{
		{Name: "has-final", Scorer: eval.ScorerContains, Pattern: "final"}, // pass
		{Name: "has-zzz", Scorer: eval.ScorerContains, Pattern: "zzz"},     // fail
		{Name: "j", Scorer: eval.ScorerJudge, Rubric: "polite"},           // judge err → fail-criterion
	}}
	m := newTestManagerForScoring(pol, erroringJudge{})
	rs := &fakeResultStore{}
	m.scoreOnto(rs, "s1", "t1", "alice", []session.SessionEntry{msgEntry("assistant", "the final answer")})
	if len(rs.puts) != 3 {
		t.Fatalf("want 3 criteria persisted (judge error must NOT abort), got %d", len(rs.puts))
	}
}
