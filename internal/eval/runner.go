package eval

import (
	"context"
	"log/slog"
)

// Invoker runs one input against an agent and returns its output text.
type Invoker interface {
	Invoke(ctx context.Context, agentID, input string) (string, error)
}

// Metricer records eval counters (satisfied by *obs.ControlMetrics; kept as a
// local interface so the runner does not import obs).
type Metricer interface {
	EvalRun(tenant, status string)
	EvalCase(tenant, result string)
}

// Execute runs every case of the run's set sequentially, persisting a result
// per case, then finalizes the run. Best-effort: a per-case invoke or judge
// error fails THAT case and the run still completes. Only a store failure or a
// cancelled ctx ends the run as status=error.
//
// (Named Execute rather than Run because Run is the eval run struct type.)
func Execute(ctx context.Context, st EvalStore, inv Invoker, j Judge, runID string, m Metricer) {
	run, ok, err := st.GetRun(ctx, runID)
	if err != nil || !ok {
		slog.Warn("eval: run not found at start", "run", runID, "err", err)
		return
	}
	set, ok, err := st.GetSet(ctx, run.Tenant, run.SetName)
	if err != nil || !ok {
		_ = st.FinishRun(ctx, runID, StatusError, 0, 0, 0, 0, "set not found")
		if m != nil {
			m.EvalRun(run.Tenant, StatusError)
		}
		return
	}
	_ = st.SetRunStatus(ctx, runID, StatusRunning)

	existing, err := st.ListResults(ctx, runID)
	if err != nil {
		_ = st.FinishRun(context.Background(), runID, StatusError, 0, 0, 0, 0, err.Error())
		return
	}
	byIndex := make(map[int]Result, len(existing))
	for _, result := range existing {
		if result.CaseIndex >= 0 && result.CaseIndex < len(set.Cases) {
			byIndex[result.CaseIndex] = result
		}
	}
	total, passed, failed := 0, 0, 0
	for i, c := range set.Cases {
		if err := ctx.Err(); err != nil {
			// Leave the run in running state. Startup recovery resumes only the
			// missing case indexes, so a normal shutdown never converts
			// recoverable work into a permanent error.
			return
		}
		if result, ok := byIndex[i]; ok {
			total++
			if result.Passed {
				passed++
			} else {
				failed++
			}
			continue
		}
		var output string
		var pass bool
		var detail string
		out, ierr := inv.Invoke(ctx, run.AgentID, c.Input)
		if ctx.Err() != nil {
			// Do not persist a shutdown/interruption as a failed evaluation
			// case. Leaving this index absent lets startup recovery run it.
			return
		}
		if ierr != nil {
			pass, detail = false, "invoke error: "+ierr.Error()
		} else {
			output = out
			pass, detail = Score(ctx, j, c, out)
		}
		if perr := st.PutResult(ctx, runID, Result{
			CaseIndex: i, Input: c.Input, Output: output, Scorer: string(c.Scorer), Passed: pass, Detail: detail,
		}); perr != nil {
			_ = st.FinishRun(ctx, runID, StatusError, total, passed, failed, score(passed, total), perr.Error())
			if m != nil {
				m.EvalRun(run.Tenant, StatusError)
			}
			return
		}
		total++
		if pass {
			passed++
		} else {
			failed++
		}
		if m != nil {
			if pass {
				m.EvalCase(run.Tenant, "pass")
			} else {
				m.EvalCase(run.Tenant, "fail")
			}
		}
	}
	_ = st.FinishRun(ctx, runID, StatusCompleted, total, passed, failed, score(passed, total), "")
	if m != nil {
		m.EvalRun(run.Tenant, StatusCompleted)
	}
}

// RecoverIncomplete restarts pending/running runs after a control-plane
// restart. Execute resumes from persisted case indexes, so already-completed
// agent invocations are not repeated.
func RecoverIncomplete(ctx context.Context, st EvalStore, inv Invoker, j Judge, m Metricer) error {
	runs, err := st.ListRuns(ctx, "")
	if err != nil {
		return err
	}
	for _, run := range runs {
		if run.Status != StatusPending && run.Status != StatusRunning {
			continue
		}
		runID := run.RunID
		go Execute(ctx, st, inv, j, runID, m)
	}
	return nil
}

func score(passed, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(passed) / float64(total)
}
