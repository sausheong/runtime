package eval

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	finalizeTimeout       = 5 * time.Second
	defaultRecoveryWorker = 4
	defaultSubmitWorkers  = 4
	defaultSubmitQueue    = 256
)

var (
	runLeaseDuration     = 10 * time.Minute
	recoveryScanInterval = time.Minute
)

var ErrRunNotClaimed = errors.New("eval: run is already leased or terminal")

type executionJob struct {
	ctx   context.Context
	st    EvalStore
	inv   Invoker
	judge Judge
	runID string
	m     Metricer
}

type submitter struct {
	jobs chan executionJob
}

func newSubmitter(workers, queue int) *submitter {
	s := &submitter{jobs: make(chan executionJob, queue)}
	for range workers {
		go func() {
			for job := range s.jobs {
				Execute(job.ctx, job.st, job.inv, job.judge, job.runID, job.m)
			}
		}()
	}
	return s
}

func (s *submitter) submit(job executionJob) bool {
	select {
	case s.jobs <- job:
		return true
	default:
		return false
	}
}

var defaultSubmitter = newSubmitter(defaultSubmitWorkers, defaultSubmitQueue)

// Submit queues normal (API/console) execution without creating one goroutine
// per request. False means the bounded queue is saturated.
func Submit(ctx context.Context, st EvalStore, inv Invoker, j Judge, runID string, m Metricer) bool {
	return defaultSubmitter.submit(executionJob{ctx: ctx, st: st, inv: inv, judge: j, runID: runID, m: m})
}

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

// Execute runs a durable eval worker and logs transition errors. Callers that
// need the result (principally tests and recovery orchestration) use
// ExecuteChecked.
func Execute(ctx context.Context, st EvalStore, inv Invoker, j Judge, runID string, m Metricer) {
	if err := ExecuteChecked(ctx, st, inv, j, runID, m); err != nil &&
		!errors.Is(err, context.Canceled) && !errors.Is(err, ErrRunNotClaimed) {
		slog.Error("eval run failed", "run", runID, "err", err)
	}
}

// ExecuteChecked claims a run, resumes missing case indexes, and finalizes it
// only while still holding the lease. Store transition failures are returned,
// never silently discarded.
func ExecuteChecked(ctx context.Context, st EvalStore, inv Invoker, j Judge, runID string, m Metricer) error {
	run, ok, err := st.GetRun(ctx, runID)
	if err != nil {
		return fmt.Errorf("load run: %w", err)
	}
	if !ok {
		return fmt.Errorf("load run: not found")
	}

	owner := uuid.NewString()
	if err := claim(ctx, st, runID, owner); err != nil {
		return err
	}
	runCtx, cancelRun := context.WithCancel(ctx)
	heartbeat := startLeaseHeartbeat(runCtx, cancelRun, st, runID, owner)
	defer heartbeat.stop()

	set, ok, err := st.GetSet(runCtx, run.Tenant, run.SetName)
	if err != nil {
		return finalizeError(ctx, st, run, owner, 0, 0, 0, err)
	}
	if !ok {
		return finalizeError(ctx, st, run, owner, 0, 0, 0, errors.New("set not found"))
	}

	existing, err := st.ListResults(runCtx, runID)
	if err != nil {
		return finalizeError(ctx, st, run, owner, 0, 0, 0, err)
	}
	byIndex := make(map[int]Result, len(existing))
	for _, result := range existing {
		if result.CaseIndex >= 0 && result.CaseIndex < len(set.Cases) {
			byIndex[result.CaseIndex] = result
		}
	}

	total, passed, failed := 0, 0, 0
	for i, c := range set.Cases {
		if err := runCtx.Err(); err != nil {
			if heartbeatErr := heartbeat.err(); heartbeatErr != nil {
				return heartbeatErr
			}
			// Leave the lease to expire. Recovery can safely resume the absent
			// case indexes without recording shutdown as an eval failure.
			return err
		}
		if result, found := byIndex[i]; found {
			total++
			if result.Passed {
				passed++
			} else {
				failed++
			}
			continue
		}
		if err := claim(runCtx, st, runID, owner); err != nil {
			return err
		}

		output, pass, detail := "", false, ""
		out, invokeErr := inv.Invoke(runCtx, run.AgentID, c.Input)
		if err := runCtx.Err(); err != nil {
			if heartbeatErr := heartbeat.err(); heartbeatErr != nil {
				return heartbeatErr
			}
			return err
		}
		if invokeErr != nil {
			detail = "invoke error: " + invokeErr.Error()
		} else {
			output = out
			pass, detail = Score(runCtx, j, c, out)
		}
		written, err := st.PutResultClaimed(runCtx, runID, owner, Result{
			CaseIndex: i,
			Input:     c.Input,
			Output:    output,
			Scorer:    string(c.Scorer),
			Passed:    pass,
			Detail:    detail,
		})
		if err != nil {
			if runCtx.Err() != nil {
				return runCtx.Err()
			}
			return finalizeError(ctx, st, run, owner, total, passed, failed, err)
		}
		if !written {
			return ErrRunNotClaimed
		}

		total++
		result := "fail"
		if pass {
			passed++
			result = "pass"
		} else {
			failed++
		}
		if m != nil {
			m.EvalCase(run.Tenant, result)
		}
	}

	if err := heartbeat.stop(); err != nil {
		return err
	}
	finalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalizeTimeout)
	defer cancel()
	finished, err := st.FinishRunClaimed(
		finalCtx, runID, owner, StatusCompleted,
		total, passed, failed, score(passed, total), "",
	)
	if err != nil {
		return err
	}
	if !finished {
		return ErrRunNotClaimed
	}
	if m != nil {
		m.EvalRun(run.Tenant, StatusCompleted)
	}
	return nil
}

