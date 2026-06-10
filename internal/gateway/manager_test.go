package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sausheong/harness/tool"

	"github.com/sausheong/runtime/internal/config"
)

// fakeTool is a minimal tool.Tool whose Name follows the harness adapter
// convention "mcp__<server>__<tool>" (what hmcp.Connect produces).
type fakeTool struct {
	name string
	out  string
	err  string
}

func (f fakeTool) Name() string                           { return f.name }
func (f fakeTool) Description() string                    { return "fake " + f.name }
func (f fakeTool) Parameters() json.RawMessage            { return json.RawMessage(`{"type":"object"}`) }
func (f fakeTool) IsConcurrencySafe(json.RawMessage) bool { return false }
func (f fakeTool) Execute(context.Context, json.RawMessage) (tool.ToolResult, error) {
	if f.err != "" {
		return tool.ToolResult{Error: f.err}, nil
	}
	return tool.ToolResult{Output: f.out}, nil
}

type fakeConn struct {
	tools  []tool.Tool
	closed atomic.Bool
}

func (f *fakeConn) Tools() []tool.Tool { return f.tools }
func (f *fakeConn) Close() error       { f.closed.Store(true); return nil }

// scriptDial returns a dialFunc that fails `failures[name]` times for each
// named server before succeeding with the given conn.
func scriptDial(conns map[string]*fakeConn, failures map[string]int) dialFunc {
	attempts := map[string]*int{}
	for name := range conns {
		n := 0
		attempts[name] = &n
	}
	return func(_ context.Context, s config.GatewayServer) (upstreamConn, error) {
		cnt := attempts[s.Name]
		if cnt == nil {
			return nil, errors.New("unknown server " + s.Name)
		}
		*cnt++
		if *cnt <= failures[s.Name] {
			return nil, errors.New("scripted dial failure")
		}
		c, ok := conns[s.Name]
		if !ok {
			return nil, errors.New("no conn scripted for " + s.Name)
		}
		return c, nil
	}
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

func TestManagerConnectsAndExposesTools(t *testing.T) {
	conns := map[string]*fakeConn{
		"fs": {tools: []tool.Tool{fakeTool{name: "mcp__fs__read", out: "data"}}},
	}
	m := NewManager([]config.GatewayServer{{Name: "fs", Command: "x"}},
		WithDial(scriptDial(conns, nil)), WithBackoff(10*time.Millisecond, 50*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)

	waitFor(t, 2*time.Second, func() bool { return len(m.ToolsFor("any-tenant")) == 1 })
	ts := m.ToolsFor("any-tenant")
	// Gateway strips the harness adapter's mcp__ prefix: re-exposed name is
	// <server>__<tool> so the consuming agent ends up with
	// mcp__gateway__fs__read, not a double prefix.
	if got := ts[0].Name(); got != "fs__read" {
		t.Fatalf("want fs__read, got %q", got)
	}
}

func TestManagerTenantFiltering(t *testing.T) {
	conns := map[string]*fakeConn{
		"open":   {tools: []tool.Tool{fakeTool{name: "mcp__open__t", out: "o"}}},
		"scoped": {tools: []tool.Tool{fakeTool{name: "mcp__scoped__t", out: "s"}}},
	}
	m := NewManager([]config.GatewayServer{
		{Name: "open", Command: "x"},
		{Name: "scoped", Command: "x", Tenants: []string{"acme"}},
	}, WithDial(scriptDial(conns, nil)), WithBackoff(10*time.Millisecond, 50*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	waitFor(t, 2*time.Second, func() bool { return len(m.ToolsFor("acme")) == 2 })

	if got := len(m.ToolsFor("acme")); got != 2 {
		t.Fatalf("acme should see 2 tools, got %d", got)
	}
	if got := len(m.ToolsFor("globex")); got != 1 {
		t.Fatalf("globex should see 1 tool, got %d", got)
	}
	// AllTools is the superuser / open-mode view.
	if got := len(m.AllTools()); got != 2 {
		t.Fatalf("AllTools should see 2, got %d", got)
	}
}

func TestManagerDegradeAndReconnect(t *testing.T) {
	conns := map[string]*fakeConn{
		"flaky": {tools: []tool.Tool{fakeTool{name: "mcp__flaky__t", out: "x"}}},
	}
	// First 2 dials fail, third succeeds.
	m := NewManager([]config.GatewayServer{{Name: "flaky", Command: "x"}},
		WithDial(scriptDial(conns, map[string]int{"flaky": 2})),
		WithBackoff(10*time.Millisecond, 50*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)

	// Immediately after start the upstream is down but the manager is alive.
	sts := m.Status("")
	if len(sts) != 1 || sts[0].Name != "flaky" {
		t.Fatalf("unexpected status: %+v", sts)
	}

	waitFor(t, 5*time.Second, func() bool { return len(m.ToolsFor("t")) == 1 })
	sts = m.Status("")
	if sts[0].State != "up" || sts[0].ToolCount != 1 {
		t.Fatalf("want up/1, got %+v", sts[0])
	}
}

func TestManagerStatusTenantScoped(t *testing.T) {
	conns := map[string]*fakeConn{
		"open":   {tools: []tool.Tool{fakeTool{name: "mcp__open__t", out: "o"}}},
		"scoped": {tools: []tool.Tool{fakeTool{name: "mcp__scoped__t", out: "s"}}},
	}
	m := NewManager([]config.GatewayServer{
		{Name: "open", Command: "x"},
		{Name: "scoped", Command: "x", Tenants: []string{"acme"}},
	}, WithDial(scriptDial(conns, nil)), WithBackoff(10*time.Millisecond, 50*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	waitFor(t, 2*time.Second, func() bool { return len(m.AllTools()) == 2 })

	// Status(""): unscoped (superuser/open) — sees both.
	if got := len(m.Status("")); got != 2 {
		t.Fatalf("unscoped status should list 2, got %d", got)
	}
	// Tenant-scoped status hides foreign upstreams.
	if got := len(m.Status("globex")); got != 1 {
		t.Fatalf("globex status should list 1, got %d", got)
	}
	if got := len(m.Status("acme")); got != 2 {
		t.Fatalf("acme status should list 2, got %d", got)
	}
}

func TestManagerGenerationBumpsOnReconnect(t *testing.T) {
	conns := map[string]*fakeConn{
		"fs": {tools: []tool.Tool{fakeTool{name: "mcp__fs__read", out: "d"}}},
	}
	m := NewManager([]config.GatewayServer{{Name: "fs", Command: "x"}},
		WithDial(scriptDial(conns, map[string]int{"fs": 1})),
		WithBackoff(10*time.Millisecond, 50*time.Millisecond))
	g0 := m.Generation()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	waitFor(t, 5*time.Second, func() bool { return m.Generation() > g0 })
}

func TestManagerMarkDownTriggersRedial(t *testing.T) {
	fc := &fakeConn{tools: []tool.Tool{fakeTool{name: "mcp__fs__read", out: "d"}}}
	// First dial yields fc; redials yield a fresh conn each time, matching
	// real dialers (a recycled pointer would defeat the conn-identity check
	// that distinguishes a stale failure report from a current one).
	// Redials are gated: without the gate, the supervise loop (10ms poll) can
	// reconnect between markDown and the "tools cleared" assertion below and
	// flake the test.
	var dials atomic.Int32
	var allowRedial atomic.Bool
	dial := func(_ context.Context, _ config.GatewayServer) (upstreamConn, error) {
		if dials.Add(1) == 1 {
			return fc, nil
		}
		if !allowRedial.Load() {
			return nil, errors.New("redial gated")
		}
		return &fakeConn{tools: fc.tools}, nil
	}
	m := NewManager([]config.GatewayServer{{Name: "fs", Command: "x"}},
		WithDial(dial),
		WithBackoff(10*time.Millisecond, 50*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	waitFor(t, 2*time.Second, func() bool { return len(m.AllTools()) == 1 })

	u := m.ups[0]
	u.mu.Lock()
	observed := u.conn
	u.mu.Unlock()
	gBefore := m.Generation()

	m.markDown(u, observed, errors.New("session died"))
	if !fc.closed.Load() {
		t.Fatal("markDown did not close the observed conn")
	}
	if len(m.AllTools()) != 0 {
		t.Fatal("tools not cleared after markDown")
	}
	if m.Generation() <= gBefore {
		t.Fatal("generation not bumped on markDown")
	}
	// Supervise loop notices and redials once the gate opens.
	allowRedial.Store(true)
	waitFor(t, 5*time.Second, func() bool { return len(m.AllTools()) == 1 })

	// Stale report: marking down with the OLD conn must NOT touch the new one.
	gAfter := m.Generation()
	m.markDown(u, observed, errors.New("stale"))
	if len(m.AllTools()) != 1 {
		t.Fatal("stale markDown cleared a healthy connection")
	}
	if m.Generation() != gAfter {
		t.Fatal("stale markDown bumped generation")
	}
}

func TestManagerCloseWithoutCancel(t *testing.T) {
	fc := &fakeConn{tools: []tool.Tool{fakeTool{name: "mcp__fs__read", out: "d"}}}
	m := NewManager([]config.GatewayServer{{Name: "fs", Command: "x"}},
		WithDial(scriptDial(map[string]*fakeConn{"fs": fc}, nil)),
		WithBackoff(10*time.Millisecond, 50*time.Millisecond))
	m.Start(context.Background()) // caller never cancels
	waitFor(t, 2*time.Second, func() bool { return len(m.AllTools()) == 1 })

	done := make(chan struct{})
	go func() { m.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close deadlocked without external cancel")
	}
	if !fc.closed.Load() {
		t.Fatal("conn not closed")
	}
}

func TestManagerCloseClosesUpstreams(t *testing.T) {
	fc := &fakeConn{tools: []tool.Tool{fakeTool{name: "mcp__fs__read", out: "d"}}}
	m := NewManager([]config.GatewayServer{{Name: "fs", Command: "x"}},
		WithDial(scriptDial(map[string]*fakeConn{"fs": fc}, nil)),
		WithBackoff(10*time.Millisecond, 50*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	waitFor(t, 2*time.Second, func() bool { return len(m.AllTools()) == 1 })
	cancel()
	m.Close()
	if !fc.closed.Load() {
		t.Fatal("upstream conn not closed")
	}
}
