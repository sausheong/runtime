package agentruntime

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"log/slog"
	"time"

	"github.com/sausheong/harness/session"
	"github.com/sausheong/runtime/internal/eval"
)

type scoreJob struct {
	sessionID      string
	tenant         string
	actor          string
	status         string
	terminalReason string
	toolErrored    bool
	entries        []session.SessionEntry
}

// sampled is the deterministic sample decision: fnv32a(sessionID) % 100 < rate.
func sampled(sessionID string, rate int) bool {
	if rate <= 0 {
		return false
	}
	if rate >= 100 {
		return true
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(sessionID))
	return int(h.Sum32()%100) < rate
}

func finalAssistantText(entries []session.SessionEntry) string {
	out := ""
	for _, e := range entries {
		if e.Type != session.EntryTypeMessage || e.Role != "assistant" {
			continue
		}
		var md session.MessageData
		if err := json.Unmarshal(e.Data, &md); err == nil && md.Text != "" {
			out = md.Text
		}
	}
	return out
}

type resultPutter interface {
	PutOnlineResult(ctx context.Context, sessionID, criterion, tenant, actor, scorer string, passed bool, detail string) error
}

func (m *Manager) startScoring(workers, queue int, timeout time.Duration) {
	ctx, cancel := context.WithCancel(context.Background())
	m.scoreMu.Lock()
	m.scoreQueue = make(chan scoreJob, queue)
	m.scoreCancel = cancel
	m.scoreTimeout = timeout
	m.scoreClosed = false
	m.scoreMu.Unlock()
	for range workers {
		m.scoreWG.Add(1)
		go func() {
			defer m.scoreWG.Done()
			for job := range m.scoreQueue {
				jobCtx, stop := context.WithTimeout(ctx, timeout)
				m.scoreSession(jobCtx, job)
				stop()
			}
		}()
	}
}

// enqueueScore is intentionally non-blocking. Saturated queues drop the
// sampled job and expose that decision through a bounded metric.
func (m *Manager) enqueueScore(job scoreJob) bool {
	m.scoreMu.RLock()
	defer m.scoreMu.RUnlock()
	if m.scoreClosed || m.scoreQueue == nil {
		m.metrics.EvalQueueDropped("shutdown")
		return false
	}
	select {
	case m.scoreQueue <- job:
		return true
	default:
		m.metrics.EvalQueueDropped("full")
		return false
	}
}

// stopScoring first closes the queue so workers drain it. If drain exceeds the
// deadline, lifecycle cancellation stops provider and database calls.
func (m *Manager) stopScoring(timeout time.Duration) {
	m.scoreMu.Lock()
	if m.scoreClosed || m.scoreQueue == nil {
		m.scoreMu.Unlock()
		return
	}
	m.scoreClosed = true
	close(m.scoreQueue)
	cancel := m.scoreCancel
	m.scoreMu.Unlock()

	done := make(chan struct{})
	go func() {
		m.scoreWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		cancel()
		select {
		case <-done:
		case <-time.After(100 * time.Millisecond):
			m.metrics.EvalQueueDropped("shutdown_timeout")
			slog.Error("online scoring workers ignored cancellation; continuing bounded shutdown")
		}
	}
	cancel()
}

func (m *Manager) scoreSession(ctx context.Context, job scoreJob) {
	m.scoreOnto(ctx, m.st, job.sessionID, job.tenant, job.actor, job.status, job.terminalReason, job.toolErrored, job.entries)
}

func criterionCase(c eval.Criterion) eval.Case {
	if c.Scorer == eval.ScorerJudge {
		return eval.Case{Scorer: eval.ScorerJudge, Rubric: c.Rubric}
	}
	return eval.Case{Scorer: c.Scorer, Expected: c.Pattern}
}

func (m *Manager) scoreOnto(ctx context.Context, rs resultPutter, sessionID, tenant, actor, status, terminalReason string, toolErrored bool, entries []session.SessionEntry) {
	output := finalAssistantText(entries)
	qualityFailed := false
	allPersisted := true
	if m.evalPolicy != nil {
		for _, c := range m.evalPolicy.Criteria {
			if ctx.Err() != nil {
				allPersisted = false
				break
			}
			passed, detail := eval.Score(ctx, m.evalJudge, criterionCase(c), output)
			if err := rs.PutOnlineResult(ctx, sessionID, c.Name, tenant, actor, string(c.Scorer), passed, detail); err != nil {
				allPersisted = false
				slog.Warn("eval: put online result failed", "session", sessionID, "criterion", c.Name, "err", err)
				continue
			}
			if !passed {
				qualityFailed = true
			}
			result := "fail"
			if passed {
				result = "pass"
			}
			m.metrics.EvalCriterion(result)
		}
		if allPersisted {
			m.metrics.EvalSessionScored()
		}
	}
	m.classifyAndPersistContext(ctx, sessionID, status, terminalReason, toolErrored, qualityFailed)
}
