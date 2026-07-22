package agentruntime

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sausheong/harness/tool"

	"github.com/sausheong/runtime/internal/identity"
)

// recordingTool is a minimal tool.Tool for exercising closeSessionSandboxes.
// Its Execute delegates to exec, letting a test capture what it saw on ctx.
type recordingTool struct {
	name string
	exec func(ctx context.Context, input json.RawMessage) (tool.ToolResult, error)
}

func (t recordingTool) Name() string                { return t.name }
func (t recordingTool) Description() string          { return "recording test tool" }
func (t recordingTool) Parameters() json.RawMessage  { return json.RawMessage(`{"type":"object"}`) }
func (t recordingTool) Execute(ctx context.Context, input json.RawMessage) (tool.ToolResult, error) {
	return t.exec(ctx, input)
}
func (t recordingTool) IsConcurrencySafe(json.RawMessage) bool { return false }

func TestCloseSessionSandboxesBestEffort(t *testing.T) {
	// A registry with a recording fake tool at the sandbox close_session name.
	reg := tool.NewRegistry()
	var gotSession string
	reg.Register(recordingTool{
		name: "mcp__gateway__sandbox__close_session",
		exec: func(ctx context.Context, _ json.RawMessage) (tool.ToolResult, error) {
			gotSession = identity.SessionFrom(ctx)
			return tool.ToolResult{}, nil
		},
	})
	m := &Manager{cfg: Config{Tools: reg}}
	m.closeSessionSandboxes("sess-42")
	if gotSession != "sess-42" {
		t.Fatalf("close_session saw session %q, want sess-42", gotSession)
	}
}

func TestCloseSessionSandboxesNoToolIsNoop(t *testing.T) {
	m := &Manager{cfg: Config{Tools: tool.NewRegistry()}} // no gateway tools
	m.closeSessionSandboxes("sess-42")                    // must not panic
}
