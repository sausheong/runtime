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

	total, passed, failed := 0, 0, 0
	for i, c := range set.Cases {
		if err := ctx.Err(); err != nil {
			_ = st.FinishRun(ctx, runID, StatusError, total, passed, failed, score(passed, total), "cancelled")
			if m != nil {
				m.EvalRun(run.Tenant, StatusError)
			}
			return
		}
		var output string
		var pass bool
		var detail string
		out, ierr := inv.Invoke(ctx, run.AgentID, c.Input)
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

func score(passed, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(passed) / float64(total)
}
