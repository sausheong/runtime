# Gateway M1 — MCP Federation Core Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A central MCP endpoint (`/gateway/mcp`) on the control plane that federates statically configured upstream MCP servers, re-exposes their tools tenant-filtered, and is consumed by runtime agents via a `gateway: true` opt-in.

**Architecture:** New `internal/gateway` package: a `Manager` supervises upstream connections (harness `mcp.Connect` → `tool.Tool` slices, degrade+reconnect), and per-tenant SDK `*mcp.Server` views are built lazily from `Manager.ToolsFor(tenant)` and served by `NewStreamableHTTPHandler`. Control plane mounts `/gateway/mcp` + `/gateway/status` behind the existing identity middleware. `gateway: true` agents get `RUNTIME_GATEWAY_URL`/`RUNTIME_GATEWAY_KEY` injected via `buildEnv`; `agentkind` appends a gateway `MCPServers` entry.

**Tech Stack:** Go 1.25, `github.com/modelcontextprotocol/go-sdk v1.5.0` (direct dep — currently indirect), harness `tools/mcp` + `tool` (via `replace ../harness`), existing `internal/identity` + `controlplane`.

**Spec:** `docs/superpowers/specs/2026-06-10-gateway-m1-mcp-federation-design.md`

**Branch:** `feat/gateway-m1` (create from `master` before Task 1).

**Conventions that matter (from ROADMAP):**
- The `go` CLI is ground truth; IGNORE IDE/LSP diagnostics (the `replace ../harness` cross-module setup confuses them). Trust `go build ./...` / `go test ./...`.
- Unit tests are hermetic. Integration tests carry `//go:build integration` and need Postgres at `postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable` (local Postgres.app).
- All commands run from the repo root `/Users/sausheong/projects/runtime`.

## File Structure

| File | Responsibility |
|---|---|
| `internal/config/config.go` (modify) | `GatewayConfig`/`GatewayServer` types, `${VAR}` expansion, validation, `AgentConfig.Gateway` bool |
| `internal/gateway/manager.go` (create) | Upstream lifecycle: connect, degrade, reconnect w/ backoff, tool snapshots, generation counter, tenancy filter |
| `internal/gateway/server.go` (create) | Per-tenant SDK `*mcp.Server` cache + `http.Handler` (StreamableHTTP), role gate, status endpoint handler |
| `internal/gateway/connect.go` (create) | Dial seam: maps `config.GatewayServer` → harness `mcp.Connect` (swappable in tests) |
| `controlplane/proxy.go` (modify) | `AgentProcess.Gateway*` fields + env injection in `buildEnv` |
| `controlplane/registry.go` (modify) | Thread gateway fields from config into `AgentProcess` |
| `internal/agentkind/registry.go` (modify) | `Deps.GatewayURL/GatewayKey` + `wireGateway` appending `Spec.MCPServers` |
| `cmd/agentd/main.go` (modify) | Read `RUNTIME_GATEWAY_URL`/`RUNTIME_GATEWAY_KEY` into `Deps` |
| `cmd/runtimed/main.go` (modify) | Build `gateway.Manager` + mount routes in `buildRoot` |
| `test/gateway_e2e_test.go` (create) | Through-serve integration test |

---

### Task 1: Gateway config types + validation

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go` (append)

- [ ] **Step 1: Write the failing tests**

Append to `internal/config/config_test.go` (read the file first to match its test style — it uses plain `testing`, no testify):

```go
func TestGatewayConfigValidation(t *testing.T) {
	base := func() *Config {
		return &Config{Agents: []AgentConfig{
			{ID: "a", Name: "A", Model: "m", ListenAddr: "127.0.0.1:1"},
		}}
	}

	t.Run("valid stdio and http servers", func(t *testing.T) {
		c := base()
		c.Gateway = GatewayConfig{Servers: []GatewayServer{
			{Name: "fs", Command: "npx", Args: []string{"-y", "server-fs"}},
			{Name: "web", URL: "https://example.com/mcp"},
		}}
		if err := c.Validate(); err != nil {
			t.Fatalf("expected valid, got %v", err)
		}
	})

	t.Run("empty gateway section is fine", func(t *testing.T) {
		c := base()
		if err := c.Validate(); err != nil {
			t.Fatalf("expected valid, got %v", err)
		}
	})

	t.Run("server requires name", func(t *testing.T) {
		c := base()
		c.Gateway = GatewayConfig{Servers: []GatewayServer{{URL: "https://x/mcp"}}}
		if err := c.Validate(); err == nil {
			t.Fatal("expected error for missing name")
		}
	})

	t.Run("duplicate server names rejected", func(t *testing.T) {
		c := base()
		c.Gateway = GatewayConfig{Servers: []GatewayServer{
			{Name: "fs", URL: "https://a/mcp"},
			{Name: "fs", URL: "https://b/mcp"},
		}}
		if err := c.Validate(); err == nil {
			t.Fatal("expected error for duplicate name")
		}
	})

	t.Run("command and url mutually exclusive", func(t *testing.T) {
		c := base()
		c.Gateway = GatewayConfig{Servers: []GatewayServer{
			{Name: "fs", Command: "npx", URL: "https://x/mcp"},
		}}
		if err := c.Validate(); err == nil {
			t.Fatal("expected error for both command and url")
		}
	})

	t.Run("one of command or url required", func(t *testing.T) {
		c := base()
		c.Gateway = GatewayConfig{Servers: []GatewayServer{{Name: "fs"}}}
		if err := c.Validate(); err == nil {
			t.Fatal("expected error for neither command nor url")
		}
	})
}

func TestGatewayEnvExpansion(t *testing.T) {
	t.Setenv("GW_TEST_TOKEN", "sekrit")

	t.Run("expands ${VAR} in headers env and agent_keys", func(t *testing.T) {
		c := &Config{
			Agents: []AgentConfig{{ID: "a", Name: "A", Model: "m", ListenAddr: "127.0.0.1:1"}},
			Gateway: GatewayConfig{
				Servers: []GatewayServer{{
					Name: "web", URL: "https://x/mcp",
					Headers: map[string]string{"Authorization": "Bearer ${GW_TEST_TOKEN}"},
					Env:     map[string]string{"TOKEN": "${GW_TEST_TOKEN}"},
				}},
				AgentKeys: map[string]string{"default": "${GW_TEST_TOKEN}"},
			},
		}
		if err := c.Validate(); err != nil {
			t.Fatalf("expected valid, got %v", err)
		}
		if got := c.Gateway.Servers[0].Headers["Authorization"]; got != "Bearer sekrit" {
			t.Fatalf("header not expanded: %q", got)
		}
		if got := c.Gateway.Servers[0].Env["TOKEN"]; got != "sekrit" {
			t.Fatalf("env not expanded: %q", got)
		}
		if got := c.Gateway.AgentKeys["default"]; got != "sekrit" {
			t.Fatalf("agent key not expanded: %q", got)
		}
	})

	t.Run("unset var is a load error", func(t *testing.T) {
		c := &Config{
			Agents: []AgentConfig{{ID: "a", Name: "A", Model: "m", ListenAddr: "127.0.0.1:1"}},
			Gateway: GatewayConfig{Servers: []GatewayServer{{
				Name: "web", URL: "https://x/mcp",
				Headers: map[string]string{"Authorization": "Bearer ${GW_UNSET_VAR_XYZ}"},
			}}},
		}
		if err := c.Validate(); err == nil {
			t.Fatal("expected error for unset env var")
		}
	})

	t.Run("literal values pass through", func(t *testing.T) {
		c := &Config{
			Agents: []AgentConfig{{ID: "a", Name: "A", Model: "m", ListenAddr: "127.0.0.1:1"}},
			Gateway: GatewayConfig{Servers: []GatewayServer{{
				Name: "web", URL: "https://x/mcp",
				Headers: map[string]string{"X-Plain": "no-vars-here"},
			}}},
		}
		if err := c.Validate(); err != nil {
			t.Fatalf("expected valid, got %v", err)
		}
		if got := c.Gateway.Servers[0].Headers["X-Plain"]; got != "no-vars-here" {
			t.Fatalf("literal mangled: %q", got)
		}
	})
}

