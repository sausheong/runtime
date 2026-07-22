package browser

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestServerToolsRegistered(t *testing.T) {
	m := NewManager(NewFakeBackend(), Config{})
	srv := NewServer(m, false)
	if srv == nil {
		t.Fatal("nil server")
	}
}

func TestPopTenantStripAndDefault(t *testing.T) {
	tn, present, rest, err := popTenant(json.RawMessage(`{"__rt_tenant":"acme","x":1}`))
	if err != nil || tn != "acme" || !present {
		t.Fatalf("tenant=%q present=%v err=%v", tn, present, err)
	}
	var got map[string]any
	_ = json.Unmarshal(rest, &got)
	if _, leaked := got["__rt_tenant"]; leaked {
		t.Fatal("__rt_tenant not stripped")
	}
	tn2, present2, _, _ := popTenant(json.RawMessage(`{}`))
	if present2 || tn2 != "default" {
		t.Fatalf("absent key: tenant=%q present=%v", tn2, present2)
	}
}

// startServerManager wraps a caller-supplied Manager in the MCP server and
// connects an in-memory client session to it. Used by tests that need to
// inspect the Manager (e.g. session-scoped teardown) after driving tools.
func startServerManager(t *testing.T, m *Manager, allowDirect bool) *sdk.ClientSession {
	t.Helper()
	srv := NewServer(m, allowDirect)

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
// from its single TextContent (nil when the text is not a JSON object).
func call(t *testing.T, sess *sdk.ClientSession, name string, args map[string]any) (*sdk.CallToolResult, map[string]any) {
	t.Helper()
	res, err := sess.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	var parsed map[string]any
	if len(res.Content) > 0 {
		if tc, ok := res.Content[0].(*sdk.TextContent); ok {
			_ = json.Unmarshal([]byte(tc.Text), &parsed)
		}
	}
	return res, parsed
}

func text(t *testing.T, res *sdk.CallToolResult) string {
	t.Helper()
	return res.Content[0].(*sdk.TextContent).Text
}

// TestAbsentTenantFailsClosed pins the misconfiguration guard at the handler
// level: without the gateway-injected __rt_tenant key (and without allowDirect),
// a tool call must fail closed rather than silently merge into tenant "default".
// The guard fires in the add() wrapper before any backend interaction, so the
// fake backend's lack of a CDP endpoint is irrelevant here.
func TestAbsentTenantFailsClosed(t *testing.T) {
	m := NewManager(NewFakeBackend(), Config{})
	sess := startServerManager(t, m, false)

	res, _ := call(t, sess, "create_browser", map[string]any{})
	if !res.IsError {
		t.Fatal("create_browser without __rt_tenant should be IsError")
	}
	msg := text(t, res)
	if !strings.Contains(msg, "missing gateway tenant") {
		t.Fatalf("absent-tenant error %q should contain %q", msg, "missing gateway tenant")
	}
}

func TestCreateBrowserToolFakeBackend(t *testing.T) {
	m := NewManager(NewFakeBackend(), Config{})
	srv := NewServer(m, true)
	_ = srv
	// Direct Manager check stands in for the create_browser handler path
	// (the fake backend has no CDP endpoint, so action tools can't run, but
	// create/list/close lifecycle works).
	s, err := m.Create(context.Background(), "default", "")
	if err != nil {
		t.Fatal(err)
	}
	if s.ID == "" {
		t.Fatal("empty id")
	}
}

// TestToolsCloseSessionReapsSessionBrowsers drives the close_session tool
// against a session-scoped manager: a browser created under __rt_session "s1"
// must be gone from the manager after close_session, while a same-tenant
// browser under a different session survives.
func TestToolsCloseSessionReapsSessionBrowsers(t *testing.T) {
	m := NewManager(NewFakeBackend(), Config{MaxPerTenant: 5, SessionScoped: true})
	sess := startServerManager(t, m, false)

	// Browser in session s1.
	res, out := call(t, sess, "create_browser", map[string]any{
		"__rt_tenant": "acme", "__rt_session": "s1",
	})
	if res.IsError {
		t.Fatalf("create_browser (s1) errored: %s", text(t, res))
	}
	if id1, _ := out["browser_id"].(string); id1 == "" {
		t.Fatalf("create_browser (s1) missing browser_id: %v", out)
	}
	// Browser in session s2 (same tenant).
	res, out = call(t, sess, "create_browser", map[string]any{
		"__rt_tenant": "acme", "__rt_session": "s2",
	})
	if res.IsError {
		t.Fatalf("create_browser (s2) errored: %s", text(t, res))
	}
	if id2, _ := out["browser_id"].(string); id2 == "" {
		t.Fatalf("create_browser (s2) missing browser_id: %v", out)
	}

	if got := len(m.List("acme", "s1")); got != 1 {
		t.Fatalf("s1 should see 1 browser before close, got %d", got)
	}

	// close_session for s1.
	res, out = call(t, sess, "close_session", map[string]any{
		"__rt_tenant": "acme", "__rt_session": "s1",
	})
	if res.IsError {
		t.Fatalf("close_session errored: %s", text(t, res))
	}
	if closed, _ := out["closed"].(bool); !closed {
		t.Fatalf("close_session closed = %v, want true", out["closed"])
	}

	// s1's browser is gone; s2's browser survives.
	if got := len(m.List("acme", "s1")); got != 0 {
		t.Fatalf("s1 should see 0 browsers after close_session, got %d", got)
	}
	if got := len(m.List("acme", "s2")); got != 1 {
		t.Fatalf("s2 browser should survive s1 close_session, got %d", got)
	}

	// Idempotent: closing s1 again still succeeds.
	res, _ = call(t, sess, "close_session", map[string]any{
		"__rt_tenant": "acme", "__rt_session": "s1",
	})
	if res.IsError {
		t.Fatalf("second close_session should succeed (idempotent): %s", text(t, res))
	}
}

// TestToolsSessionScopedListIsolation confirms that, with SessionScoped, a
// browser minted in one session is invisible to list_browsers in another.
func TestToolsSessionScopedListIsolation(t *testing.T) {
	m := NewManager(NewFakeBackend(), Config{MaxPerTenant: 5, SessionScoped: true})
	sess := startServerManager(t, m, false)

	res, _ := call(t, sess, "create_browser", map[string]any{
		"__rt_tenant": "acme", "__rt_session": "s1",
	})
	if res.IsError {
		t.Fatalf("create_browser (s1) errored: %s", text(t, res))
	}

	_, out := call(t, sess, "list_browsers", map[string]any{
		"__rt_tenant": "acme", "__rt_session": "s1",
	})
	if bs, _ := out["browsers"].([]any); len(bs) != 1 {
		t.Fatalf("s1 list should see 1 browser, got %v", out["browsers"])
	}
	_, out = call(t, sess, "list_browsers", map[string]any{
		"__rt_tenant": "acme", "__rt_session": "s2",
	})
	if bs, _ := out["browsers"].([]any); len(bs) != 0 {
		t.Fatalf("s2 list should see 0 browsers, got %v", out["browsers"])
	}
}
