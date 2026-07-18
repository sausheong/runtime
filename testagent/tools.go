package testagent

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"strconv"
	"time"

	"github.com/sausheong/harness/tool"
)

// MarkerTool is a deterministic tool that records a row each time it runs. It is
// used by the resume integration test to prove a committed side effect survives
// a mid-turn crash.
//
// When CRASH_AFTER_MARKER=1, Execute calls os.Exit(1) immediately AFTER the row
// is committed but BEFORE returning — simulating a crash after a side effect but
// before the turn step checkpoints. On resume the durable workflow re-drives the
// turn and re-runs the marker, so two rows may exist. That is the documented
// at-least-once contract (spec §7); the integration test asserts on it honestly.
type MarkerTool struct {
	DB *sql.DB
}

// Name returns the tool name.
func (m MarkerTool) Name() string { return "marker" }

// Description returns the tool description.
func (m MarkerTool) Description() string { return "records that it ran" }

// Parameters returns the (empty-object) JSON Schema for the tool input.
func (m MarkerTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}

// IsConcurrencySafe reports false: the marker mutates state.
func (m MarkerTool) IsConcurrencySafe(_ json.RawMessage) bool { return false }

// Execute inserts a marker row, optionally crashes (at-least-once demo), then
// returns a fixed result.
func (m MarkerTool) Execute(ctx context.Context, _ json.RawMessage) (tool.ToolResult, error) {
	if _, err := m.DB.ExecContext(ctx, "INSERT INTO markers (ran_at) VALUES (now())"); err != nil {
		return tool.ToolResult{}, err
	}

	if os.Getenv("CRASH_AFTER_MARKER") == "1" {
		// Crash after the committed side effect, before the turn checkpoints.
		// Sleep briefly so the POST /sessions HTTP response (carrying the
		// session id) flushes to the client before this process dies — the
		// workflow runs concurrently with the response write.
		time.Sleep(300 * time.Millisecond) // let the POST /sessions response flush before we die
		os.Exit(1)
	}

	return tool.ToolResult{Output: "marked"}, nil
}

// SleepTool blocks for TESTAGENT_SLEEP_MS milliseconds (default 5000) or until
// ctx is cancelled. Used by integration tests to trip turn_timeout: RunTurn's
// context deadline cancels ctx, the tool returns, and the turn errors with
// context.DeadlineExceeded.
type SleepTool struct{}

// Name returns the tool name.
func (SleepTool) Name() string { return "sleep" }

// Description returns the tool description.
func (SleepTool) Description() string { return "sleeps; used to test turn timeouts" }

// Parameters returns the (empty-object) JSON Schema for the tool input.
func (SleepTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}

// IsConcurrencySafe reports true: sleeping mutates nothing.
func (SleepTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }

// Execute sleeps for TESTAGENT_SLEEP_MS milliseconds (default 5000) or until
// ctx is cancelled, whichever comes first.
func (SleepTool) Execute(ctx context.Context, _ json.RawMessage) (tool.ToolResult, error) {
	ms := 5000
	if v := os.Getenv("TESTAGENT_SLEEP_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			ms = n
		}
	}
	select {
	case <-time.After(time.Duration(ms) * time.Millisecond):
		return tool.ToolResult{Output: "slept"}, nil
	case <-ctx.Done():
		return tool.ToolResult{}, ctx.Err()
	}
}