func TestAgentConfigGatewayFlag(t *testing.T) {
	c := &Config{Agents: []AgentConfig{
		{ID: "a", Name: "A", Model: "m", ListenAddr: "127.0.0.1:1", Gateway: true},
	}}
	if err := c.Validate(); err != nil {
		t.Fatalf("expected valid, got %v", err)
	}
	if !c.Agents[0].Gateway {
		t.Fatal("gateway flag lost")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/config/ -run 'TestGateway|TestAgentConfigGatewayFlag' -v`
Expected: compile FAILURE — `GatewayConfig`, `GatewayServer`, `c.Gateway`, `AgentConfig.Gateway` undefined.

- [ ] **Step 3: Implement**

In `internal/config/config.go`:

1. Add to `AgentConfig` (after `Memory`):

```go
	Gateway    bool     `yaml:"gateway"` // optional; opt-in to the platform MCP gateway (env-injected URL+key). Default false.
```

2. Add the new types after `TokenConfig`:

```go
// GatewayServer is one upstream MCP server the gateway federates. Exactly one
// of Command (stdio) or URL (Streamable HTTP) must be set. Header, Env, and
// (in GatewayConfig) AgentKeys values support ${VAR} expansion from the
// operator environment at load time so secrets stay out of the YAML file.
type GatewayServer struct {
	Name    string            `yaml:"name"`    // required, unique; namespaces tools as <name>__<tool>
	Command string            `yaml:"command"` // stdio transport: argv[0]
	Args    []string          `yaml:"args"`
	Env     map[string]string `yaml:"env"`     // extra env for the stdio child
	URL     string            `yaml:"url"`     // Streamable HTTP transport
	Headers map[string]string `yaml:"headers"` // static headers (auth) for HTTP
	Tenants []string          `yaml:"tenants"` // nil/empty ⇒ visible to ALL tenants
}

// GatewayConfig is the optional top-level gateway: section.
type GatewayConfig struct {
	Servers   []GatewayServer   `yaml:"servers"`
	AgentKeys map[string]string `yaml:"agent_keys"` // tenant → service key injected into gateway:true agents
	SelfURL   string            `yaml:"self_url"`   // optional base URL agents use to reach the gateway
}

// Enabled reports whether any upstream is configured.
func (g GatewayConfig) Enabled() bool { return len(g.Servers) > 0 }
```

3. Add `Gateway GatewayConfig \`yaml:"gateway"\`` to `Config`.

4. In `Validate()`, after the token loop, append:

```go
	names := map[string]bool{}
	for i := range c.Gateway.Servers {
		s := &c.Gateway.Servers[i]
		if s.Name == "" {
			return fmt.Errorf("config: gateway server[%d] requires name", i)
		}
		if names[s.Name] {
			return fmt.Errorf("config: duplicate gateway server name %q", s.Name)
		}
		names[s.Name] = true
		if (s.Command == "") == (s.URL == "") {
			return fmt.Errorf("config: gateway server %q requires exactly one of command or url", s.Name)
		}
		if err := expandEnvMap(s.Headers, "gateway server "+s.Name+" headers"); err != nil {
			return err
		}
		if err := expandEnvMap(s.Env, "gateway server "+s.Name+" env"); err != nil {
			return err
		}
	}
	if err := expandEnvMap(c.Gateway.AgentKeys, "gateway agent_keys"); err != nil {
		return err
	}
	return nil
```

(The existing `return nil` at the end of `Validate` is replaced by this block's final `return nil`.)

5. Add the helper at the bottom of the file:

```go
// expandEnvMap expands ${VAR} references in every value of m from the operator
// environment, in place. An unset (or empty) variable is a hard error — silent
// empty-string expansion would send a malformed credential downstream. The
// $VAR form (no braces) is also expanded, matching os.Expand semantics.
func expandEnvMap(m map[string]string, what string) error {
	for k, v := range m {
		var missing []string
		expanded := os.Expand(v, func(name string) string {
			val, ok := os.LookupEnv(name)
			if !ok || val == "" {
				missing = append(missing, name)
			}
			return val
		})
		if len(missing) > 0 {
			return fmt.Errorf("config: %s %q references unset env var(s) %v", what, k, missing)
		}
		m[k] = expanded
	}
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/config/ -v`
Expected: ALL PASS (new tests + pre-existing ones).

- [ ] **Step 5: Commit**

```bash
git add internal/config/
git commit -m "feat(gateway): config types, validation, \${VAR} expansion"
```

---

### Task 2: Gateway Manager — upstream lifecycle, tool snapshots, tenancy filter

**Files:**
- Create: `internal/gateway/manager.go`
- Create: `internal/gateway/connect.go`
- Test: `internal/gateway/manager_test.go`

The Manager treats each upstream as a small supervised unit. The dial seam (`connect.go`) is a function value so tests substitute in-memory upstreams without network/processes.

- [ ] **Step 1: Write connect.go (the dial seam — no test of its own; it's a thin map + the real harness call)**

```go
// Package gateway federates upstream MCP servers into one tenant-filtered MCP
// endpoint on the control plane. The Manager owns upstream lifecycle
// (connect, degrade, reconnect); server.go exposes the federated tool set as
// per-tenant MCP servers over Streamable HTTP.
package gateway

import (
	"context"

	hmcp "github.com/sausheong/harness/tools/mcp"
	"github.com/sausheong/harness/tool"

	"github.com/sausheong/runtime/internal/config"
)

// upstreamConn is the connected form of one upstream: its adapted tools and a
// closer. Satisfied by *hmcp.Client in production and by fakes in tests.
type upstreamConn interface {
	Tools() []tool.Tool
	Close() error
}

// dialFunc connects one configured upstream. Swapped in tests.
type dialFunc func(ctx context.Context, s config.GatewayServer) (upstreamConn, error)

// dialHarness is the production dialFunc: it maps config.GatewayServer onto
// harness mcp.ServerConfig and Connects (stdio or Streamable HTTP).
func dialHarness(ctx context.Context, s config.GatewayServer) (upstreamConn, error) {
	return hmcp.Connect(ctx, hmcp.ServerConfig{
		Name:    s.Name,
		Command: s.Command,
		Args:    s.Args,
		Env:     s.Env,
		URL:     s.URL,
		Headers: s.Headers,
	})
}
```

- [ ] **Step 2: Write the failing Manager tests**

`internal/gateway/manager_test.go`. Test upstreams are fakes implementing `upstreamConn`; a scripted `dialFunc` controls connect success/failure per attempt.

```go
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

func (f fakeTool) Name() string                            { return f.name }
func (f fakeTool) Description() string                     { return "fake " + f.name }
func (f fakeTool) Parameters() json.RawMessage             { return json.RawMessage(`{"type":"object"}`) }
func (f fakeTool) IsConcurrencySafe(json.RawMessage) bool  { return false }
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

// scriptDial returns a dialFunc that fails `failures` times for each named
// server before succeeding with the given conn.
func scriptDial(conns map[string]*fakeConn, failures map[string]int) dialFunc {
	var mu = make(map[string]*int)
	for name := range conns {
		n := 0
		mu[name] = &n
	}
	return func(_ context.Context, s config.GatewayServer) (upstreamConn, error) {
		cnt := mu[s.Name]
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

	waitFor(t, 2*time.Second, func() (ok bool) {
		ts := m.ToolsFor("any-tenant")
		return len(ts) == 1
	})
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

	waitFor(t, 2*time.Second, func() bool { return len(m.ToolsFor("t")) == 1 })
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
	waitFor(t, 2*time.Second, func() bool { return m.Generation() > g0 })
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
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./internal/gateway/ -v`
Expected: compile FAILURE — `NewManager`, `WithDial`, `WithBackoff`, etc. undefined.

- [ ] **Step 4: Implement manager.go**

```go
package gateway

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sausheong/harness/tool"

	"github.com/sausheong/runtime/internal/config"
)

// UpstreamStatus is the operator-facing state of one upstream.
type UpstreamStatus struct {
	Name        string    `json:"name"`
	Transport   string    `json:"transport"` // "stdio" | "http"
	State       string    `json:"state"`     // "up" | "down"
	ToolCount   int       `json:"tool_count"`
	LastError   string    `json:"last_error,omitempty"`
	ConnectedAt time.Time `json:"connected_at,omitzero"`
}

// upstream is one configured server plus its live connection state.
type upstream struct {
	cfg config.GatewayServer

	mu          sync.Mutex
	conn        upstreamConn
	tools       []tool.Tool // renamed view (gateway names), nil when down
	lastErr     error
	connectedAt time.Time
}

// Manager owns the configured upstreams. Start launches one supervision
// goroutine per upstream (connect → on failure retry with capped backoff).
// All read methods are safe for concurrent use.
type Manager struct {
	ups        []*upstream
	dial       dialFunc
	minBackoff time.Duration
	maxBackoff time.Duration

	generation atomic.Uint64
	wg         sync.WaitGroup
}

// Option configures a Manager.
type Option func(*Manager)

// WithDial overrides the production dialer (tests).
func WithDial(d dialFunc) Option { return func(m *Manager) { m.dial = d } }

// WithBackoff overrides retry pacing (tests).
func WithBackoff(min, max time.Duration) Option {
	return func(m *Manager) { m.minBackoff, m.maxBackoff = min, max }
}

// NewManager builds a Manager for the configured servers. Call Start to begin
// connecting.
func NewManager(servers []config.GatewayServer, opts ...Option) *Manager {
	m := &Manager{dial: dialHarness, minBackoff: time.Second, maxBackoff: time.Minute}
	for _, s := range servers {
		m.ups = append(m.ups, &upstream{cfg: s})
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Start launches the supervision loops. Non-blocking; safe to call once.
func (m *Manager) Start(ctx context.Context) {
	for _, u := range m.ups {
		m.wg.Add(1)
		go m.supervise(ctx, u)
	}
}

// supervise keeps one upstream connected: dial, mark up, wait for ctx
// cancellation... reconnection on call failure is driven by markDown (callers
// report a dead session) plus this loop noticing conn == nil.
func (m *Manager) supervise(ctx context.Context, u *upstream) {
	defer m.wg.Done()
	backoff := m.minBackoff
	for {
		if ctx.Err() != nil {
			return
		}
		u.mu.Lock()
		connected := u.conn != nil
		u.mu.Unlock()
		if !connected {
			conn, err := m.dial(ctx, u.cfg)
			if err != nil {
				u.mu.Lock()
				u.lastErr = err
				u.mu.Unlock()
				slog.Warn("gateway: upstream connect failed",
					"server", u.cfg.Name, "transport", transportOf(u.cfg), "err", err)
				// capped exponential backoff with jitter
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff + time.Duration(rand.Int64N(int64(backoff/2+1)))):
				}
				backoff = min(backoff*2, m.maxBackoff)
				continue
			}
			renamed := renameTools(u.cfg.Name, conn.Tools())
			u.mu.Lock()
			u.conn, u.tools, u.lastErr, u.connectedAt = conn, renamed, nil, time.Now()
			u.mu.Unlock()
			m.generation.Add(1)
			backoff = m.minBackoff
			slog.Info("gateway: upstream connected",
				"server", u.cfg.Name, "transport", transportOf(u.cfg), "tools", len(renamed))
		}
		// Poll for down-marking (markDown clears conn). Cheap; avoids needing
		// a notification channel through the tool execution path.
		select {
		case <-ctx.Done():
			return
		case <-time.After(m.minBackoff):
		}
	}
}

// markDown records a mid-flight failure: closes and clears the connection so
// the supervision loop redials. Called by gwTool.Execute on session errors.
func (m *Manager) markDown(u *upstream, err error) {
	u.mu.Lock()
	if u.conn != nil {
		_ = u.conn.Close()
	}
	u.conn, u.tools, u.lastErr = nil, nil, err
	u.mu.Unlock()
	m.generation.Add(1)
	slog.Warn("gateway: upstream marked down", "server", u.cfg.Name, "err", err)
}

// Close tears down all connections. Call after the context passed to Start is
// cancelled.
func (m *Manager) Close() {
	m.wg.Wait()
	for _, u := range m.ups {
		u.mu.Lock()
		if u.conn != nil {
			_ = u.conn.Close()
			u.conn = nil
		}
		u.mu.Unlock()
	}
}

// Generation increments whenever the federated tool set may have changed
// (connect, reconnect, down). Server caches key on it.
func (m *Manager) Generation() uint64 { return m.generation.Load() }

// visibleTo reports whether an upstream is visible to tenant. Empty Tenants ⇒
// visible to all. The empty tenant ("") means the unscoped view (superuser or
// open mode) and sees everything.
func visibleTo(s config.GatewayServer, tenant string) bool {
	if tenant == "" || len(s.Tenants) == 0 {
		return true
	}
	for _, t := range s.Tenants {
		if t == tenant {
			return true
		}
	}
	return false
}

// ToolsFor returns the live tools visible to tenant.
func (m *Manager) ToolsFor(tenant string) []tool.Tool {
	var out []tool.Tool
	for _, u := range m.ups {
		if !visibleTo(u.cfg, tenant) {
			continue
		}
		u.mu.Lock()
		out = append(out, u.tools...)
		u.mu.Unlock()
	}
	return out
}

// AllTools is the unscoped view (open mode / superuser).
func (m *Manager) AllTools() []tool.Tool { return m.ToolsFor("") }

// Status returns per-upstream state. tenant=="" ⇒ unscoped (all upstreams);
// otherwise only upstreams visible to that tenant.
func (m *Manager) Status(tenant string) []UpstreamStatus {
	var out []UpstreamStatus
	for _, u := range m.ups {
		if !visibleTo(u.cfg, tenant) {
			continue
		}
		u.mu.Lock()
		st := UpstreamStatus{
			Name:      u.cfg.Name,
			Transport: transportOf(u.cfg),
			State:     "down",
			ToolCount: len(u.tools),
		}
		if u.conn != nil {
			st.State = "up"
			st.ConnectedAt = u.connectedAt
		}
		if u.lastErr != nil {
			st.LastError = u.lastErr.Error()
		}
		u.mu.Unlock()
		out = append(out, st)
	}
	return out
}

func transportOf(s config.GatewayServer) string {
	if s.Command != "" {
		return "stdio"
	}
	return "http"
}

// renameTools wraps each harness-adapted tool so its gateway-facing name is
// "<server>__<tool>" instead of the adapter's "mcp__<server>__<tool>". The
// consuming harness client prepends its own "mcp__gateway__", so stripping
// here avoids a double prefix. Names not following the adapter convention
// pass through unchanged (TrimPrefix is a no-op).
func renameTools(server string, ts []tool.Tool) []tool.Tool {
	out := make([]tool.Tool, 0, len(ts))
	for _, t := range ts {
		out = append(out, renamedTool{Tool: t, name: strings.TrimPrefix(t.Name(), "mcp__")})
	}
	return out
}

// renamedTool overrides only Name; everything else delegates.
type renamedTool struct {
	tool.Tool
	name string
}

func (r renamedTool) Name() string { return r.name }
```

Note for the implementer: the exact rename rule is — strip a leading `"mcp__"` if present; the rest (`<server>__<tool>`) is already correctly namespaced by the harness adapter. The `server` parameter of `renameTools` is intentionally unused after this simplification; drop it if you prefer (`renameTools(ts []tool.Tool)`) and update the call site.

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/gateway/ -v`
Expected: ALL PASS. If `TestManagerDegradeAndReconnect` flakes on timing, widen `waitFor` to 5s — never sleep-and-hope.

- [ ] **Step 6: Run vet + full unit suite**

Run: `go vet ./... && go test ./...`
Expected: clean (integration tests excluded by build tag).

- [ ] **Step 7: Commit**

```bash
git add internal/gateway/
git commit -m "feat(gateway): Manager — upstream lifecycle, tenancy filter, degrade+reconnect"
```

---

### Task 3: Gateway MCP server — per-tenant views, role gate, status + HTTP handler

**Files:**
- Create: `internal/gateway/server.go`
- Test: `internal/gateway/server_test.go`

This task builds the SDK-server layer: per-tenant `*sdk.Server` cache keyed on (tenant, generation), the StreamableHTTP handler, the role gate, and the `/gateway/status` JSON handler. Principal extraction uses a small injected func so the package doesn't import `controlplane` (which would be an import cycle — controlplane will import gateway).

- [ ] **Step 1: Write the failing tests**

`internal/gateway/server_test.go`:

```go
package gateway

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
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
	waitFor(t, 2*time.Second, func() bool {
		return len(m.AllTools()) == totalTools(conns)
	})
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
	names := listNames(t, sess)
	if len(names) != 2 {
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
	// Listing is allowed.
	if names := listNames(t, viewer); len(names) != 2 {
		t.Fatalf("viewer should list 2, got %v", names)
	}
	// Calling is denied via isError result mentioning the role.
	res, err := viewer.CallTool(context.Background(), &sdk.CallToolParams{Name: "open__echo"})
	if err != nil {
		t.Fatalf("expected isError result, got transport error: %v", err)
	}
	if !res.IsError {
		t.Fatal("viewer call should be IsError")
	}
}

func TestServerRebuildsOnGenerationChange(t *testing.T) {
	conns := gwConns()
	m := startManager(t, gwServers(), conns)
	h := NewHandler(m)
	sess := dialGateway(t, h, &identity.Principal{TenantID: "acme", Role: identity.RoleOperator})
	if names := listNames(t, sess); len(names) != 2 {
		t.Fatalf("pre: want 2, got %v", names)
	}
	// Simulate an upstream going down: markDown bumps generation; a NEW MCP
	// session should see the reduced tool set.
	m.markDown(m.ups[1], context.DeadlineExceeded)
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/gateway/ -v -run 'TestServer|TestStatus'`
Expected: compile FAILURE — `Handler`, `NewHandler` undefined.

- [ ] **Step 3: Implement server.go**

```go
package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sausheong/harness/tool"

	"github.com/sausheong/runtime/internal/identity"
)

// Handler serves the federated tool set over MCP Streamable HTTP plus the
// operator status endpoint. Per-tenant SDK servers are cached and rebuilt
// when the Manager's generation moves.
//
// PrincipalFor extracts the authenticated principal from a request context.
// It is injected (rather than importing controlplane) to avoid an import
// cycle; runtimed wires it to controlplane.PrincipalFromContext. A false
// return means open mode: full visibility, calls allowed.
type Handler struct {
	m            *Manager
	PrincipalFor func(ctx context.Context) (identity.Principal, bool)

	mu    sync.Mutex
	cache map[string]*cachedServer // tenant view key → server
}

type cachedServer struct {
	gen uint64
	srv *sdk.Server
}

// NewHandler builds a Handler over m. PrincipalFor defaults to "no principal"
// (open mode) until wired.
func NewHandler(m *Manager) *Handler {
	return &Handler{
		m:            m,
		PrincipalFor: func(context.Context) (identity.Principal, bool) { return identity.Principal{}, false },
		cache:        map[string]*cachedServer{},
	}
}

// viewKey computes the cache key and the tenant filter for a principal.
// Unscoped ("" tenant filter) for open mode and superusers.
func viewKey(p identity.Principal, ok bool) (key, tenant string) {
	if !ok || p.Superuser {
		return "*", ""
	}
	return "t:" + p.TenantID, p.TenantID
}

// HTTP returns the Streamable HTTP handler for /gateway/mcp.
func (h *Handler) HTTP() http.Handler {
	return sdk.NewStreamableHTTPHandler(func(r *http.Request) *sdk.Server {
		p, ok := h.PrincipalFor(r.Context())
		return h.serverFor(p, ok)
	}, nil)
}

// serverFor returns the cached SDK server for the principal's view,
// rebuilding when the manager generation has moved.
func (h *Handler) serverFor(p identity.Principal, ok bool) *sdk.Server {
	key, tenant := viewKey(p, ok)
	gen := h.m.Generation()
	h.mu.Lock()
	defer h.mu.Unlock()
	if c, hit := h.cache[key]; hit && c.gen == gen {
		return c.srv
	}
	srv := sdk.NewServer(&sdk.Implementation{Name: "runtime-gateway", Version: "m1"}, nil)
	for _, t := range h.m.ToolsFor(tenant) {
		t := t
		srv.AddTool(&sdk.Tool{
			Name:        t.Name(),
			Description: t.Description(),
			InputSchema: json.RawMessage(t.Parameters()),
		}, h.toolHandler(t))
	}
	h.cache[key] = &cachedServer{gen: gen, srv: srv}
	return srv
}

// toolHandler adapts one harness tool.Tool to an SDK ToolHandler, enforcing
// the role gate (call requires ≥ operator when a principal is present).
func (h *Handler) toolHandler(t tool.Tool) sdk.ToolHandler {
	return func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		if p, ok := h.PrincipalFor(ctx); ok && !p.Superuser && p.Role == identity.RoleViewer {
			return errResult("forbidden: role viewer cannot call tools (requires operator)"), nil
		}
		res, err := t.Execute(ctx, req.Params.Arguments)
		if err != nil {
			return errResult(err.Error()), nil
		}
		if res.Error != "" {
			return errResult(res.Error), nil
		}
		out := &sdk.CallToolResult{}
		if res.Output != "" || len(res.Images) == 0 {
			out.Content = append(out.Content, &sdk.TextContent{Text: res.Output})
		}
		for _, img := range res.Images {
			out.Content = append(out.Content, &sdk.ImageContent{
				MIMEType: img.MimeType, Data: img.Data,
			})
		}
		return out, nil
	}
}

func errResult(msg string) *sdk.CallToolResult {
	return &sdk.CallToolResult{
		IsError: true,
		Content: []sdk.Content{&sdk.TextContent{Text: msg}},
	}
}

// Status serves GET /gateway/status: per-upstream state, tenant-scoped.
// Requires role ≥ operator when identity is on (open mode: allowed).
func (h *Handler) Status(w http.ResponseWriter, r *http.Request) {
	tenant := ""
	if p, ok := h.PrincipalFor(r.Context()); ok {
		if p.Role == identity.RoleViewer && !p.Superuser {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if !p.Superuser {
			tenant = p.TenantID
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(h.m.Status(tenant))
}
```

Implementation notes:
- `sdk.ImageContent.Data` is `[]byte` (base64 handled by the SDK marshaling) — pass `img.Data` directly.
- The empty-output case: when a tool returns empty Output AND no images, still emit one empty `TextContent` so the result has content (the SDK requires non-nil content arrays; verify with the round-trip test).
- If `req.Params.Arguments` is the raw-params type: in v1.5.0 the server-side raw request is `CallToolRequest = ServerRequest[*CallToolParamsRaw]` whose `Arguments` is `json.RawMessage` — exactly what `tool.Tool.Execute` takes.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/gateway/ -v`
Expected: ALL PASS.

- [ ] **Step 5: Vet + full unit suite + commit**

Run: `go vet ./... && go test ./...`
Expected: clean.

```bash
git add internal/gateway/
git commit -m "feat(gateway): per-tenant MCP servers, role gate, status endpoint"
```

---

### Task 4: Agent env injection (`gateway: true` → RUNTIME_GATEWAY_URL/KEY)

**Files:**
- Modify: `controlplane/proxy.go` (AgentProcess fields + buildEnv)
- Modify: `controlplane/registry.go` (thread config through)
- Test: `controlplane/proxy_test.go` (append), `controlplane/registry_test.go` (append)

- [ ] **Step 1: Write the failing tests**

Read `controlplane/proxy_test.go` first to see existing buildEnv test style, then append:

```go
func TestBuildEnvGateway(t *testing.T) {
	t.Run("gateway on: url and key injected", func(t *testing.T) {
		a := AgentProcess{
			AgentID: "x", Addr: "127.0.0.1:1", BinPath: "bin", PGDSN: "dsn",
			Tenant: "acme", GatewayOn: true,
			GatewayURL: "http://127.0.0.1:8080/gateway/mcp",
			GatewayKey: "svk-test",
		}
		env, err := a.buildEnv(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		assertHasEnv(t, env, "RUNTIME_GATEWAY_URL=http://127.0.0.1:8080/gateway/mcp")
		assertHasEnv(t, env, "RUNTIME_GATEWAY_KEY=svk-test")
	})

	t.Run("gateway on, no key (open mode): url only", func(t *testing.T) {
		a := AgentProcess{
			AgentID: "x", Addr: "127.0.0.1:1", BinPath: "bin", PGDSN: "dsn",
			Tenant: "default", GatewayOn: true,
			GatewayURL: "http://127.0.0.1:8080/gateway/mcp",
		}
		env, err := a.buildEnv(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		assertHasEnv(t, env, "RUNTIME_GATEWAY_URL=http://127.0.0.1:8080/gateway/mcp")
		assertNoEnvPrefix(t, env, "RUNTIME_GATEWAY_KEY=")
	})

	t.Run("gateway off: neither injected", func(t *testing.T) {
		a := AgentProcess{AgentID: "x", Addr: "127.0.0.1:1", BinPath: "bin", PGDSN: "dsn", Tenant: "t"}
		env, err := a.buildEnv(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		assertNoEnvPrefix(t, env, "RUNTIME_GATEWAY_URL=")
		assertNoEnvPrefix(t, env, "RUNTIME_GATEWAY_KEY=")
	})
}

func assertHasEnv(t *testing.T, env []string, want string) {
	t.Helper()
	for _, e := range env {
		if e == want {
			return
		}
	}
	t.Fatalf("env missing %q", want)
}

func assertNoEnvPrefix(t *testing.T, env []string, prefix string) {
	t.Helper()
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			t.Fatalf("env unexpectedly has %q", e)
		}
	}
}
```

(If helpers with these names already exist in the package's tests, reuse them instead of redefining.)

Append to `controlplane/registry_test.go`:

```go
func TestRegistryThreadsGateway(t *testing.T) {
	cfg := &config.Config{
		Agents: []config.AgentConfig{
			{ID: "g", Name: "G", Model: "m", ListenAddr: "127.0.0.1:1", Tenant: "acme", Gateway: true},
			{ID: "p", Name: "P", Model: "m", ListenAddr: "127.0.0.1:2"},
		},
		Gateway: config.GatewayConfig{
			Servers:   []config.GatewayServer{{Name: "fs", Command: "x"}},
			AgentKeys: map[string]string{"acme": "svk-acme"},
			SelfURL:   "http://127.0.0.1:9999",
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	r := NewRegistry(cfg, "bin", "dsn")
	r.SetGateway("http://127.0.0.1:9999/gateway/mcp", cfg.Gateway.AgentKeys)

	g, _ := r.Get("g")
	if !g.GatewayOn || g.GatewayURL != "http://127.0.0.1:9999/gateway/mcp" || g.GatewayKey != "svk-acme" {
		t.Fatalf("gateway agent not wired: %+v", g)
	}
	p, _ := r.Get("p")
	if p.GatewayOn || p.GatewayURL != "" || p.GatewayKey != "" {
		t.Fatalf("non-gateway agent leaked gateway env: %+v", p)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./controlplane/ -run 'TestBuildEnvGateway|TestRegistryThreadsGateway' -v`
Expected: compile FAILURE — `GatewayOn`, `GatewayURL`, `GatewayKey`, `SetGateway` undefined.

- [ ] **Step 3: Implement**

In `controlplane/proxy.go`, add fields to `AgentProcess` after `Memory`:

```go
	GatewayOn  bool   // opt-in: when true, spawn env carries RUNTIME_GATEWAY_URL (+_KEY when set).
	GatewayURL string // full URL of the platform gateway MCP endpoint.
	GatewayKey string // tenant service key for the gateway; "" in open mode.
```

In `buildEnv`, after the `if a.Memory { ... }` block:

```go
	if a.GatewayOn {
		env = append(env, "RUNTIME_GATEWAY_URL="+a.GatewayURL)
		if a.GatewayKey != "" {
			env = append(env, "RUNTIME_GATEWAY_KEY="+a.GatewayKey)
		}
	}
```

In `controlplane/registry.go`:

1. In `NewRegistry`, add `GatewayOn: a.Gateway,` to the `AgentProcess` literal.
2. Add a setter (same pattern/caveats as `SetBroker` — call before serving):

```go
// SetGateway records the gateway endpoint URL and per-tenant agent keys,
// stamped onto every gateway-enabled AgentProcess returned by Get. Like
// SetBroker, it must complete before the supervisor goroutines start.
func (r *Registry) SetGateway(url string, keys map[string]string) {
	for id, ap := range r.agents {
		if !ap.GatewayOn {
			continue
		}
		ap.GatewayURL = url
		ap.GatewayKey = keys[ap.Tenant]
		r.agents[id] = ap
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./controlplane/ -v`
Expected: ALL PASS.

- [ ] **Step 5: Commit**

```bash
git add controlplane/
git commit -m "feat(gateway): inject RUNTIME_GATEWAY_URL/KEY into gateway-enabled agents"
```

---

### Task 5: agentd consumes the gateway env (wireGateway)

**Files:**
- Modify: `internal/agentkind/registry.go`
- Modify: `cmd/agentd/main.go`
- Test: `internal/agentkind/registry_test.go` (append)

- [ ] **Step 1: Write the failing tests**

Read `internal/agentkind/registry_test.go` first for style, then append:

```go
func TestWireGatewayAppendsMCPServer(t *testing.T) {
	b, _ := Get("testagent")

	t.Run("url set: gateway MCP server appended with auth header", func(t *testing.T) {
		cfg, err := b(Deps{AgentID: "a", GatewayURL: "http://cp/gateway/mcp", GatewayKey: "svk-1"})
		if err != nil {
			t.Fatal(err)
		}
		if len(cfg.Spec.MCPServers) != 1 {
			t.Fatalf("want 1 MCP server, got %d", len(cfg.Spec.MCPServers))
		}
		s := cfg.Spec.MCPServers[0]
		if s.Name != "gateway" || s.URL != "http://cp/gateway/mcp" {
			t.Fatalf("wrong server: %+v", s)
		}
		if s.Headers["Authorization"] != "Bearer svk-1" {
			t.Fatalf("wrong auth header: %+v", s.Headers)
		}
	})

	t.Run("url set, no key: no auth header (open mode)", func(t *testing.T) {
		cfg, err := b(Deps{AgentID: "a", GatewayURL: "http://cp/gateway/mcp"})
		if err != nil {
			t.Fatal(err)
		}
		if len(cfg.Spec.MCPServers) != 1 {
			t.Fatalf("want 1 MCP server, got %d", len(cfg.Spec.MCPServers))
		}
		if len(cfg.Spec.MCPServers[0].Headers) != 0 {
			t.Fatalf("unexpected headers: %+v", cfg.Spec.MCPServers[0].Headers)
		}
	})

	t.Run("no url: no MCP servers", func(t *testing.T) {
		cfg, err := b(Deps{AgentID: "a"})
		if err != nil {
			t.Fatal(err)
		}
		if len(cfg.Spec.MCPServers) != 0 {
			t.Fatalf("unexpected MCP servers: %+v", cfg.Spec.MCPServers)
		}
	})
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/agentkind/ -run TestWireGateway -v`
Expected: compile FAILURE — `Deps.GatewayURL`/`GatewayKey` undefined.

- [ ] **Step 3: Implement**

In `internal/agentkind/registry.go`:

1. Add to `Deps`:

```go
	GatewayURL string // when set, append the platform gateway as an MCP server on the spec.
	GatewayKey string // optional Bearer key for the gateway ("" in open mode).
```

2. Add import `hmcp "github.com/sausheong/harness/tools/mcp"`.

3. Add `wireGateway` (after `wireMemory`):

```go
// wireGateway appends the platform MCP gateway to the agent's spec when the
// control plane injected RUNTIME_GATEWAY_URL. BuildRuntime then connects to it
// like any other MCP server; tools surface as mcp__gateway__<server>__<tool>.
func wireGateway(cfg *agentruntime.Config, d Deps) {
	if d.GatewayURL == "" {
		return
	}
	s := hmcp.ServerConfig{Name: "gateway", URL: d.GatewayURL}
	if d.GatewayKey != "" {
		s.Headers = map[string]string{"Authorization": "Bearer " + d.GatewayKey}
	}
	cfg.Spec.MCPServers = append(cfg.Spec.MCPServers, s)
}
```

4. Call it in BOTH builders, right after the existing `wireMemory` calls:

In `buildTestAgent`, after `if err := wireMemory(&cfg, d); err != nil { ... }`:

```go
	wireGateway(&cfg, d)
```

In `buildNutrition`, same position:

```go
	wireGateway(&cfg, d)
```

In `cmd/agentd/main.go`, extend the env reads (after `memoryOn := ...`):

```go
	gatewayURL := os.Getenv("RUNTIME_GATEWAY_URL")
	gatewayKey := os.Getenv("RUNTIME_GATEWAY_KEY")
```

and the Deps literal:

```go
	cfg, err := build(agentkind.Deps{
		AgentID: agentID, DB: db, Tenant: tenant, Memory: memoryOn,
		GatewayURL: gatewayURL, GatewayKey: gatewayKey,
	})
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/agentkind/ -v && go build ./...`
Expected: ALL PASS, clean build.

- [ ] **Step 5: Commit**

```bash
git add internal/agentkind/ cmd/agentd/
git commit -m "feat(gateway): agentd wires injected gateway env into AgentSpec.MCPServers"
```

---

### Task 6: runtimed assembly — build Manager, mount routes, self-URL, fail-closed key check

**Files:**
- Modify: `cmd/runtimed/main.go`
- Test: build + existing suites (assembly glue is exercised end-to-end in Task 7; the derivation helper gets a unit test here)

- [ ] **Step 1: Write the failing test for the self-URL derivation helper**

The helper lives in `cmd/runtimed` (package main). Create `cmd/runtimed/gateway_url_test.go`:

```go
package main

import "testing"

func TestGatewaySelfURL(t *testing.T) {
	cases := []struct {
		selfURL, ctlAddr, want string
	}{
		{"", ":8080", "http://127.0.0.1:8080/gateway/mcp"},
		{"", "0.0.0.0:9090", "http://127.0.0.1:9090/gateway/mcp"},
		{"", "127.0.0.1:8081", "http://127.0.0.1:8081/gateway/mcp"},
		{"", "10.0.0.5:8080", "http://10.0.0.5:8080/gateway/mcp"},
		{"http://gw.example.com", ":8080", "http://gw.example.com/gateway/mcp"},
		{"http://gw.example.com/", ":8080", "http://gw.example.com/gateway/mcp"},
	}
	for _, c := range cases {
		if got := gatewaySelfURL(c.selfURL, c.ctlAddr); got != c.want {
			t.Errorf("gatewaySelfURL(%q,%q) = %q, want %q", c.selfURL, c.ctlAddr, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./cmd/runtimed/ -v`
Expected: compile FAILURE — `gatewaySelfURL` undefined.

- [ ] **Step 3: Implement in cmd/runtimed/main.go**

1. Add imports: `"net"`, `"strings"`, `"github.com/sausheong/runtime/internal/gateway"`.

2. Add the helper:

```go
// gatewaySelfURL derives the URL agents use to reach the gateway. An explicit
// self_url wins; otherwise it comes from the control-plane listen address with
// a wildcard/empty host rewritten to loopback (agents are local subprocesses).
func gatewaySelfURL(selfURL, ctlAddr string) string {
	if selfURL != "" {
		return strings.TrimRight(selfURL, "/") + "/gateway/mcp"
	}
	host, port, err := net.SplitHostPort(ctlAddr)
	if err != nil || host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
		if err == nil {
			// port from SplitHostPort is fine
		} else {
			// ctlAddr like ":8080": SplitHostPort succeeds with empty host,
			// so err here means something stranger — fall back to the raw addr.
			return "http://127.0.0.1" + ctlAddr + "/gateway/mcp"
		}
	}
	return "http://" + net.JoinHostPort(host, port) + "/gateway/mcp"
}
```

(Note: `net.SplitHostPort(":8080")` succeeds with host `""` — the err branch only triggers on malformed input. Keep the fallback anyway; it's harmless.)

3. In `main()`, after `reg := controlplane.NewRegistry(...)` and after `cfg` is loaded — but BEFORE the identity block (so a key misconfig fails before agents spawn) — add:

```go
	// Gateway (B1 M1): build the upstream manager when configured. Fail-closed
	// guard: identity on + a gateway:true agent whose tenant has no agent key
	// is a startup error (the agent would be unable to authenticate).
	var gwHandler *gateway.Handler
	if cfg.Gateway.Enabled() {
		gwURL := gatewaySelfURL(cfg.Gateway.SelfURL, ctlAddr)
		reg.SetGateway(gwURL, cfg.Gateway.AgentKeys)
		gwManager := gateway.NewManager(cfg.Gateway.Servers)
		gwManager.Start(ctx)
		defer gwManager.Close()
		gwHandler = gateway.NewHandler(gwManager)
		gwHandler.PrincipalFor = controlplane.PrincipalFromContext
		slog.Info("gateway enabled", "upstreams", len(cfg.Gateway.Servers), "url", gwURL)
	}
```

4. The fail-closed check needs `identityOn`, which is computed later. Move the gateway key check to just after `identityOn` is computed:

```go
	if identityOn && cfg.Gateway.Enabled() {
		for _, a := range cfg.Agents {
			if a.Gateway && cfg.Gateway.AgentKeys[a.Tenant] == "" {
				slog.Error("gateway agent has no agent_key for its tenant (identity is on)",
					"agent", a.ID, "tenant", a.Tenant)
				os.Exit(1)
			}
		}
	}
```

(Ordering note: `reg.SetGateway` + manager construction can stay before the identity block; only the fail-closed validation needs `identityOn`. Both run before the agent-spawn loop, so no subprocess is orphaned.)

5. Mount the routes: change `buildRoot`'s signature to accept the handler and mount inside:

```go
func buildRoot(reg *controlplane.Registry, adminS controlplane.AdminStore, consoleOIDC console.OIDCConfig, secretBroker controlplane.SecretAdmin, gw *gateway.Handler) http.Handler {
	apiMux := controlplane.NewAPI(reg)
	if adminS != nil {
		controlplane.RegisterAdmin(apiMux, adminS)
		controlplane.RegisterSecretAdmin(apiMux, adminS, secretBroker)
	}
	if gw != nil {
		apiMux.Handle("/gateway/mcp", gw.HTTP())
		apiMux.HandleFunc("GET /gateway/status", gw.Status)
	}
	consoleH := console.Handler(reg, consoleOIDC)
	root := http.NewServeMux()
	root.Handle("/ui", consoleH)
	root.Handle("/ui/", consoleH)
	root.Handle("/", apiMux)
	return root
}
```

Update BOTH `buildRoot(...)` call sites to pass `gwHandler` (it is nil when the gateway is disabled — routes 404 via the mux, exactly the spec's back-compat behavior... note: an unmounted pattern falls through to the apiMux default, verify a request to /gateway/mcp on a gateway-less config returns 404, which the `/` catch-all of NewAPI does NOT shadow because NewAPI registers only /healthz, /agents, /agents/{id}/ patterns).

- [ ] **Step 4: Build + test + verify**

Run: `go build ./... && go test ./cmd/runtimed/ -v && go test ./...`
Expected: clean build, helper test PASS, full unit suite PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/runtimed/
git commit -m "feat(gateway): runtimed assembly — manager lifecycle, routes, self-URL, fail-closed keys"
```

---

### Task 7: Through-serve integration test

**Files:**
- Create: `test/gateway_e2e_test.go`

The full proof: `runtimed` + a fake Streamable HTTP MCP upstream + a `gateway: true` test agent. The scripted provider calls the tool named `marker`; we cannot rename gateway tools per-agent, so the e2e instead asserts (a) the gateway endpoint federates the upstream over real HTTP with tools listable via an MCP client, and (b) a `gateway: true` agent boots, connects to the gateway at BuildRuntime time, and still completes a turn (proving the MCP connect path doesn't break the agent loop). Tool-call-through-agent is covered by unit tests at the gateway layer plus harness's own MCP adapter tests; the live proof (Task 9) covers the full chain with a real LLM.

- [ ] **Step 1: Write the test**

```go
//go:build integration

package test

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestGatewayE2E boots the WHOLE stack with a gateway section: runtimed
// federates a fake Streamable HTTP MCP upstream, an external MCP client lists
// and calls a tool through /gateway/mcp, and a gateway:true agent (which
// connects to the gateway during BuildRuntime) completes a scripted turn.
func TestGatewayE2E(t *testing.T) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatalf("ping postgres (is it running at %s?): %v", dsn, err)
	}
	mustExec(t, db, `DROP TABLE IF EXISTS markers`)
	mustExec(t, db, `CREATE TABLE markers (id BIGSERIAL PRIMARY KEY, ran_at TIMESTAMPTZ)`)
	mustExec(t, db, `DROP TABLE IF EXISTS session_events, sessions, agents CASCADE`)
	mustExec(t, db, `DROP SCHEMA IF EXISTS dbos CASCADE`)

	// Fake upstream MCP server over Streamable HTTP.
	upstream := sdk.NewServer(&sdk.Implementation{Name: "fake-upstream", Version: "v0"}, nil)
	upstream.AddTool(&sdk.Tool{
		Name:        "greet",
		Description: "greets",
		InputSchema: map[string]any{"type": "object"},
	}, func(_ context.Context, _ *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "hello from upstream"}}}, nil
	})
	upSrv := httptest.NewServer(sdk.NewStreamableHTTPHandler(
		func(*http.Request) *sdk.Server { return upstream }, nil))
	defer upSrv.Close()

	// Build binaries.
	tmp := t.TempDir()
	agentd := filepath.Join(tmp, "agentd")
	if out, err := exec.Command("go", "build", "-o", agentd, "../cmd/agentd").CombinedOutput(); err != nil {
		t.Fatalf("build agentd: %v\n%s", err, out)
	}
	runtimed := filepath.Join(tmp, "runtimed")
	if out, err := exec.Command("go", "build", "-o", runtimed, "../cmd/runtimed").CombinedOutput(); err != nil {
		t.Fatalf("build runtimed: %v\n%s", err, out)
	}

	// Config: one gateway:true agent + the fake upstream. Open mode (no
	// identity), so no agent_keys needed.
	cfgPath := filepath.Join(tmp, "runtime.yaml")
	cfg := "agents:\n" +
		"  - {id: gw-agent, name: GW, model: test/scripted, listen_addr: 127.0.0.1:8131, gateway: true}\n" +
		"gateway:\n" +
		"  servers:\n" +
		"    - {name: fake, url: " + upSrv.URL + "}\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	ctlAddr := "127.0.0.1:8130"
	cmd := exec.Command(runtimed)
	cmd.Env = append(os.Environ(),
		"RUNTIME_PG_DSN="+dsn,
		"RUNTIME_CTL_ADDR="+ctlAddr,
		"RUNTIME_AGENTD_BIN="+agentd,
		"RUNTIME_CONFIG="+cfgPath,
	)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	base := "http://" + ctlAddr
	waitURL(t, base+"/healthz", 15*time.Second)

	// (a) External MCP client federates through the gateway.
	cli := sdk.NewClient(&sdk.Implementation{Name: "e2e", Version: "v0"}, nil)
	sess, err := cli.Connect(context.Background(),
		&sdk.StreamableClientTransport{Endpoint: base + "/gateway/mcp"}, nil)
	if err != nil {
		t.Fatalf("connect gateway: %v", err)
	}
	defer sess.Close()
	lt, err := sess.ListTools(context.Background(), &sdk.ListToolsParams{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(lt.Tools) != 1 || lt.Tools[0].Name != "fake__greet" {
		t.Fatalf("want [fake__greet], got %+v", lt.Tools)
	}
	res, err := sess.CallTool(context.Background(), &sdk.CallToolParams{Name: "fake__greet"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("call errored: %+v", res.Content)
	}
	if txt := res.Content[0].(*sdk.TextContent).Text; txt != "hello from upstream" {
		t.Fatalf("wrong result: %q", txt)
	}

	// (b) Gateway status visible.
	stResp, err := http.Get(base + "/gateway/status")
	if err != nil {
		t.Fatal(err)
	}
	stBody := readAll(t, stResp)
	if !strings.Contains(stBody, `"fake"`) || !strings.Contains(stBody, `"up"`) {
		t.Fatalf("status missing upstream: %s", stBody)
	}

	// (c) The gateway:true agent — which connected to the gateway during
	// BuildRuntime — boots healthy and completes a scripted turn.
	waitURL(t, base+"/agents/gw-agent/healthz", 30*time.Second)
	_, body := invokeOn(t, base, "gw-agent")
	if !strings.Contains(body, "final answer") {
		t.Fatalf("gateway agent turn did not complete:\n%s", body)
	}
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return sb.String()
}
```

(`mustExec`, `waitURL`, `invokeOn` already exist in the `test` package — reuse them, do not redefine. If `mustExec` lives in another file with a different name, read `test/*.go` helpers first and adapt.)

- [ ] **Step 2: Run the integration test**

Postgres.app must be running. Run:

```bash
go test -tags integration ./test/ -run TestGatewayE2E -v -timeout 180s
```

Expected: PASS — federated list+call works, status shows `up`, the gateway agent completes its turn.

If the agent fails to boot: check that harness `BuildRuntime` connecting to `RUNTIME_GATEWAY_URL` succeeds — the gateway must be listening BEFORE agents spawn (runtimed's HTTP server starts after the agent loop, so the agent's MCP connect would fail and... see Step 3).

- [ ] **Step 3: Fix the bootstrap ordering issue (expected finding)**

`runtimed` starts the HTTP server AFTER spawning agents, but a `gateway: true` agent connects to `/gateway/mcp` during BuildRuntime at startup. Two acceptable resolutions, in order of preference:

1. **Supervisor saves us:** agentd crashes on MCP connect failure, the Supervisor backs off and respawns, and by the next attempt the control plane is up. The test then passes (within the 30s health window) with no code change. If so: add a comment in `cmd/runtimed/main.go` noting the deliberate reliance on respawn, and move on.
2. If respawn does NOT converge (e.g. agentd treats MCP failure as fatal-fast loop): in `cmd/runtimed/main.go`, start the HTTP server (srv goroutine) BEFORE the agent spawn loop, keeping the shutdown path identical. This is a small move of the `srv := ...` / `go func() { serveErr <- ... }()` block above the spawn loop.

Either way, re-run Step 2 until green. Document which path was taken in the commit message.

- [ ] **Step 4: Run the FULL integration suite to check for regressions**

```bash
go test -tags integration ./test/ -v -timeout 900s
```

Expected: ALL PASS (these tests share Postgres state and ports — they are not parallel; if a pre-existing test flakes, re-run it alone before suspecting the gateway change).

- [ ] **Step 5: Commit**

```bash
git add test/ cmd/runtimed/
git commit -m "test(gateway): through-serve e2e — federation, status, gateway:true agent boot"
```

---

### Task 8: Sample config + README

**Files:**
- Modify: `runtime.yaml` (commented example)
- Modify: `README.md` (gateway section)

- [ ] **Step 1: Add a commented gateway example to runtime.yaml**

Append to `runtime.yaml`:

```yaml
# Optional: MCP gateway — federate upstream MCP servers into one endpoint at
# /gateway/mcp. Agents opt in with `gateway: true` (the platform injects
# RUNTIME_GATEWAY_URL and, when identity is on, RUNTIME_GATEWAY_KEY).
#
# gateway:
#   self_url: http://127.0.0.1:8080      # base URL agents use; default derives from RUNTIME_CTL_ADDR
#   agent_keys:                          # tenant → service key for gateway:true agents (identity on)
#     default: ${GW_DEFAULT_KEY}
#   servers:
#     - name: fs                         # tools exposed as fs__<tool> (agents see mcp__gateway__fs__<tool>)
#       command: npx
#       args: ["-y", "@modelcontextprotocol/server-filesystem", "/data"]
#     - name: search
#       url: https://mcp.example.com/mcp
#       headers: {Authorization: "Bearer ${SEARCH_TOKEN}"}
#       tenants: [acme]                  # omit ⇒ visible to all tenants
```

- [ ] **Step 2: Add a README section**

Read `README.md` first; add a "MCP Gateway (B1 M1)" section following the structure of the existing Memory/Identity sections, covering: what it does (federation, one endpoint), config example (same as runtime.yaml above), auth (service key Bearer; open mode), tenancy (`tenants:` allowlist; invisible ⇒ tool-not-found), the `gateway: true` agent flag + injected env vars, failure semantics (degrade + reconnect; `isError` on dead upstream), `GET /gateway/status`, and M1 limitations (static config only; tools only — no resources/prompts; no REST adapters; no semantic search; operator-managed agent keys).

- [ ] **Step 3: Verify build/tests still green, then commit**

Run: `go vet ./... && go test ./... && go build ./...`
Expected: clean.

```bash
git add runtime.yaml README.md
git commit -m "docs(gateway): sample config + README section for MCP gateway M1"
```

---

### Task 9: Live proof + ROADMAP update (FINAL — requires operator)

**Files:**
- Modify: `ROADMAP.md`

- [ ] **Step 1: Live smoke against a real public MCP server**

This step needs a real stdio MCP server (npx). From the repo root:

```bash
cat > /tmp/runtime.gateway-live.yaml <<'EOF'
agents:
  - id: support
    name: Support Agent
    model: test/scripted
    listen_addr: 127.0.0.1:8101
    gateway: true
gateway:
  servers:
    - name: fs
      command: npx
      args: ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"]
EOF
go build -o /tmp/agentd ./cmd/agentd && go build -o /tmp/runtimed ./cmd/runtimed
RUNTIME_CONFIG=/tmp/runtime.gateway-live.yaml RUNTIME_AGENTD_BIN=/tmp/agentd /tmp/runtimed
```

Then in another shell, verify federation with any MCP client — simplest check via curl-level status + the e2e client pattern:

```bash
curl -s http://127.0.0.1:8080/gateway/status   # expect fs: state=up, tool_count>0
```

Record the outcome (status output + one successful tools/list via an MCP client) in the commit message or a `docs/superpowers/` note. If `npx` reference server flakes, any public stdio MCP server is acceptable.

- [ ] **Step 2: Update ROADMAP.md**

In §B "The other 5 sub-projects", item 1 (**Gateway**), append a "**First milestone DONE**" paragraph following the exact narrative pattern of the Memory/Identity milestone entries: what shipped (federation core: `/gateway/mcp` Streamable HTTP endpoint, static YAML upstreams stdio+HTTP, tenant-filtered visibility via Identity service keys, `gateway: true` agent opt-in with env injection, degrade+reconnect, `/gateway/status`), what remains (REST adapters, semantic tool search, dynamic registration, resources/prompts, console panel, auto-minted keys, rate limits), and the spec/plan paths. Also update the "**Checkpoint date**" and "**Current state**" header at the top.

- [ ] **Step 3: Final full verification**

```bash
go vet ./... && go build ./... && go test ./...
go test -tags integration ./test/ -timeout 900s
```

Expected: everything green.

- [ ] **Step 4: Commit**

```bash
git add ROADMAP.md
git commit -m "docs: ROADMAP through Gateway M1 (MCP federation core)"
```

---

## Completion

After all tasks pass: use the **superpowers:finishing-a-development-branch** skill to merge `feat/gateway-m1` to `master` (the M1–M3 flow).
