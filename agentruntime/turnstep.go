package agentruntime

import (
	"encoding/json"

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
}

// turnOutput is the checkpointed return value of a single turn step. On replay
// DBOS returns this verbatim without re-executing the turn, so it must carry
// everything needed to rebuild session state: the entries the turn produced.
type turnOutput struct {
	Done    bool                   `json:"done"`
	Reason  string                 `json:"reason"`
	Entries []session.SessionEntry `json:"entries"`
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
