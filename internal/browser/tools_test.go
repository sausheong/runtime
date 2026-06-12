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

// TestAbsentTenantFailsClosed pins the misconfiguration guard at the handler
// level: without the gateway-injected __rt_tenant key (and without allowDirect),
// a tool call must fail closed rather than silently merge into tenant "default".
// The guard fires in the add() wrapper before any backend interaction, so the
// fake backend's lack of a CDP endpoint is irrelevant here.
func TestAbsentTenantFailsClosed(t *testing.T) {
	m := NewManager(NewFakeBackend(), Config{})
	srv := NewServer(m, false)

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

	res, err := sess.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "create_browser",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("call create_browser: %v", err)
	}
	if !res.IsError {
		t.Fatal("create_browser without __rt_tenant should be IsError")
	}
	msg := res.Content[0].(*sdk.TextContent).Text
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
	s, err := m.Create(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	if s.ID == "" {
		t.Fatal("empty id")
	}
}
