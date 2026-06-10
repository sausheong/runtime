package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sausheong/harness/tool"

	"github.com/sausheong/runtime/internal/config"
	"github.com/sausheong/runtime/internal/identity"
)

// startManager spins up a Manager over fake upstreams and waits until all are up.
func startManager(t *testing.T, servers []config.GatewayServer, conns map[string]*fakeConn) *Manager {
	t.Helper()
	m := NewManager(servers, WithDial(scriptDial(conns, nil)), WithBackoff(10*time.Millisecond, 50*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m.Start(ctx)
	waitFor(t, 2*time.Second, func() bool { return len(m.AllTools()) == totalTools(conns) })
	return m
}

func totalTools(conns map[string]*fakeConn) int {
	n := 0
	for _, c := range conns {
		n += len(c.tools)
	}
	return n
}

// dialGateway connects an SDK MCP client to the gateway's HTTP handler with
// the given principal injected (nil principal ⇒ open mode).
func dialGateway(t *testing.T, h *Handler, p *identity.Principal) *sdk.ClientSession {
	t.Helper()
	h.PrincipalFor = func(_ context.Context) (identity.Principal, bool) {
		if p == nil {
			return identity.Principal{}, false
		}
		return *p, true
	}
	srv := httptest.NewServer(h.HTTP())
	t.Cleanup(srv.Close)
	cli := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "v0"}, nil)
	sess, err := cli.Connect(context.Background(),
		&sdk.StreamableClientTransport{Endpoint: srv.URL}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

func listNames(t *testing.T, sess *sdk.ClientSession) []string {
	t.Helper()
	res, err := sess.ListTools(context.Background(), &sdk.ListToolsParams{})
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	return names
}

func gwServers() []config.GatewayServer {
	return []config.GatewayServer{
		{Name: "open", Command: "x"},
		{Name: "scoped", Command: "x", Tenants: []string{"acme"}},
	}
}

func gwConns() map[string]*fakeConn {
	return map[string]*fakeConn{
		"open":   {tools: []tool.Tool{fakeTool{name: "mcp__open__echo", out: "hi"}}},
		"scoped": {tools: []tool.Tool{fakeTool{name: "mcp__scoped__secret", out: "s3"}}},
	}
}

func TestServerOpenModeSeesAll(t *testing.T) {
	m := startManager(t, gwServers(), gwConns())
	h := NewHandler(m)
	sess := dialGateway(t, h, nil) // open mode
	if names := listNames(t, sess); len(names) != 2 {
		t.Fatalf("open mode should list 2 tools, got %v", names)
	}
}

func TestServerTenantFiltered(t *testing.T) {
	m := startManager(t, gwServers(), gwConns())
	h := NewHandler(m)
	acme := dialGateway(t, h, &identity.Principal{TenantID: "acme", Role: identity.RoleOperator})
	if names := listNames(t, acme); len(names) != 2 {
		t.Fatalf("acme should list 2, got %v", names)
	}
}

func TestServerOtherTenantHidden(t *testing.T) {
	m := startManager(t, gwServers(), gwConns())
	h := NewHandler(m)
	globex := dialGateway(t, h, &identity.Principal{TenantID: "globex", Role: identity.RoleOperator})
	names := listNames(t, globex)
	if len(names) != 1 || names[0] != "open__echo" {
		t.Fatalf("globex should list only open__echo, got %v", names)
	}
	// Calling the hidden tool: tool-not-found error, not forbidden.
	_, err := globex.CallTool(context.Background(), &sdk.CallToolParams{Name: "scoped__secret"})
	if err == nil || !strings.Contains(err.Error(), "scoped__secret") {
		t.Fatalf("expected tool-not-found error, got %v", err)
	}
}

func TestServerSuperuserSeesAll(t *testing.T) {
	m := startManager(t, gwServers(), gwConns())
	h := NewHandler(m)
	su := dialGateway(t, h, &identity.Principal{TenantID: "default", Role: identity.RoleAdmin, Superuser: true})
	if names := listNames(t, su); len(names) != 2 {
		t.Fatalf("superuser should list 2, got %v", names)
	}
}

func TestServerCallToolRoundTrip(t *testing.T) {
	m := startManager(t, gwServers(), gwConns())
	h := NewHandler(m)
	sess := dialGateway(t, h, &identity.Principal{TenantID: "acme", Role: identity.RoleOperator})
	res, err := sess.CallTool(context.Background(), &sdk.CallToolParams{
		Name: "open__echo", Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError: %+v", res.Content)
	}
	txt, ok := res.Content[0].(*sdk.TextContent)
	if !ok || txt.Text != "hi" {
		t.Fatalf("want text 'hi', got %+v", res.Content[0])
	}
}

func TestServerToolErrorBecomesIsError(t *testing.T) {
	conns := map[string]*fakeConn{
		"open": {tools: []tool.Tool{fakeTool{name: "mcp__open__boom", err: "kaput"}}},
	}
	m := startManager(t, []config.GatewayServer{{Name: "open", Command: "x"}}, conns)
	h := NewHandler(m)
	sess := dialGateway(t, h, &identity.Principal{TenantID: "t", Role: identity.RoleOperator})
	res, err := sess.CallTool(context.Background(), &sdk.CallToolParams{Name: "open__boom"})
	if err != nil {
		t.Fatalf("transport error, want isError result: %v", err)
	}
	if !res.IsError {
		t.Fatal("want IsError=true")
	}
	txt := res.Content[0].(*sdk.TextContent)
	if !strings.Contains(txt.Text, "kaput") {
		t.Fatalf("error text lost: %q", txt.Text)
	}
}

func TestServerViewerCannotCall(t *testing.T) {
	m := startManager(t, gwServers(), gwConns())
	h := NewHandler(m)
	viewer := dialGateway(t, h, &identity.Principal{TenantID: "acme", Role: identity.RoleViewer})
	if names := listNames(t, viewer); len(names) != 2 {
		t.Fatalf("viewer should list 2, got %v", names)
	}
	res, err := viewer.CallTool(context.Background(), &sdk.CallToolParams{Name: "open__echo"})
	if err != nil {
		t.Fatalf("expected isError result, got transport error: %v", err)
	}
	if !res.IsError {
		t.Fatal("viewer call should be IsError")
	}
}

func TestServerRebuildsOnGenerationChange(t *testing.T) {
	// Each server's dial succeeds exactly once; later dials fail. This keeps
	// the upstream DOWN after markDown — otherwise the supervise loop could
	// redial within its 10-50ms backoff and the second session would see the
	// full tool set again (flake).
	conns := gwConns()
	var mu sync.Mutex
	dialed := map[string]bool{}
	dial := func(_ context.Context, s config.GatewayServer) (upstreamConn, error) {
		mu.Lock()
		defer mu.Unlock()
		if dialed[s.Name] {
			return nil, errors.New("scripted: no redial")
		}
		dialed[s.Name] = true
		return conns[s.Name], nil
	}
	m := NewManager(gwServers(), WithDial(dial), WithBackoff(10*time.Millisecond, 50*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m.Start(ctx)
	waitFor(t, 2*time.Second, func() bool { return len(m.AllTools()) == totalTools(conns) })

	h := NewHandler(m)
	sess := dialGateway(t, h, &identity.Principal{TenantID: "acme", Role: identity.RoleOperator})
	if names := listNames(t, sess); len(names) != 2 {
		t.Fatalf("pre: want 2, got %v", names)
	}
	// Simulate an upstream going down: markDown bumps generation; a NEW MCP
	// session must see the reduced tool set.
	u := m.ups[1]
	u.mu.Lock()
	observed := u.conn
	u.mu.Unlock()
	m.markDown(u, observed, context.DeadlineExceeded)
	sess2 := dialGateway(t, h, &identity.Principal{TenantID: "acme", Role: identity.RoleOperator})
	if names := listNames(t, sess2); len(names) != 1 {
		t.Fatalf("post-down: want 1, got %v", names)
	}
}

func TestStatusHandler(t *testing.T) {
	m := startManager(t, gwServers(), gwConns())
	h := NewHandler(m)
	h.PrincipalFor = func(_ context.Context) (identity.Principal, bool) {
		return identity.Principal{TenantID: "globex", Role: identity.RoleOperator}, true
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/gateway/status", nil)
	h.Status(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status code %d", rec.Code)
	}
	var rows []UpstreamStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Name != "open" {
		t.Fatalf("globex status rows wrong: %+v", rows)
	}
}

func TestStatusHandlerViewerForbidden(t *testing.T) {
	m := startManager(t, gwServers(), gwConns())
	h := NewHandler(m)
	h.PrincipalFor = func(_ context.Context) (identity.Principal, bool) {
		return identity.Principal{TenantID: "acme", Role: identity.RoleViewer}, true
	}
	rec := httptest.NewRecorder()
	h.Status(rec, httptest.NewRequest("GET", "/gateway/status", nil))
	if rec.Code != 403 {
		t.Fatalf("viewer should get 403, got %d", rec.Code)
	}
}
