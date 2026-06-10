package sandbox

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// startServer builds a Manager over the fake backend, wraps it in the MCP
// server, and connects an in-memory client session to it.
func startServer(t *testing.T) *sdk.ClientSession {
	t.Helper()
	m := NewManager(NewFakeBackend(), Config{MaxPerTenant: 2})
	srv := NewServer(m)

	ct, st := sdk.NewInMemoryTransports()
	ss, err := srv.Connect(context.Background(), st, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })

	cli := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "v0"}, nil)
	sess, err := cli.Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

// call invokes a tool and returns the raw result plus the parsed JSON object
// from its single TextContent. parsed is nil for error results whose text is
// not JSON.
func call(t *testing.T, sess *sdk.ClientSession, name string, args map[string]any) (*sdk.CallToolResult, map[string]any) {
	t.Helper()
	res, err := sess.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      name,
		Arguments: args,
	})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	if len(res.Content) != 1 {
		t.Fatalf("call %s: want exactly 1 content item, got %d", name, len(res.Content))
	}
	tc, ok := res.Content[0].(*sdk.TextContent)
	if !ok {
		t.Fatalf("call %s: content is %T, want *sdk.TextContent", name, res.Content[0])
	}
	var parsed map[string]any
	_ = json.Unmarshal([]byte(tc.Text), &parsed)
	return res, parsed
}

// text returns the single TextContent text of a result.
func text(t *testing.T, res *sdk.CallToolResult) string {
	t.Helper()
	return res.Content[0].(*sdk.TextContent).Text
}

func TestToolsListExactlySeven(t *testing.T) {
	sess := startServer(t)
	res, err := sess.ListTools(context.Background(), &sdk.ListToolsParams{})
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	var got []string
	for _, tl := range res.Tools {
		got = append(got, tl.Name)
	}
	sort.Strings(got)
	want := []string{
		"close_sandbox", "create_sandbox", "execute_code", "list_sandboxes",
		"read_file", "run_command", "write_file",
	}
	if len(got) != len(want) {
		t.Fatalf("want %d tools, got %v", len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tool names = %v, want %v", got, want)
		}
	}
}

