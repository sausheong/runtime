package agentruntime

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/sausheong/harness/session"
)

// Failure-category taxonomy (P3.1 M3). One category per terminal session,
// assigned by classify's fixed precedence. "" (unclassified) is not a member —
// a non-terminal session simply never gets classified.
const (
	CatNone          = "none"
	CatQualityFail   = "quality_fail"
	CatToolError     = "tool_error"
	CatAgentError    = "agent_error"
	CatTimeout       = "timeout"
	CatLimitExceeded = "limit_exceeded"
)

// classify assigns exactly one failure category by fixed first-match precedence
// (highest→lowest). Pure and deterministic: identical inputs → identical output
// (replay-safe).
//
//	status         — final session status: completed | error | limit_exceeded
//	terminalReason — the terminal turn's stop reason (completed | error |
//	                 aborted | limit:turn_timeout | limit_exceeded)
//	toolErrored    — any turn had a tool_result with IsError
//	qualityFailed  — any online_eval_results row for the session failed
//
// Precedence note: a per-turn deadline sets status=limit_exceeded AND
// terminalReason=limit:turn_timeout; the timeout check comes BEFORE the
// limit_exceeded status check so a per-turn deadline reports as timeout, not
// limit_exceeded (a distinct operational signal). An errored/limited session is
// never judged on tool/quality.
func classify(status, terminalReason string, toolErrored, qualityFailed bool) string {
	switch {
	case terminalReason == "limit:turn_timeout":
		return CatTimeout
	case status == "limit_exceeded":
		return CatLimitExceeded
	case status == "error" || terminalReason == "error" || terminalReason == "aborted":
		return CatAgentError
	case toolErrored:
		return CatToolError
	case qualityFailed:
		return CatQualityFail
	default:
		return CatNone
	}
}

// entriesHaveToolError reports whether any entry is a tool_result marked
// IsError. Mirrors finalAssistantText's entry-walk (agentruntime/eval.go): a
// malformed tool_result Data is skipped, never fatal.
func entriesHaveToolError(entries []session.SessionEntry) bool {
	for _, e := range entries {
		if e.Type != session.EntryTypeToolResult {
			continue
		}
		var tr session.ToolResultData
		if err := json.Unmarshal(e.Data, &tr); err != nil {
			continue
		}
		if tr.IsError {
			return true
		}
	}
	return false
}

// classifyAndPersist derives the terminal session's failure category and records
// it (store + metric). Best-effort and non-fatal: a store error is logged, never
// returned — classification must never affect a turn. Deterministic + idempotent
// ⇒ a DBOS replay re-derives and re-writes the identical category.
func (m *Manager) classifyAndPersist(sessionID, status, terminalReason string, toolErrored, qualityFailed bool) {
	cat := classify(status, terminalReason, toolErrored, qualityFailed)
	if err := m.st.SetFailureCategory(context.Background(), sessionID, cat); err != nil {
		slog.Warn("eval: set failure category failed", "session", sessionID, "category", cat, "err", err)
		return
	}
	m.metrics.FailureClassified(cat)
}
