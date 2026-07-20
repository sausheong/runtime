package agentruntime

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"log/slog"

	"github.com/sausheong/harness/session"
	"github.com/sausheong/runtime/internal/eval"
)

// sampled is the deterministic sample decision: fnv32a(sessionID) % 100 < rate.
// Deterministic (never rand) so a DBOS replay of the terminal block makes the
// identical decision. rate<=0 ⇒ never; rate>=100 ⇒ always.
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

// finalAssistantText returns the last assistant message's text in entries, "" if
// none. Mirrors publishableEvents' assistant-text extraction.
func finalAssistantText(entries []session.SessionEntry) string {
	out := ""
	for _, e := range entries {
		if e.Type != session.EntryTypeMessage || e.Role != "assistant" {
			continue
		}
		var md session.MessageData
		if err := json.Unmarshal(e.Data, &md); err != nil {
			continue
		}
		if md.Text != "" {
			out = md.Text
		}
	}
	return out
}

// resultPutter is the minimal store surface scoreOnto needs (test seam).
type resultPutter interface {
	PutOnlineResult(ctx context.Context, sessionID, criterion, tenant, actor, scorer string, passed bool, detail string) error
}

// scoreSession scores a finished session's output against the configured policy
// and persists one result per criterion. Best-effort: called in a background
// goroutine off the turn path; a judge/criterion error fails THAT criterion,
// never the session. No-op when no policy/criteria.
func (m *Manager) scoreSession(sessionID, tenant, actor string, entries []session.SessionEntry) {
	if m.evalPolicy == nil || len(m.evalPolicy.Criteria) == 0 {
		return
	}
	m.scoreOnto(m.st, sessionID, tenant, actor, entries)
}

// criterionCase maps a policy Criterion onto the eval.Case scoring vocabulary so
// eval.Score can evaluate it: judge carries Rubric, contains/regex carry Pattern
// as Expected. Mirrors eval.Criterion.toCase (unexported in that package).
func criterionCase(c eval.Criterion) eval.Case {
	if c.Scorer == eval.ScorerJudge {
		return eval.Case{Scorer: eval.ScorerJudge, Rubric: c.Rubric}
	}
	return eval.Case{Scorer: c.Scorer, Expected: c.Pattern}
}

func (m *Manager) scoreOnto(rs resultPutter, sessionID, tenant, actor string, entries []session.SessionEntry) {
	ctx := context.Background()
	output := finalAssistantText(entries)
	for _, c := range m.evalPolicy.Criteria {
		passed, detail := eval.Score(ctx, m.evalJudge, criterionCase(c), output)
		if err := rs.PutOnlineResult(ctx, sessionID, c.Name, tenant, actor, string(c.Scorer), passed, detail); err != nil {
			slog.Warn("eval: put online result failed", "session", sessionID, "criterion", c.Name, "err", err)
		}
		res := "fail"
		if passed {
			res = "pass"
		}
		m.metrics.EvalCriterion(res)
	}
	m.metrics.EvalSessionScored()
}
