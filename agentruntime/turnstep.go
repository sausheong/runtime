package agentruntime

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/sausheong/harness/llm"
	"github.com/sausheong/harness/session"
)

// WireEvent is the SSE-facing event the platform publishes for a session.
// Minimal for M1: text + lifecycle. Shared with the HTTP layer.
type WireEvent struct {
	Type string `json:"type"` // text | tool_result | done | error
	Text string `json:"text,omitempty"`
	Err  string `json:"error,omitempty"`
	// Seq is the store-assigned sequence number. Carried in memory for SSE
	// id: emission and client dedupe/resume; excluded from the JSON payload.
	Seq int64 `json:"-"`
}

// turnInput is the JSON-serializable input to the durable session workflow.
type turnInput struct {
	UserMsg   string `json:"user_msg"`             // non-empty only on the first turn
	ImageB64  string `json:"image_b64,omitempty"`  // optional base64 image, first turn only
	ImageMime string `json:"image_mime,omitempty"` // defaults to image/jpeg when ImageB64 set
	// RequestID is the X-Request-ID of the originating POST /sessions. Part of
	// the checkpointed workflow input, so it is re-supplied identically on DBOS
	// replay (replay-safe) and lets turn logs correlate back to the HTTP edge.
	RequestID string `json:"request_id,omitempty"`
	// StartedAt is the session's wall-clock start, stamped once by
	// startSession and re-supplied verbatim on DBOS replay: the deterministic
	// origin for session_timeout. Zero on pre-upgrade in-flight sessions
	// (those skip the session_timeout check).
	StartedAt time.Time `json:"started_at,omitempty"`
}

// turnOutput is the checkpointed return value of a single turn step. On replay
// DBOS returns this verbatim without re-executing the turn, so it must carry
// everything needed to rebuild session state: the entries the turn produced.
type turnOutput struct {
	Done    bool                   `json:"done"`
	Reason  string                 `json:"reason"`
	Entries []session.SessionEntry `json:"entries"`
	// Usage is this turn's token usage, checkpointed with the entries so the
	// cumulative max_tokens budget is rebuilt exactly on replay. Nil on
	// pre-upgrade checkpoints and usage-less turns (counts 0).
	Usage *llm.Usage `json:"usage,omitempty"`
}

// timeoutCheck is the checkpointed verdict of the once-per-iteration
// session_timeout decision step. The step reads the real clock ONCE live;
// on replay DBOS returns this verdict verbatim, so replay never consults
// the clock.
type timeoutCheck struct {
	ElapsedMS int64 `json:"elapsed_ms"`
	Exceeded  bool  `json:"exceeded"`
}

// sumTokens converts one turn's usage to its budget contribution:
// input + output. Cache tokens are excluded by design. Nil ⇒ 0.
func sumTokens(u *llm.Usage) int {
	if u == nil {
		return 0
	}
	return u.InputTokens + u.OutputTokens
}

// breachMsg formats the uniform limit-breach event text.
func breachMsg(limit string, observed, configured int64) string {
	return fmt.Sprintf("limit exceeded: %s (%d/%d)", limit, observed, configured)
}

// applyEntries re-applies a turn's entries onto the in-memory session. Used
// after each turn step (live AND on replay of checkpointed turns) so session
// state matches what the LLM originally saw. Pure w.r.t. (sess, entries).
func applyEntries(sess *session.Session, entries []session.SessionEntry) {
	for _, e := range entries {
		sess.Append(e)
	}
}

// publishableEvents derives the client-facing events from a turn's entries.
// Deterministic: identical entries always yield identical events, so replay
// and live execution publish the same stream. M1 surfaces assistant text only.
func publishableEvents(entries []session.SessionEntry) []WireEvent {
	var out []WireEvent
	for _, e := range entries {
		if e.Type != session.EntryTypeMessage || e.Role != "assistant" {
			continue
		}
		var md session.MessageData
		if err := json.Unmarshal(e.Data, &md); err != nil {
			continue
		}
		if md.Text != "" {
			out = append(out, WireEvent{Type: "text", Text: md.Text})
		}
	}
	return out
}
