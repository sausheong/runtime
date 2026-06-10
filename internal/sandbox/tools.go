package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// persistNote is appended to tool descriptions that touch sandbox state, so
// the model knows what persists between calls and what does not.
const persistNote = " Files in /workspace persist across calls within the same sandbox; Python variables do NOT (each execution is a fresh process) — write intermediate results to files."

// errMissing is the uniform missing-required-argument error.
type errMissing string

func (e errMissing) Error() string {
	return "missing required argument(s): " + string(e)
}

// decode unmarshals raw tool arguments into v; empty/absent arguments are
// fine (v keeps its zero values).
func decode(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, v)
}

// errResult builds the MCP-level tool error: IsError with one text part.
// Tool failures are never Go errors — those would kill the protocol call.
func errResult(msg string) *sdk.CallToolResult {
	return &sdk.CallToolResult{
		IsError: true,
		Content: []sdk.Content{&sdk.TextContent{Text: msg}},
	}
}

// execOut shapes an ExecResult for the model.
func execOut(res ExecResult) map[string]any {
	return map[string]any{
		"stdout":      res.Stdout,
		"stderr":      res.Stderr,
		"exit_code":   res.ExitCode,
		"timed_out":   res.TimedOut,
		"duration_ms": res.Duration.Milliseconds(),
	}
}

