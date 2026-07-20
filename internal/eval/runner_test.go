package eval

import (
	"context"
	"errors"
	"testing"
)

type fakeInvoker struct {
	out map[string]string // input -> output
	err map[string]error  // input -> error
}

func (f fakeInvoker) Invoke(_ context.Context, _, input string) (string, error) {
	if e := f.err[input]; e != nil {
		return "", e
	}
	return f.out[input], nil
}

type nopMetric struct{ runs, cases int }

func (n *nopMetric) EvalRun(_, _ string)  { n.runs++ }
func (n *nopMetric) EvalCase(_, _ string) { n.cases++ }

func TestRunScoresAllCasesAndCompletes(t *testing.T) {
	ctx := context.Background()
	m := NewMemStore()
	set := Set{Tenant: "t1", Name: "s", Cases: []Case{
		{Input: "a", Scorer: ScorerExact, Expected: "A"},         // pass
		{Input: "b", Scorer: ScorerExact, Expected: "B"},         // fail (output "x")
		{Input: "c", Scorer: ScorerJudge, Rubric: "ok"},          // judge error → fail-the-case
		{Input: "d", Scorer: ScorerExact, Expected: "D"},         // invoke error → fail-the-case
	}}
	_ = m.PutSet(ctx, set)
	_ = m.CreateRun(ctx, Run{RunID: "r", Tenant: "t1", SetName: "s", AgentID: "a1", Status: StatusPending})
	inv := fakeInvoker{
		out: map[string]string{"a": "A", "b": "x", "c": "whatever"},
		err: map[string]error{"d": errors.New("boom")},
	}
	j := fakeJudge{err: errors.New("judge down")}
	met := &nopMetric{}
	Execute(ctx, m, inv, j, "r", met)

	gr, _, _ := m.GetRun(ctx, "r")
	if gr.Status != StatusCompleted {
		t.Fatalf("status=%s want completed (a judge/invoke error must NOT abort the run)", gr.Status)
	}
	if gr.Total != 4 || gr.Passed != 1 || gr.Failed != 3 {
		t.Fatalf("counts total=%d passed=%d failed=%d want 4/1/3", gr.Total, gr.Passed, gr.Failed)
	}
	res, _ := m.ListResults(ctx, "r")
	if len(res) != 4 {
		t.Fatalf("results=%d want 4", len(res))
	}
	if met.cases != 4 || met.runs != 1 {
		t.Fatalf("metrics cases=%d runs=%d want 4/1", met.cases, met.runs)
	}
}