func TestToolsLifecycle(t *testing.T) {
	sess := startServer(t)
	acme := map[string]any{"__rt_tenant": "acme"}

	// create_sandbox
	res, out := call(t, sess, "create_sandbox", acme)
	if res.IsError {
		t.Fatalf("create_sandbox errored: %s", text(t, res))
	}
	id, _ := out["sandbox_id"].(string)
	if !strings.HasPrefix(id, "sbx-") {
		t.Fatalf("sandbox_id = %q, want sbx- prefix", id)
	}
	if exp, _ := out["expires_at"].(string); exp == "" {
		t.Fatalf("create_sandbox missing expires_at: %v", out)
	}

	// execute_code
	res, out = call(t, sess, "execute_code", map[string]any{
		"__rt_tenant": "acme", "sandbox_id": id, "code": "print(1)",
	})
	if res.IsError {
		t.Fatalf("execute_code errored: %s", text(t, res))
	}
	if stdout, _ := out["stdout"].(string); !strings.Contains(stdout, "python3") {
		t.Fatalf("execute_code stdout = %q, want python3 mention", stdout)
	}
	if _, ok := out["exit_code"]; !ok {
		t.Fatalf("execute_code missing exit_code: %v", out)
	}

	// run_command
	res, out = call(t, sess, "run_command", map[string]any{
		"__rt_tenant": "acme", "sandbox_id": id, "command": "ls /workspace",
	})
	if res.IsError {
		t.Fatalf("run_command errored: %s", text(t, res))
	}
	if stdout, _ := out["stdout"].(string); !strings.Contains(stdout, "sh -c") {
		t.Fatalf("run_command stdout = %q, want sh -c mention", stdout)
	}

	// write_file → read_file round trip
	res, out = call(t, sess, "write_file", map[string]any{
		"__rt_tenant": "acme", "sandbox_id": id, "path": "data.txt", "content": "hi",
	})
	if res.IsError {
		t.Fatalf("write_file errored: %s", text(t, res))
	}
	if b, _ := out["bytes"].(float64); b != 2 {
		t.Fatalf("write_file bytes = %v, want 2", out["bytes"])
	}
	res, out = call(t, sess, "read_file", map[string]any{
		"__rt_tenant": "acme", "sandbox_id": id, "path": "data.txt",
	})
	if res.IsError {
		t.Fatalf("read_file errored: %s", text(t, res))
	}
	if c, _ := out["content"].(string); c != "hi" {
		t.Fatalf("read_file content = %q, want \"hi\"", out["content"])
	}
	if tr, _ := out["truncated"].(bool); tr {
		t.Fatalf("read_file truncated = true, want false")
	}

	// list_sandboxes: 1 for acme, 0 for globex
	res, out = call(t, sess, "list_sandboxes", acme)
	if res.IsError {
		t.Fatalf("list_sandboxes errored: %s", text(t, res))
	}
	boxes, ok := out["sandboxes"].([]any)
	if !ok {
		t.Fatalf("list_sandboxes sandboxes is %T, want array (never null)", out["sandboxes"])
	}
	if len(boxes) != 1 {
		t.Fatalf("acme should see 1 sandbox, got %d", len(boxes))
	}
	first := boxes[0].(map[string]any)
	for _, k := range []string{"sandbox_id", "created_at", "expires_at", "last_used_at"} {
		if _, ok := first[k]; !ok {
			t.Fatalf("list_sandboxes entry missing %q: %v", k, first)
		}
	}
	res, out = call(t, sess, "list_sandboxes", map[string]any{"__rt_tenant": "globex"})
	if res.IsError {
		t.Fatalf("list_sandboxes (globex) errored: %s", text(t, res))
	}
	boxes, ok = out["sandboxes"].([]any)
	if !ok {
		t.Fatalf("globex sandboxes is %T, want [] not null", out["sandboxes"])
	}
	if len(boxes) != 0 {
		t.Fatalf("globex should see 0 sandboxes, got %d", len(boxes))
	}

	// cross-tenant: globex using acme's id must be indistinguishable from a
	// nonexistent id.
	resForeign, _ := call(t, sess, "execute_code", map[string]any{
		"__rt_tenant": "globex", "sandbox_id": id, "code": "print(1)",
	})
	if !resForeign.IsError {
		t.Fatalf("cross-tenant execute_code should be IsError")
	}
	resNope, _ := call(t, sess, "execute_code", map[string]any{
		"__rt_tenant": "globex", "sandbox_id": "sbx-nope", "code": "print(1)",
	})
	if !resNope.IsError {
		t.Fatalf("nonexistent-id execute_code should be IsError")
	}
	if a, b := text(t, resForeign), text(t, resNope); a != b {
		t.Fatalf("existence leak: foreign-id error %q != unknown-id error %q", a, b)
	}

	// close_sandbox
	res, out = call(t, sess, "close_sandbox", map[string]any{
		"__rt_tenant": "acme", "sandbox_id": id,
	})
	if res.IsError {
		t.Fatalf("close_sandbox errored: %s", text(t, res))
	}
	if closed, _ := out["closed"].(bool); !closed {
		t.Fatalf("close_sandbox closed = %v, want true", out["closed"])
	}
	// Idempotent.
	res, out = call(t, sess, "close_sandbox", map[string]any{
		"__rt_tenant": "acme", "sandbox_id": id,
	})
	if res.IsError {
		t.Fatalf("second close_sandbox should succeed (idempotent): %s", text(t, res))
	}
	if closed, _ := out["closed"].(bool); !closed {
		t.Fatalf("second close_sandbox closed = %v, want true", out["closed"])
	}
}

func TestToolsPathEscapeIsError(t *testing.T) {
	sess := startServer(t)
	_, out := call(t, sess, "create_sandbox", map[string]any{"__rt_tenant": "acme"})
	id := out["sandbox_id"].(string)

	res, _ := call(t, sess, "write_file", map[string]any{
		"__rt_tenant": "acme", "sandbox_id": id, "path": "../escape", "content": "x",
	})
	if !res.IsError {
		t.Fatalf("write_file with escaping path should be IsError")
	}
	if msg := text(t, res); !strings.Contains(msg, "outside") {
		t.Fatalf("escape error %q should mention confinement", msg)
	}
}

func TestToolsMissingArgsIsError(t *testing.T) {
	sess := startServer(t)

	res, _ := call(t, sess, "execute_code", map[string]any{
		"__rt_tenant": "acme", "code": "print(1)",
	})
	if !res.IsError {
		t.Fatalf("execute_code without sandbox_id should be IsError")
	}
	if msg := text(t, res); !strings.Contains(msg, "missing required argument") {
		t.Fatalf("missing-arg error %q should say missing required argument(s)", msg)
	}

	res, _ = call(t, sess, "execute_code", map[string]any{
		"__rt_tenant": "acme", "sandbox_id": "sbx-x",
	})
	if !res.IsError {
		t.Fatalf("execute_code without code should be IsError")
	}
}

func TestToolsReadFileMissingPathSaysNoSuchFile(t *testing.T) {
	sess := startServer(t)
	_, out := call(t, sess, "create_sandbox", map[string]any{"__rt_tenant": "acme"})
	id := out["sandbox_id"].(string)

	res, _ := call(t, sess, "read_file", map[string]any{
		"__rt_tenant": "acme", "sandbox_id": id, "path": "missing.txt",
	})
	if !res.IsError {
		t.Fatalf("read_file of missing path should be IsError")
	}
	if msg := text(t, res); !strings.Contains(msg, "no such file") {
		t.Fatalf("missing-file error %q should contain %q, not the generic message", msg, "no such file")
	}
}