// NewServer builds the sandboxd MCP server: the 7 sandbox tools over m.
// Tool names are unprefixed — the gateway namespaces them (sandbox__*).
// Every handler first pops the reserved __rt_tenant argument the gateway
// injects (present-but-empty ⇒ "default", the gateway's open mode), so
// tenancy never appears in any schema and the model can never choose its
// own tenant.
//
// When allowDirect is false, an ABSENT __rt_tenant key fails closed: the
// gateway always injects the key for forward_tenant upstreams (open mode
// injects ""), so absence means a non-forwarding upstream or a direct
// caller — silently mapping everyone to "default" there would merge tenants.
// Honest caveat: this guard catches misconfiguration loudly on the honest
// path; an adversarial agent calling through a NON-forwarding upstream can
// still set the key itself — forward_tenant: true at the gateway is the
// actual security boundary (see the design spec).
func NewServer(m *Manager, allowDirect bool) *sdk.Server {
	srv := sdk.NewServer(&sdk.Implementation{Name: "runtime-sandbox", Version: "m1"}, nil)

	add := func(name, desc, schema string, h func(ctx context.Context, tenant string, args json.RawMessage) (any, error)) {
		srv.AddTool(&sdk.Tool{
			Name:        name,
			Description: desc,
			InputSchema: json.RawMessage(schema),
		}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			tenant, present, rest, err := popTenant(req.Params.Arguments)
			if err != nil {
				return errResult("invalid arguments: " + err.Error()), nil
			}
			if !present && !allowDirect {
				return errResult("missing gateway tenant: sandboxd must be served behind the platform gateway with forward_tenant: true (or set RUNTIME_SANDBOX_ALLOW_DIRECT=1 for single-tenant direct use)"), nil
			}
			out, err := h(ctx, tenant, rest)
			if err != nil {
				return errResult(err.Error()), nil
			}
			b, err := json.Marshal(out)
			if err != nil {
				return errResult("internal: marshal result: " + err.Error()), nil
			}
			return &sdk.CallToolResult{
				Content: []sdk.Content{&sdk.TextContent{Text: string(b)}},
			}, nil
		})
	}

	add("create_sandbox",
		"Create an isolated code-execution sandbox (Python 3.12 + numpy/pandas/matplotlib; no network access). Returns a sandbox_id for use with the other sandbox tools."+persistNote,
		`{"type":"object","properties":{}}`,
		func(ctx context.Context, tenant string, _ json.RawMessage) (any, error) {
			s, err := m.Create(ctx, tenant)
			if err != nil {
				return nil, err
			}
			return map[string]any{
				"sandbox_id": s.ID,
				"expires_at": s.ExpiresAt.Format(time.RFC3339),
			}, nil
		})

	add("execute_code",
		"Execute Python code in the sandbox (python3 -c, working directory /workspace). Returns stdout, stderr and exit_code."+persistNote,
		`{"type":"object","properties":{
			"sandbox_id":{"type":"string","description":"id from create_sandbox"},
			"code":{"type":"string","description":"Python source to execute"},
			"timeout_s":{"type":"integer","description":"wall-clock timeout in seconds (default 30, max 120)"}
		},"required":["sandbox_id","code"]}`,
		func(ctx context.Context, tenant string, raw json.RawMessage) (any, error) {
			var a struct {
				SandboxID string `json:"sandbox_id"`
				Code      string `json:"code"`
				TimeoutS  int    `json:"timeout_s"`
			}
			if err := decode(raw, &a); err != nil {
				return nil, fmt.Errorf("invalid arguments: %w", err)
			}
			if a.SandboxID == "" || a.Code == "" {
				return nil, errMissing("sandbox_id, code")
			}
			res, err := m.ExecCode(ctx, tenant, a.SandboxID, a.Code, a.TimeoutS)
			if err != nil {
				return nil, err
			}
			return execOut(res), nil
		})

	add("run_command",
		"Run a shell command in the sandbox (sh -c, working directory /workspace). Returns stdout, stderr and exit_code."+persistNote,
		`{"type":"object","properties":{
			"sandbox_id":{"type":"string","description":"id from create_sandbox"},
			"command":{"type":"string","description":"shell command to run"},
			"timeout_s":{"type":"integer","description":"wall-clock timeout in seconds (default 30, max 120)"}
		},"required":["sandbox_id","command"]}`,
		func(ctx context.Context, tenant string, raw json.RawMessage) (any, error) {
			var a struct {
				SandboxID string `json:"sandbox_id"`
				Command   string `json:"command"`
				TimeoutS  int    `json:"timeout_s"`
			}
			if err := decode(raw, &a); err != nil {
				return nil, fmt.Errorf("invalid arguments: %w", err)
			}
			if a.SandboxID == "" || a.Command == "" {
				return nil, errMissing("sandbox_id, command")
			}
			res, err := m.ExecCommand(ctx, tenant, a.SandboxID, a.Command, a.TimeoutS)
			if err != nil {
				return nil, err
			}
			return execOut(res), nil
		})

	add("write_file",
		"Write a file inside the sandbox. Paths are relative to /workspace (absolute paths must stay inside it); parent directories are created."+persistNote,
		`{"type":"object","properties":{
			"sandbox_id":{"type":"string","description":"id from create_sandbox"},
			"path":{"type":"string","description":"file path under /workspace"},
			"content":{"type":"string","description":"file content"}
		},"required":["sandbox_id","path","content"]}`,
		func(ctx context.Context, tenant string, raw json.RawMessage) (any, error) {
			var a struct {
				SandboxID string  `json:"sandbox_id"`
				Path      string  `json:"path"`
				Content   *string `json:"content"`
			}
			if err := decode(raw, &a); err != nil {
				return nil, fmt.Errorf("invalid arguments: %w", err)
			}
			// content may legitimately be the empty string; only its absence
			// is an error.
			if a.SandboxID == "" || a.Path == "" || a.Content == nil {
				return nil, errMissing("sandbox_id, path, content")
			}
			if err := m.WriteFile(ctx, tenant, a.SandboxID, a.Path, []byte(*a.Content)); err != nil {
				return nil, err
			}
			return map[string]any{"path": a.Path, "bytes": len(*a.Content)}, nil
		})

	add("read_file",
		"Read a file from the sandbox. Paths are relative to /workspace (absolute paths must stay inside it). Content is capped at 256 KiB; truncated reports whether the cap was hit."+persistNote,
		`{"type":"object","properties":{
			"sandbox_id":{"type":"string","description":"id from create_sandbox"},
			"path":{"type":"string","description":"file path under /workspace"}
		},"required":["sandbox_id","path"]}`,
		func(ctx context.Context, tenant string, raw json.RawMessage) (any, error) {
			var a struct {
				SandboxID string `json:"sandbox_id"`
				Path      string `json:"path"`
			}
			if err := decode(raw, &a); err != nil {
				return nil, fmt.Errorf("invalid arguments: %w", err)
			}
			if a.SandboxID == "" || a.Path == "" {
				return nil, errMissing("sandbox_id, path")
			}
			content, truncated, err := m.ReadFile(ctx, tenant, a.SandboxID, a.Path)
			if err != nil {
				return nil, err
			}
			return map[string]any{
				"content":   string(content),
				"bytes":     len(content),
				"truncated": truncated,
			}, nil
		})

	add("list_sandboxes",
		"List your live sandboxes with their creation, last-use and expiry times.",
		`{"type":"object","properties":{}}`,
		func(_ context.Context, tenant string, _ json.RawMessage) (any, error) {
			sessions := m.List(tenant)
			boxes := make([]map[string]any, 0, len(sessions)) // [] not null
			for _, s := range sessions {
				boxes = append(boxes, map[string]any{
					"sandbox_id":   s.ID,
					"created_at":   s.CreatedAt.Format(time.RFC3339),
					"expires_at":   s.ExpiresAt.Format(time.RFC3339),
					"last_used_at": s.LastUsed.Format(time.RFC3339),
				})
			}
			return map[string]any{"sandboxes": boxes}, nil
		})

	add("close_sandbox",
		"Close a sandbox and discard its /workspace contents. Idempotent: closing an already-closed sandbox succeeds.",
		`{"type":"object","properties":{
			"sandbox_id":{"type":"string","description":"id from create_sandbox"}
		},"required":["sandbox_id"]}`,
		func(ctx context.Context, tenant string, raw json.RawMessage) (any, error) {
			var a struct {
				SandboxID string `json:"sandbox_id"`
			}
			if err := decode(raw, &a); err != nil {
				return nil, fmt.Errorf("invalid arguments: %w", err)
			}
			if a.SandboxID == "" {
				return nil, errMissing("sandbox_id")
			}
			if err := m.Close(ctx, tenant, a.SandboxID); err != nil {
				return nil, err
			}
			return map[string]any{"closed": true}, nil
		})

	return srv
}
