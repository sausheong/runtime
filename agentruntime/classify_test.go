package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/sausheong/harness/session"
	"github.com/sausheong/runtime/internal/obs"
	"github.com/sausheong/runtime/internal/store"
)

func TestClassifyPrecedence(t *testing.T) {
	cases := []struct {
		name           string
		status         string
		terminalReason string
		toolErrored    bool
		qualityFailed  bool
		want           string
	}{
		// Rule 6: the common clean case.
		{"clean", "completed", "completed", false, false, CatNone},
		// Rule 5: quality miss on a clean completion.
		{"quality_fail", "completed", "completed", false, true, CatQualityFail},
		// Rule 4: tool error on a clean completion outranks quality_fail.
		{"tool_over_quality", "completed", "completed", true, true, CatToolError},
		// Rule 3: agent error via status.
		{"error_status", "error", "error", false, false, CatAgentError},
		// Rule 3: agent error via aborted reason.
		{"aborted_reason", "error", "aborted", false, false, CatAgentError},
		// Rule 3 outranks rule 4: an errored session is NOT judged on tool/quality.
		{"error_over_tool", "error", "error", true, true, CatAgentError},
		// Rule 2: per-turn deadline. NOTE failLimit sets status=limit_exceeded
		// AND reason=limit:turn_timeout — timeout MUST win over limit_exceeded.
		{"turn_timeout_beats_limit", "limit_exceeded", "limit:turn_timeout", false, false, CatTimeout},
		// Rule 1: cumulative-budget limit (max_turns/max_tokens/session_timeout).
		{"limit_exceeded", "limit_exceeded", "limit_exceeded", false, false, CatLimitExceeded},
		// Rule 1 outranks rule 4: a limit breach with a tool error is limit_exceeded.
		{"limit_over_tool", "limit_exceeded", "limit_exceeded", true, false, CatLimitExceeded},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classify(c.status, c.terminalReason, c.toolErrored, c.qualityFailed)
			if got != c.want {
				t.Fatalf("classify(%q,%q,%v,%v)=%q, want %q",
					c.status, c.terminalReason, c.toolErrored, c.qualityFailed, got, c.want)
			}
		})
	}
}

func toolResultEntry(t *testing.T, isErr bool) session.SessionEntry {
	t.Helper()
	data, _ := json.Marshal(session.ToolResultData{Output: "x", IsError: isErr})
	return session.SessionEntry{Type: session.EntryTypeToolResult, Data: data}
}

func TestEntriesHaveToolError(t *testing.T) {
	if entriesHaveToolError(nil) {
		t.Fatal("nil entries ⇒ false")
	}
	clean := []session.SessionEntry{toolResultEntry(t, false)}
	if entriesHaveToolError(clean) {
		t.Fatal("clean tool_result ⇒ false")
	}
	errd := []session.SessionEntry{toolResultEntry(t, false), toolResultEntry(t, true)}
	if !entriesHaveToolError(errd) {
		t.Fatal("a tool_result with IsError ⇒ true")
	}
	// A non-tool_result entry with junk data must not panic or match.
	msg := session.SessionEntry{Type: session.EntryTypeMessage, Data: json.RawMessage(`{"text":"hi"}`)}
	if entriesHaveToolError([]session.SessionEntry{msg}) {
		t.Fatal("message entry ⇒ false")
	}
	// A tool_result whose Data is malformed JSON must be skipped, not fatal
	// (exercises the unmarshal-error continue branch).
	bad := session.SessionEntry{Type: session.EntryTypeToolResult, Data: json.RawMessage(`{`)}
	if entriesHaveToolError([]session.SessionEntry{bad}) {
		t.Fatal("malformed tool_result Data ⇒ false (skipped, not fatal)")
	}
}

// fakeCatStore captures the category set via SetFailureCategory; every other
// store method is inherited from the embedded (nil) interface and never called.
type fakeCatStore struct {
	store.Store // embed to satisfy the interface; only SetFailureCategory used
	setID       string
	setCat      string
	err         error
}

func (f *fakeCatStore) SetFailureCategory(_ context.Context, id, cat string) error {
	f.setID, f.setCat = id, cat
	return f.err
}

func TestClassifyAndPersist(t *testing.T) {
	fs := &fakeCatStore{}
	m := &Manager{st: fs, metrics: obs.NewAgentMetrics("a", "t", "m")}
	m.classifyAndPersist("sess-1", "completed", "completed", true, false)
	if fs.setID != "sess-1" || fs.setCat != CatToolError {
		t.Fatalf("persisted (%q,%q), want (sess-1,%s)", fs.setID, fs.setCat, CatToolError)
	}

	// A store error is non-fatal (no panic, no propagation — void method).
	fs.err = errors.New("db down")
	m.classifyAndPersist("sess-2", "completed", "completed", false, false) // must not panic
}