type leaseHeartbeat struct {
	stopOnce sync.Once
	stopCh   chan struct{}
	done     chan struct{}
	errCh    chan error
}

func startLeaseHeartbeat(ctx context.Context, cancel context.CancelFunc, st EvalStore, runID, owner string) *leaseHeartbeat {
	h := &leaseHeartbeat{
		stopCh: make(chan struct{}),
		done:   make(chan struct{}),
		errCh:  make(chan error, 1),
	}
	interval := runLeaseDuration / 3
	if interval <= 0 {
		interval = time.Millisecond
	}
	go func() {
		defer close(h.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-h.stopCh:
				return
			case <-ticker.C:
				if err := claim(ctx, st, runID, owner); err != nil {
					select {
					case h.errCh <- fmt.Errorf("renew eval lease: %w", err):
					default:
					}
					cancel()
					return
				}
			}
		}
	}()
	return h
}

func (h *leaseHeartbeat) stop() error {
	h.stopOnce.Do(func() { close(h.stopCh) })
	<-h.done
	return h.err()
}

func (h *leaseHeartbeat) err() error {
	select {
	case err := <-h.errCh:
		return err
	default:
		return nil
	}
}

func claim(ctx context.Context, st EvalStore, runID, owner string) error {
	now := time.Now().UTC()
	ok, err := st.ClaimRun(ctx, runID, owner, now, now.Add(runLeaseDuration))
	if err != nil {
		return err
	}
	if !ok {
		return ErrRunNotClaimed
	}
	return nil
}

func finalizeError(ctx context.Context, st EvalStore, run Run, owner string, total, passed, failed int, cause error) error {
	finalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalizeTimeout)
	defer cancel()
	finished, finishErr := st.FinishRunClaimed(
		finalCtx, run.RunID, owner, StatusError,
		total, passed, failed, score(passed, total), cause.Error(),
	)
	if finishErr != nil {
		return errors.Join(cause, finishErr)
	}
	if !finished {
		return errors.Join(cause, ErrRunNotClaimed)
	}
	return cause
}

// RecoverIncomplete starts a bounded worker pool for pending/running runs.
// ExecuteChecked's lease prevents duplicate execution across control-plane
// replicas or overlapping recovery scans.
func RecoverIncomplete(ctx context.Context, st EvalStore, inv Invoker, j Judge, m Metricer) error {
	return recoverIncomplete(ctx, st, inv, j, m, defaultRecoveryWorker)
}

func recoverIncomplete(ctx context.Context, st EvalStore, inv Invoker, j Judge, m Metricer, workers int) error {
	if workers < 1 {
		workers = 1
	}
	jobs := make(chan string, workers*4)
	scanLimit := cap(jobs)
	for range workers {
		go func() {
			for runID := range jobs {
				Execute(ctx, st, inv, j, runID, m)
			}
		}()
	}
	scan := func() error {
		runs, err := st.ListIncompleteRuns(ctx, scanLimit)
		if err != nil {
			return err
		}
		for _, run := range runs {
			select {
			case jobs <- run.RunID:
			case <-ctx.Done():
				return ctx.Err()
			default:
				return nil
			}
		}
		return nil
	}
	if err := scan(); err != nil {
		close(jobs)
		return err
	}
	go func() {
		ticker := time.NewTicker(recoveryScanInterval)
		defer ticker.Stop()
		defer close(jobs)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := scan(); err != nil && !errors.Is(err, context.Canceled) {
					slog.Error("eval recovery scan failed", "err", err)
				}
			}
		}
	}()
	return nil
}

func score(passed, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(passed) / float64(total)
}
