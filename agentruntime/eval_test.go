package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sausheong/harness/session"
	"github.com/sausheong/runtime/internal/eval"
	"github.com/sausheong/runtime/internal/obs"
	"github.com/sausheong/runtime/internal/store"
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
// policy + judge. metrics is left nil (nil-safe). st is a fake capturing store:
// scoreOnto's M3 tail now calls classifyAndPersist, which persists the derived
// category via m.st.SetFailureCategory — so st must be non-nil even though the
// criteria loop writes through the injected resultPutter.
func newTestManagerForScoring(pol *eval.Policy, j eval.Judge) *Manager {
	return &Manager{evalPolicy: pol, evalJudge: j, st: &fakeCatStore{}}
}

func TestScoreSessionAllCriteriaFailClosed(t *testing.T) {
	pol := &eval.Policy{Tenant: "t1", AgentID: "a1", SampleRate: 100, Criteria: []eval.Criterion{
		{Name: "has-final", Scorer: eval.ScorerContains, Pattern: "final"}, // pass
		{Name: "has-zzz", Scorer: eval.ScorerContains, Pattern: "zzz"},     // fail
		{Name: "j", Scorer: eval.ScorerJudge, Rubric: "polite"},            // judge err → fail-criterion
	}}
	m := newTestManagerForScoring(pol, erroringJudge{})
	rs := &fakeResultStore{}
	m.scoreOnto(context.Background(), rs, "s1", "t1", "alice", "completed", "completed", false, []session.SessionEntry{msgEntry("assistant", "the final answer")})
	if len(rs.puts) != 3 {
		t.Fatalf("want 3 criteria persisted (judge error must NOT abort), got %d", len(rs.puts))
	}
}

type concurrencyJudge struct {
	mu      sync.Mutex
	active  int
	max     int
	started chan struct{}
	release <-chan struct{}
}

func (j *concurrencyJudge) Grade(ctx context.Context, _, _, _ string) (bool, string, error) {
	j.mu.Lock()
	j.active++
	if j.active > j.max {
		j.max = j.active
	}
	if j.started != nil {
		select {
		case j.started <- struct{}{}:
		default:
		}
	}
	j.mu.Unlock()
	select {
	case <-j.release:
	case <-ctx.Done():
	}
	j.mu.Lock()
	j.active--
	j.mu.Unlock()
	return true, "", ctx.Err()
}

func scoringManager(j eval.Judge) *Manager {
	return &Manager{
		agentID: "a",
		tenant:  "t",
		st:      store.NewMemStore(),
		metrics: obs.NewAgentMetrics("a", "t", "test"),
		evalPolicy: &eval.Policy{Criteria: []eval.Criterion{
			{Name: "judge", Scorer: eval.ScorerJudge, Rubric: "ok"},
		}},
		evalJudge: j,
	}
}

func TestScoringWorkerConcurrencyIsBoundedAndDrains(t *testing.T) {
	release := make(chan struct{})
	judge := &concurrencyJudge{release: release}
	m := scoringManager(judge)
	m.startScoring(2, 8, time.Second)
	for i := 0; i < 6; i++ {
		if !m.enqueueScore(scoreJob{sessionID: string(rune('a' + i)), status: "completed", terminalReason: "completed"}) {
			t.Fatalf("job %d unexpectedly dropped", i)
		}
	}
	close(release)
	m.stopScoring(time.Second)
	judge.mu.Lock()
	defer judge.mu.Unlock()
	if judge.max > 2 {
		t.Fatalf("max concurrency=%d, want <=2", judge.max)
	}
}

func TestScoringQueueFullDropsWithoutBlocking(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	judge := &concurrencyJudge{release: release, started: started}
	m := scoringManager(judge)
	m.startScoring(1, 1, time.Second)
	if !m.enqueueScore(scoreJob{sessionID: "one"}) {
		t.Fatal("first job dropped")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}
	if !m.enqueueScore(scoreJob{sessionID: "two"}) {
		t.Fatal("second job should fit queue")
	}
	if m.enqueueScore(scoreJob{sessionID: "three"}) {
		t.Fatal("third job should be dropped from full queue")
	}
	close(release)
	m.stopScoring(time.Second)
	body := scrapeAgentMetrics(t, m)
	if !strings.Contains(body, `agent_eval_queue_dropped_total{agent="a",reason="full",tenant="t"} 1`) {
		t.Fatalf("queue drop metric missing:\n%s", body)
	}
}

func TestScoringShutdownCancelsAfterDrainDeadline(t *testing.T) {
	never := make(chan struct{})
	started := make(chan struct{}, 1)
	m := scoringManager(&concurrencyJudge{release: never, started: started})
	m.startScoring(1, 1, time.Hour)
	m.enqueueScore(scoreJob{sessionID: "one"})
	<-started
	begin := time.Now()
	m.stopScoring(20 * time.Millisecond)
	if elapsed := time.Since(begin); elapsed > time.Second {
		t.Fatalf("shutdown took %s", elapsed)
	}
}

type nonCooperativeJudge struct {
	started chan struct{}
	release <-chan struct{}
}

func (j nonCooperativeJudge) Grade(context.Context, string, string, string) (bool, string, error) {
	close(j.started)
	<-j.release
	return true, "", nil
}

func TestScoringShutdownRemainsBoundedWhenWorkerIgnoresCancellation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	m := scoringManager(nonCooperativeJudge{started: started, release: release})
	m.startScoring(1, 1, time.Hour)
	m.enqueueScore(scoreJob{sessionID: "one"})
	<-started
	begin := time.Now()
	m.stopScoring(20 * time.Millisecond)
	if elapsed := time.Since(begin); elapsed > 500*time.Millisecond {
		t.Fatalf("non-cooperative shutdown took %s", elapsed)
	}
	close(release)
}

type errorResultStore struct{ error }

func (e errorResultStore) PutOnlineResult(context.Context, string, string, string, string, string, bool, string) error {
	return e.error
}

func TestScoringMetricsRequirePersistedResults(t *testing.T) {
	m := newTestManagerForScoring(&eval.Policy{Criteria: []eval.Criterion{
		{Name: "x", Scorer: eval.ScorerContains, Pattern: "x"},
	}}, nil)
	m.metrics = obs.NewAgentMetrics("a", "t", "test")
	m.scoreOnto(context.Background(), errorResultStore{errors.New("down")}, "s", "t", "actor", "completed", "completed", false, []session.SessionEntry{msgEntry("assistant", "x")})
	body := scrapeAgentMetrics(t, m)
	if strings.Contains(body, "agent_eval_criteria_total") || strings.Contains(body, "agent_eval_sessions_scored_total") {
		t.Fatalf("persist-failed metrics were emitted:\n%s", body)
	}
}

func scrapeAgentMetrics(t *testing.T, m *Manager) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.metrics.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body, err := io.ReadAll(rec.Result().Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
