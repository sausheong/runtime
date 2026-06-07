package testagent

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"

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
		os.Exit(1)
	}

	return tool.ToolResult{Output: "marked"}, nil
}
