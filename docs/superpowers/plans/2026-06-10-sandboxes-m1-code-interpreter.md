# Sandboxes M1 — Code Interpreter Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A `cmd/sandboxd` MCP server giving agents an isolated, stateful Docker-backed code interpreter, federated behind the existing gateway with tenant-scoped ownership.

**Architecture:** sandboxd is a stdio MCP server (same go-sdk the gateway uses) wrapping an `internal/sandbox.Manager` that runs one locked-down container per sandbox session via a `backend` interface (real Docker impl + in-memory fake). The only gateway change is `forward_tenant: true` on a `gateway.servers` entry: the gateway strips any caller-supplied `__rt_tenant` argument and injects the authenticated principal's tenant before forwarding.

**Tech Stack:** Go 1.25, `github.com/modelcontextprotocol/go-sdk v1.5.0` (already a dep), `github.com/docker/docker/client` (new dep), existing `internal/gateway` + `internal/config`.

**Spec:** `docs/superpowers/specs/2026-06-10-sandboxes-m1-code-interpreter-design.md` — read it first.

**Branch:** work on `sandboxes-m1` off `master`.

**Conventions that apply to every task:**
- The `go` CLI is ground truth — LSP is broken by the `replace github.com/sausheong/harness => ../harness` directive. Verify with `go build ./...`, `go test ./...`, `go vet ./...`.
- Unit tests are hermetic (no Docker, no network). Integration tests use `//go:build integration` and need Postgres.app at `postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable`.
- Commit after every task (the steps say when).

---

## File map (what exists after this plan)

```
internal/config/config.go            MODIFY  ForwardTenant field + validation
internal/config/config_test.go       MODIFY  new validation tests
internal/gateway/manager.go          MODIFY  ForwardsTenant(toolName) lookup
internal/gateway/server.go           MODIFY  toolHandler tenant injection
internal/gateway/server_test.go      MODIFY  injection/strip tests
internal/sandbox/backend.go          CREATE  backend interface + ExecResult + fake backend
internal/sandbox/paths.go            CREATE  /workspace path confinement
internal/sandbox/tenant.go           CREATE  __rt_tenant pop helper
internal/sandbox/manager.go          CREATE  sessions, caps, tenancy, reaper
internal/sandbox/tools.go            CREATE  7 MCP tools registered on an sdk.Server
internal/sandbox/docker.go           CREATE  real Docker backend
internal/sandbox/*_test.go           CREATE  hermetic tests for all the above
cmd/sandboxd/main.go                 CREATE  env config, wire Manager, stdio serve
deploy/sandbox.Dockerfile            CREATE  bundled python image
Makefile                             MODIFY  sandbox-image + sandboxd build targets
test/gateway_sandbox_e2e_test.go     CREATE  through-serve e2e (fake backend via env)
```

---

### Task 1: Config — `forward_tenant` field + validation

**Files:**
- Modify: `internal/config/config.go` (GatewayServer struct ~line 78; Validate ~line 160)
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `internal/config/config_test.go`:

```go
func TestGatewayForwardTenantParsesAndValidates(t *testing.T) {
	// stdio upstream with forward_tenant: true is valid.
	cfg := mustLoad(t, `
agents:
  - {id: a, name: A, model: m, listen_addr: 127.0.0.1:1}
gateway:
  servers:
    - {name: sbx, command: /bin/sandboxd, forward_tenant: true}
`)
	if !cfg.Gateway.Servers[0].ForwardTenant {
		t.Fatal("forward_tenant: true not parsed")
	}
	// default is false
	cfg = mustLoad(t, `
agents:
  - {id: a, name: A, model: m, listen_addr: 127.0.0.1:1}
gateway:
  servers:
    - {name: sbx, command: /bin/sandboxd}
`)
	if cfg.Gateway.Servers[0].ForwardTenant {
		t.Fatal("forward_tenant should default false")
	}
}

func TestGatewayForwardTenantRejectsHTTPUpstream(t *testing.T) {
	_, err := loadString(`
agents:
  - {id: a, name: A, model: m, listen_addr: 127.0.0.1:1}
gateway:
  servers:
    - {name: sbx, url: http://example.com/mcp, forward_tenant: true}
`)
	if err == nil || !strings.Contains(err.Error(), "forward_tenant") {
		t.Fatalf("want forward_tenant validation error, got %v", err)
	}
}
```

Check how existing tests in `config_test.go` load YAML — there will be a helper that parses a string and runs `Validate()`. If the helpers are named differently (e.g. tests inline `yaml.Unmarshal` + `cfg.Validate()`), match the existing pattern instead of inventing `mustLoad`/`loadString`; keep the assertions identical.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/config/ -run TestGatewayForwardTenant -v`
Expected: FAIL — `ForwardTenant` undefined.

- [ ] **Step 3: Implement**

In `internal/config/config.go`, add the field to `GatewayServer` (after `Tenants`):

```go
	// ForwardTenant makes the gateway inject the calling principal's tenant
	// into forwarded tool-call arguments as the reserved "__rt_tenant" key
	// (stripping any caller-supplied value first). Only valid for stdio
	// (command:) upstreams: the trust argument is that a stdio child is
	// reachable ONLY through the gateway.
	ForwardTenant bool `yaml:"forward_tenant"`
```

In `Validate()`, inside the existing gateway-server loop (after the exactly-one-of-command-or-url check):

```go
		if s.ForwardTenant && s.URL != "" {
			return fmt.Errorf("config: gateway server %q: forward_tenant requires a stdio (command:) upstream", s.Name)
		}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/config/ -v`
Expected: all PASS (including pre-existing tests).

- [ ] **Step 5: Commit**

```bash
git add internal/config/
git commit -m "feat(config): forward_tenant on gateway stdio upstreams"
```

---

### Task 2: Gateway — tenant injection in toolHandler

**Files:**
- Modify: `internal/gateway/manager.go` (add `ForwardsTenant`)
- Modify: `internal/gateway/server.go` (`serverFor` ~line 173, `toolHandler` ~line 204)
- Test: `internal/gateway/server_test.go`

**Context:** `Manager.ToolsFor(tenant)` returns harness `tool.Tool`s renamed `<server>__<tool>`. `toolHandler(builtFor string, t tool.Tool)` executes `t.Execute(ctx, req.Params.Arguments)` where `Arguments` is `json.RawMessage`. The existing tests in `server_test.go` build a Manager with `WithDial` and a fake `upstreamConn` — follow that pattern; a fake conn's tool can capture the raw arguments it receives.

- [ ] **Step 1: Write the failing tests**

Append to `internal/gateway/server_test.go`. Use the file's existing fake-upstream helpers (there is a fake `upstreamConn`/dial setup used by e.g. `TestServerViewerCannotCall` — reuse it; the test below shows intent, adapt helper names to the file):

```go
// captureTool records the raw arguments Execute receives.
type captureTool struct {
	tool.Tool          // embed a fake tool from the existing helpers
	got json.RawMessage
}

func TestForwardTenantInjectsAndStrips(t *testing.T) {
	// Manager with one upstream named "sbx", ForwardTenant: true, one tool
	// "sbx__run" whose Execute captures its raw input.
	// Handler wired with a PrincipalFor returning tenant "acme", role operator.
	//
	// Call sbx__run with arguments {"x":1,"__rt_tenant":"evil"}.
	// Assert the captured raw arguments unmarshal to exactly
	// {"x":1,"__rt_tenant":"acme"} — caller's value stripped, principal's
	// tenant injected.
}

func TestForwardTenantOpenModeInjectsEmpty(t *testing.T) {
	// Same upstream, Handler with PrincipalFor = OpenMode.
	// Call with {"x":1}. Captured args must be {"x":1,"__rt_tenant":""}.
}

func TestNonForwardingUpstreamArgsUntouched(t *testing.T) {
	// Upstream WITHOUT ForwardTenant. Call with {"x":1,"__rt_tenant":"evil"}.
	// Captured args must be byte-identical to the input (no strip, no inject).
}
```

Write these as real tests against the file's actual helpers (read the top of `server_test.go` first). The three behaviors above are the contract; cover all three.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/gateway/ -run 'TestForwardTenant|TestNonForwarding' -v`
Expected: FAIL (compile error — `ForwardsTenant` undefined) or assertion failure.

- [ ] **Step 3: Implement**

`internal/gateway/manager.go` — add after `AllTools`:

```go
// ForwardsTenant reports whether the upstream serving the given gateway tool
// name (<server>__<tool>) has forward_tenant configured. Names without the
// "__" separator (e.g. search_tools) never forward.
func (m *Manager) ForwardsTenant(toolName string) bool {
	srv, _, ok := strings.Cut(toolName, "__")
	if !ok {
		return false
	}
	for _, u := range m.ups {
		if u.cfg.Name == srv {
			return u.cfg.ForwardTenant
		}
	}
	return false
}
```

`internal/gateway/server.go` — in `serverFor`, pass the flag:

```go
		}, h.toolHandler(key, t, h.m.ForwardsTenant(t.Name())))
```

and change `toolHandler`:

```go
func (h *Handler) toolHandler(builtFor string, t tool.Tool, forwardTenant bool) sdk.ToolHandler {
	return func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		p, ok := h.PrincipalFor(ctx)
		callerBase, _ := principalView(p, ok)
		builtBase := viewBase(builtFor)
		if callerBase != builtBase {
			return errResult("forbidden: session does not belong to this principal's view"), nil
		}
		if ok && !p.Superuser && p.Role == identity.RoleViewer {
			return errResult("forbidden: role viewer cannot call tools (requires operator)"), nil
		}
		args := req.Params.Arguments
		if forwardTenant {
			injected, err := injectTenant(args, p, ok)
			if err != nil {
				return errResult("invalid arguments: " + err.Error()), nil
			}
			args = injected
		}
		res, err := t.Execute(ctx, args)
		// ... rest unchanged ...
```

Add the helper (same file, after `toolHandler`):

```go
// injectTenant strips any caller-supplied __rt_tenant from raw JSON arguments
// and sets the authenticated principal's tenant. Open mode and superusers
// inject "" (the upstream maps it to its default-tenant rule). The agent can
// therefore never choose its own tenant.
func injectTenant(raw json.RawMessage, p identity.Principal, ok bool) (json.RawMessage, error) {
	m := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
	}
	tenant := ""
	if ok && !p.Superuser {
		tenant = p.TenantID
	}
	m["__rt_tenant"] = tenant
	return json.Marshal(m)
}
```

(Note `delete` is unnecessary — the map assignment overwrites any caller value; the strip-then-inject contract holds.)

- [ ] **Step 4: Run the full gateway test suite**

Run: `go test ./internal/gateway/ -v`
Expected: all PASS — the new tests AND every pre-existing test (the third argument to `toolHandler` changes its signature; the compiler finds all call sites).

- [ ] **Step 5: Commit**

```bash
git add internal/gateway/
git commit -m "feat(gateway): inject caller tenant into forward_tenant upstreams"
```

---

### Task 3: sandbox — path confinement + tenant pop

**Files:**
- Create: `internal/sandbox/paths.go`
- Create: `internal/sandbox/tenant.go`
- Test: `internal/sandbox/paths_test.go`, `internal/sandbox/tenant_test.go`

- [ ] **Step 1: Write the failing tests**

`internal/sandbox/paths_test.go`:

```go
package sandbox

import "testing"

func TestConfinePath(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"data.csv", "/workspace/data.csv", true},
		{"sub/dir/f.txt", "/workspace/sub/dir/f.txt", true},
		{"/workspace/f.txt", "/workspace/f.txt", true},
		{"/workspace/sub/../f.txt", "/workspace/f.txt", true},
		{"", "", false},
		{"..", "", false},
		{"../etc/passwd", "", false},
		{"/etc/passwd", "", false},
		{"/workspace/../etc/passwd", "", false},
		{"sub/../../etc", "", false},
		{"/workspacefake/f.txt", "", false}, // prefix trick
	}
	for _, c := range cases {
		got, err := confinePath(c.in)
		if c.ok && (err != nil || got != c.want) {
			t.Errorf("confinePath(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
		if !c.ok && err == nil {
			t.Errorf("confinePath(%q) = %q; want error", c.in, got)
		}
	}
}
```

`internal/sandbox/tenant_test.go`:

```go
package sandbox

import (
	"encoding/json"
	"testing"
)

func TestPopTenant(t *testing.T) {
	tenant, rest, err := popTenant(json.RawMessage(`{"__rt_tenant":"acme","x":1}`))
	if err != nil || tenant != "acme" {
		t.Fatalf("got %q, %v", tenant, err)
	}
	var m map[string]any
	if err := json.Unmarshal(rest, &m); err != nil {
		t.Fatal(err)
	}
	if _, present := m["__rt_tenant"]; present {
		t.Fatal("__rt_tenant not removed from rest")
	}
	if m["x"] != float64(1) {
		t.Fatal("other args lost")
	}
}

func TestPopTenantEmptyMapsToDefault(t *testing.T) {
	// "" (open mode / superuser) and absent both map to "default".
	for _, raw := range []string{`{"__rt_tenant":""}`, `{}`, ``} {
		tenant, _, err := popTenant(json.RawMessage(raw))
		if err != nil || tenant != "default" {
			t.Fatalf("popTenant(%q) = %q, %v; want default", raw, tenant, err)
		}
	}
}

func TestPopTenantBadJSON(t *testing.T) {
	if _, _, err := popTenant(json.RawMessage(`not json`)); err == nil {
		t.Fatal("want error")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/sandbox/ -v`
Expected: FAIL — package doesn't exist yet / functions undefined.

- [ ] **Step 3: Implement**

`internal/sandbox/paths.go`:

```go
// Package sandbox implements the Docker-backed code-interpreter sessions
// served by cmd/sandboxd as MCP tools behind the platform gateway.
package sandbox

import (
	"fmt"
	"path"
	"strings"
)

// workspace is the only writable directory in a sandbox container (a tmpfs).
const workspace = "/workspace"

// confinePath resolves a user-supplied path strictly under /workspace.
// Relative paths are joined to /workspace; absolute paths must already be
// inside it. Anything escaping after cleaning (.., absolute elsewhere,
// /workspaceX prefix tricks) is rejected. Symlink tricks are moot: file I/O
// goes through the Docker copy API, never a shell.
func confinePath(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("path is empty")
	}
	if !strings.HasPrefix(p, "/") {
		p = workspace + "/" + p
	}
	clean := path.Clean(p)
	if clean != workspace && !strings.HasPrefix(clean, workspace+"/") {
		return "", fmt.Errorf("path %q is outside %s", p, workspace)
	}
	return clean, nil
}
```

`internal/sandbox/tenant.go`:

```go
package sandbox

import "encoding/json"

// tenantKey is the reserved argument the gateway injects for
// forward_tenant upstreams. sandboxd trusts it because it is a stdio child
// reachable only through the gateway.
const tenantKey = "__rt_tenant"

// defaultTenant mirrors Identity M1's absent-tenant rule.
const defaultTenant = "default"

// popTenant extracts and removes the reserved tenant key from raw JSON tool
// arguments, returning the remaining arguments for normal decoding. An empty
// or absent tenant maps to "default".
func popTenant(raw json.RawMessage) (tenant string, rest json.RawMessage, err error) {
	m := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &m); err != nil {
			return "", nil, err
		}
	}
	if v, ok := m[tenantKey].(string); ok {
		tenant = v
	}
	delete(m, tenantKey)
	if tenant == "" {
		tenant = defaultTenant
	}
	rest, err = json.Marshal(m)
	return tenant, rest, err
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/sandbox/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/sandbox/
git commit -m "feat(sandbox): path confinement + reserved-tenant argument pop"
```

---

### Task 4: sandbox — backend interface + in-memory fake

**Files:**
- Create: `internal/sandbox/backend.go`
- Test: `internal/sandbox/backend_test.go`

The fake is NOT test-only scaffolding — `RUNTIME_SANDBOX_FAKE=1` makes sandboxd serve it (Task 7), which is how the through-serve e2e runs without Docker. So it lives in the package proper and gets its own tests.

- [ ] **Step 1: Write the failing tests**

`internal/sandbox/backend_test.go`:

```go
package sandbox

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestFakeBackendLifecycle(t *testing.T) {
	ctx := context.Background()
	be := NewFakeBackend()

	id, err := be.Create(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}

	// Files round-trip.
	if err := be.WriteFile(ctx, id, "/workspace/a.txt", []byte("hello")); err != nil {
		t.Fatal(err)
	}
	got, truncated, err := be.ReadFile(ctx, id, "/workspace/a.txt", 1024)
	if err != nil || truncated || string(got) != "hello" {
		t.Fatalf("got %q, trunc=%v, err=%v", got, truncated, err)
	}

	// ReadFile honors the limit.
	got, truncated, err = be.ReadFile(ctx, id, "/workspace/a.txt", 3)
	if err != nil || !truncated || string(got) != "hel" {
		t.Fatalf("got %q, trunc=%v, err=%v", got, truncated, err)
	}

	// Exec echoes a canned result and records the argv.
	res, err := be.Exec(ctx, id, []string{"python3", "-c", "print(1)"}, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, "python3 -c print(1)") {
		t.Fatalf("unexpected exec result: %+v", res)
	}

	// Remove forgets the container.
	if err := be.Remove(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, _, err := be.ReadFile(ctx, id, "/workspace/a.txt", 10); err == nil {
		t.Fatal("read after remove should error")
	}
}

func TestFakeBackendLeftovers(t *testing.T) {
	ctx := context.Background()
	be := NewFakeBackend()
	a, _ := be.Create(ctx, "t1")
	b, _ := be.Create(ctx, "t2")
	ids, err := be.ListLeftovers(ctx)
	if err != nil || len(ids) != 2 {
		t.Fatalf("got %v, %v", ids, err)
	}
	_ = be.Remove(ctx, a)
	ids, _ = be.ListLeftovers(ctx)
	if len(ids) != 1 || ids[0] != b {
		t.Fatalf("got %v", ids)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/sandbox/ -run TestFakeBackend -v`
Expected: FAIL — types undefined.

- [ ] **Step 3: Implement**

`internal/sandbox/backend.go`:

```go
package sandbox

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ExecResult is the outcome of one exec inside a sandbox container.
type ExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	TimedOut bool
	Duration time.Duration
}

// Backend abstracts the container engine. The real implementation is
// dockerBackend (docker.go); fakeBackend serves hermetic tests and the
// RUNTIME_SANDBOX_FAKE e2e mode.
type Backend interface {
	// Create starts one locked-down sandbox container and returns its id.
	Create(ctx context.Context, tenant string) (containerID string, err error)
	// Exec runs argv inside the container with a wall-clock timeout that
	// kills the process (never the container).
	Exec(ctx context.Context, containerID string, argv []string, timeout time.Duration) (ExecResult, error)
	// WriteFile/ReadFile move bytes in and out of the container. path is
	// already confined (callers run confinePath first). ReadFile returns at
	// most limit bytes, reporting truncation.
	WriteFile(ctx context.Context, containerID, path string, content []byte) error
	ReadFile(ctx context.Context, containerID, path string, limit int) (content []byte, truncated bool, err error)
	// Remove force-removes the container. Removing an unknown id is an error.
	Remove(ctx context.Context, containerID string) error
	// ListLeftovers returns ids of all runtime.sandbox=1 containers (for
	// reap-on-start).
	ListLeftovers(ctx context.Context) ([]string, error)
}

// fakeBackend is an in-memory backend: files are a map, exec echoes its argv.
// It backs unit tests and sandboxd's RUNTIME_SANDBOX_FAKE mode (the
// through-serve e2e without Docker).
type fakeBackend struct {
	mu    sync.Mutex
	next  int
	boxes map[string]map[string][]byte // containerID → path → content
}

func NewFakeBackend() *fakeBackend {
	return &fakeBackend{boxes: map[string]map[string][]byte{}}
}

func (f *fakeBackend) Create(_ context.Context, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	id := fmt.Sprintf("fake-%d", f.next)
	f.boxes[id] = map[string][]byte{}
	return id, nil
}

func (f *fakeBackend) Exec(_ context.Context, id string, argv []string, _ time.Duration) (ExecResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.boxes[id]; !ok {
		return ExecResult{}, fmt.Errorf("no such container %s", id)
	}
	return ExecResult{Stdout: "fake exec: " + strings.Join(argv, " ")}, nil
}

func (f *fakeBackend) WriteFile(_ context.Context, id, path string, content []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	box, ok := f.boxes[id]
	if !ok {
		return fmt.Errorf("no such container %s", id)
	}
	box[path] = append([]byte(nil), content...)
	return nil
}

func (f *fakeBackend) ReadFile(_ context.Context, id, path string, limit int) ([]byte, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	box, ok := f.boxes[id]
	if !ok {
		return nil, false, fmt.Errorf("no such container %s", id)
	}
	c, ok := box[path]
	if !ok {
		return nil, false, fmt.Errorf("no such file %s", path)
	}
	if len(c) > limit {
		return append([]byte(nil), c[:limit]...), true, nil
	}
	return append([]byte(nil), c...), false, nil
}

func (f *fakeBackend) Remove(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.boxes[id]; !ok {
		return fmt.Errorf("no such container %s", id)
	}
	delete(f.boxes, id)
	return nil
}

func (f *fakeBackend) ListLeftovers(_ context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]string, 0, len(f.boxes))
	for id := range f.boxes {
		ids = append(ids, id)
	}
	return ids, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/sandbox/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/sandbox/
git commit -m "feat(sandbox): backend seam + in-memory fake"
```

---

### Task 5: sandbox — Manager (sessions, caps, tenancy, reaper)

**Files:**
- Create: `internal/sandbox/manager.go`
- Test: `internal/sandbox/manager_test.go`

- [ ] **Step 1: Write the failing tests**

`internal/sandbox/manager_test.go`:

```go
package sandbox

import (
	"context"
	"strings"
	"testing"
	"time"
)

func testManager(t *testing.T) (*Manager, *fakeBackend, *time.Time) {
	t.Helper()
	be := NewFakeBackend()
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	m := NewManager(be, Config{
		MaxPerTenant: 2,
		IdleTTL:      10 * time.Minute,
		MaxLifetime:  time.Hour,
		ReadLimit:    1024,
	})
	m.now = func() time.Time { return now }
	return m, be, &now
}

func TestCreateExecCloseRoundTrip(t *testing.T) {
	ctx := context.Background()
	m, _, _ := testManager(t)

	s, err := m.Create(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s.ID, "sbx-") || len(s.ID) != 4+32 {
		t.Fatalf("bad id %q", s.ID)
	}

	res, err := m.ExecCode(ctx, "acme", s.ID, "print(1)", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Stdout, "python3") {
		t.Fatalf("exec didn't run python3: %+v", res)
	}

	if err := m.Close(ctx, "acme", s.ID); err != nil {
		t.Fatal(err)
	}
	// Close is idempotent.
	if err := m.Close(ctx, "acme", s.ID); err != nil {
		t.Fatal("second close should be nil (idempotent)")
	}
	// Calls after close: not found.
	if _, err := m.ExecCode(ctx, "acme", s.ID, "x", 0); err == nil {
		t.Fatal("exec after close should fail")
	}
}

func TestCrossTenantHiddenAsNotFound(t *testing.T) {
	ctx := context.Background()
	m, _, _ := testManager(t)
	s, _ := m.Create(ctx, "acme")

	_, errCross := m.ExecCode(ctx, "globex", s.ID, "x", 0)
	_, errMissing := m.ExecCode(ctx, "globex", "sbx-doesnotexist", "x", 0)
	if errCross == nil || errMissing == nil {
		t.Fatal("both must error")
	}
	if errCross.Error() != errMissing.Error() {
		t.Fatalf("cross-tenant error %q must equal missing-id error %q (existence hidden)",
			errCross, errMissing)
	}

	// List is tenant-scoped.
	if got := m.List("globex"); len(got) != 0 {
		t.Fatalf("globex sees %d sandboxes", len(got))
	}
	if got := m.List("acme"); len(got) != 1 {
		t.Fatalf("acme sees %d sandboxes", len(got))
	}
}

func TestPerTenantCap(t *testing.T) {
	ctx := context.Background()
	m, _, _ := testManager(t) // cap 2
	if _, err := m.Create(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(ctx, "acme"); err == nil ||
		!strings.Contains(err.Error(), "limit") {
		t.Fatalf("third create should hit the cap, got %v", err)
	}
	// Another tenant is unaffected.
	if _, err := m.Create(ctx, "globex"); err != nil {
		t.Fatal(err)
	}
}

func TestExecTimeoutClamped(t *testing.T) {
	ctx := context.Background()
	m, _, _ := testManager(t)
	s, _ := m.Create(ctx, "acme")
	// 0 ⇒ default 30s; 999s ⇒ clamped to 120s. The fake backend doesn't
	// time anything; we assert via the manager's clamp helper directly.
	if d := clampTimeout(0); d != 30*time.Second {
		t.Fatalf("default = %v", d)
	}
	if d := clampTimeout(999); d != 120*time.Second {
		t.Fatalf("clamp = %v", d)
	}
	if d := clampTimeout(60); d != 60*time.Second {
		t.Fatalf("pass-through = %v", d)
	}
	_ = s
}

func TestFilesConfinedAndLimited(t *testing.T) {
	ctx := context.Background()
	m, _, _ := testManager(t)
	s, _ := m.Create(ctx, "acme")

	if err := m.WriteFile(ctx, "acme", s.ID, "../etc/passwd", []byte("x")); err == nil {
		t.Fatal("escape should be rejected")
	}
	if err := m.WriteFile(ctx, "acme", s.ID, "big.txt", []byte(strings.Repeat("a", 2048))); err != nil {
		t.Fatal(err)
	}
	content, truncated, err := m.ReadFile(ctx, "acme", s.ID, "big.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !truncated || len(content) != 1024 { // ReadLimit from testManager
		t.Fatalf("len=%d truncated=%v", len(content), truncated)
	}
}

func TestReaperIdleAndMaxLifetime(t *testing.T) {
	ctx := context.Background()
	m, be, now := testManager(t)

	idle, _ := m.Create(ctx, "acme")
	busy, _ := m.Create(ctx, "acme")

	// 9 minutes pass; busy is touched, idle is not.
	*now = now.Add(9 * time.Minute)
	if _, err := m.ExecCode(ctx, "acme", busy.ID, "x", 0); err != nil {
		t.Fatal(err)
	}
	// 2 more minutes: idle crosses the 10m TTL, busy doesn't.
	*now = now.Add(2 * time.Minute)
	m.ReapOnce(ctx)
	if _, err := m.ExecCode(ctx, "acme", idle.ID, "x", 0); err == nil {
		t.Fatal("idle sandbox should be reaped")
	}
	if _, err := m.ExecCode(ctx, "acme", busy.ID, "x", 0); err != nil {
		t.Fatalf("busy sandbox reaped early: %v", err)
	}

	// Max lifetime: even a constantly-touched sandbox dies at 1h.
	*now = now.Add(50 * time.Minute) // total > 1h since create
	m.ReapOnce(ctx)
	if _, err := m.ExecCode(ctx, "acme", busy.ID, "x", 0); err == nil {
		t.Fatal("sandbox past max lifetime should be reaped")
	}
	// Containers actually removed from the backend.
	ids, _ := be.ListLeftovers(ctx)
	if len(ids) != 0 {
		t.Fatalf("backend still has %v", ids)
	}
}

func TestReapStartupRemovesLeftovers(t *testing.T) {
	ctx := context.Background()
	be := NewFakeBackend()
	_, _ = be.Create(ctx, "old1")
	_, _ = be.Create(ctx, "old2")
	m := NewManager(be, Config{MaxPerTenant: 5})
	if err := m.ReapStartup(ctx); err != nil {
		t.Fatal(err)
	}
	ids, _ := be.ListLeftovers(ctx)
	if len(ids) != 0 {
		t.Fatalf("leftovers not reaped: %v", ids)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/sandbox/ -run 'TestCreate|TestCross|TestPerTenant|TestExecTimeout|TestFiles|TestReap' -v`
Expected: FAIL — Manager undefined.

- [ ] **Step 3: Implement**

`internal/sandbox/manager.go`:

```go
package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// errNoSandbox is the single not-found error: a wrong-tenant id and a
// nonexistent id are indistinguishable (existence hidden, Identity M1
// posture).
var errNoSandbox = errors.New("no such sandbox")

const (
	defaultExecTimeout = 30 * time.Second
	maxExecTimeout     = 120 * time.Second
)

// Config bounds Manager behavior; zero fields get sane defaults in
// NewManager where noted.
type Config struct {
	MaxPerTenant int           // concurrent sandboxes per tenant (default 5)
	IdleTTL      time.Duration // close after this long unused (default 10m)
	MaxLifetime  time.Duration // close this long after create (default 1h)
	ReadLimit    int           // read_file byte cap (default 256 KiB)
}

// Session is one live sandbox.
type Session struct {
	ID          string
	Tenant      string
	ContainerID string
	CreatedAt   time.Time
	LastUsed    time.Time
	ExpiresAt   time.Time // CreatedAt + MaxLifetime
}

// Manager owns the sandbox sessions over a container backend.
type Manager struct {
	be  Backend
	cfg Config
	now func() time.Time

	mu       sync.Mutex
	sessions map[string]*Session
}

func NewManager(be Backend, cfg Config) *Manager {
	if cfg.MaxPerTenant <= 0 {
		cfg.MaxPerTenant = 5
	}
	if cfg.IdleTTL <= 0 {
		cfg.IdleTTL = 10 * time.Minute
	}
	if cfg.MaxLifetime <= 0 {
		cfg.MaxLifetime = time.Hour
	}
	if cfg.ReadLimit <= 0 {
		cfg.ReadLimit = 256 << 10
	}
	return &Manager{be: be, cfg: cfg, now: time.Now, sessions: map[string]*Session{}}
}

func newSandboxID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return "sbx-" + hex.EncodeToString(b)
}

// Create starts a new sandbox for tenant, enforcing the per-tenant cap.
func (m *Manager) Create(ctx context.Context, tenant string) (*Session, error) {
	m.mu.Lock()
	n := 0
	for _, s := range m.sessions {
		if s.Tenant == tenant {
			n++
		}
	}
	if n >= m.cfg.MaxPerTenant {
		m.mu.Unlock()
		return nil, fmt.Errorf("sandbox limit reached (%d per tenant): close one with close_sandbox", m.cfg.MaxPerTenant)
	}
	m.mu.Unlock()

	cid, err := m.be.Create(ctx, tenant)
	if err != nil {
		return nil, fmt.Errorf("sandbox backend unavailable: %w", err)
	}
	now := m.now()
	s := &Session{
		ID: newSandboxID(), Tenant: tenant, ContainerID: cid,
		CreatedAt: now, LastUsed: now, ExpiresAt: now.Add(m.cfg.MaxLifetime),
	}
	m.mu.Lock()
	m.sessions[s.ID] = s
	m.mu.Unlock()
	return s, nil
}

// lookup returns the session if it exists AND belongs to tenant; otherwise
// errNoSandbox. Touches LastUsed.
func (m *Manager) lookup(tenant, id string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok || s.Tenant != tenant {
		return nil, errNoSandbox
	}
	s.LastUsed = m.now()
	return s, nil
}

func clampTimeout(seconds int) time.Duration {
	if seconds <= 0 {
		return defaultExecTimeout
	}
	d := time.Duration(seconds) * time.Second
	if d > maxExecTimeout {
		return maxExecTimeout
	}
	return d
}

// ExecCode runs python3 -c code in the sandbox's workspace.
func (m *Manager) ExecCode(ctx context.Context, tenant, id, code string, timeoutS int) (ExecResult, error) {
	s, err := m.lookup(tenant, id)
	if err != nil {
		return ExecResult{}, err
	}
	return m.be.Exec(ctx, s.ContainerID, []string{"python3", "-c", code}, clampTimeout(timeoutS))
}

// ExecCommand runs sh -c command in the sandbox's workspace.
func (m *Manager) ExecCommand(ctx context.Context, tenant, id, command string, timeoutS int) (ExecResult, error) {
	s, err := m.lookup(tenant, id)
	if err != nil {
		return ExecResult{}, err
	}
	return m.be.Exec(ctx, s.ContainerID, []string{"sh", "-c", command}, clampTimeout(timeoutS))
}

// WriteFile writes content to a confined path in the workspace.
func (m *Manager) WriteFile(ctx context.Context, tenant, id, p string, content []byte) error {
	confined, err := confinePath(p)
	if err != nil {
		return err
	}
	s, err := m.lookup(tenant, id)
	if err != nil {
		return err
	}
	return m.be.WriteFile(ctx, s.ContainerID, confined, content)
}

// ReadFile reads a confined path, capped at cfg.ReadLimit bytes.
func (m *Manager) ReadFile(ctx context.Context, tenant, id, p string) ([]byte, bool, error) {
	confined, err := confinePath(p)
	if err != nil {
		return nil, false, err
	}
	s, err := m.lookup(tenant, id)
	if err != nil {
		return nil, false, err
	}
	return m.be.ReadFile(ctx, s.ContainerID, confined, m.cfg.ReadLimit)
}

// List returns tenant's live sessions (no cross-tenant visibility).
func (m *Manager) List(tenant string) []*Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Session
	for _, s := range m.sessions {
		if s.Tenant == tenant {
			c := *s
			out = append(out, &c)
		}
	}
	return out
}

// Close removes the sandbox. Unknown/foreign ids are a no-op (idempotent
// close is friendlier for agents than a not-found error, and reveals
// nothing cross-tenant).
func (m *Manager) Close(ctx context.Context, tenant, id string) error {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if !ok || s.Tenant != tenant {
		m.mu.Unlock()
		return nil
	}
	delete(m.sessions, id)
	m.mu.Unlock()
	if err := m.be.Remove(ctx, s.ContainerID); err != nil {
		slog.Warn("sandbox: remove container", "sandbox", id, "err", err)
	}
	return nil
}

// ReapOnce closes sessions past IdleTTL or MaxLifetime. Called periodically
// by StartReaper; exported for tests.
func (m *Manager) ReapOnce(ctx context.Context) {
	now := m.now()
	m.mu.Lock()
	var doomed []*Session
	for id, s := range m.sessions {
		if now.Sub(s.LastUsed) > m.cfg.IdleTTL || now.After(s.ExpiresAt) {
			doomed = append(doomed, s)
			delete(m.sessions, id)
		}
	}
	m.mu.Unlock()
	for _, s := range doomed {
		if err := m.be.Remove(ctx, s.ContainerID); err != nil {
			slog.Warn("sandbox: reap remove", "sandbox", s.ID, "err", err)
		} else {
			slog.Info("sandbox: reaped", "sandbox", s.ID, "tenant", s.Tenant)
		}
	}
}

// StartReaper runs ReapOnce every interval until ctx is done.
func (m *Manager) StartReaper(ctx context.Context, interval time.Duration) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.ReapOnce(ctx)
			}
		}
	}()
}

// ReapStartup removes ALL leftover sandbox containers from a previous
// sandboxd run (labeled runtime.sandbox=1 in the real backend).
func (m *Manager) ReapStartup(ctx context.Context) error {
	ids, err := m.be.ListLeftovers(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := m.be.Remove(ctx, id); err != nil {
			slog.Warn("sandbox: startup reap", "container", id, "err", err)
		}
	}
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/sandbox/ -v`
Expected: PASS. Also `go vet ./internal/sandbox/`.

- [ ] **Step 5: Commit**

```bash
git add internal/sandbox/
git commit -m "feat(sandbox): session manager with tenancy, caps, reaper"
```

---

### Task 6: sandbox — the 7 MCP tools

**Files:**
- Create: `internal/sandbox/tools.go`
- Test: `internal/sandbox/tools_test.go`

**Context:** `sdk "github.com/modelcontextprotocol/go-sdk/mcp"`. Server-side handler signature: `func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error)`; `req.Params.Arguments` is `json.RawMessage`. Tool errors are `&sdk.CallToolResult{IsError: true, Content: [&sdk.TextContent{...}]}` — never Go errors (those kill the request). Tool names here are UNPREFIXED (the gateway adds `sandbox__`).

- [ ] **Step 1: Write the failing tests**

`internal/sandbox/tools_test.go`. Test through an in-memory MCP client/server pair — the SDK supports `sdk.NewInMemoryTransports()`:

```go
package sandbox

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// startServer wires a Manager (fake backend) into an MCP server and returns
// a connected client session.
func startServer(t *testing.T) *sdk.ClientSession {
	t.Helper()
	m := NewManager(NewFakeBackend(), Config{MaxPerTenant: 2})
	srv := NewServer(m)
	ct, st := sdk.NewInMemoryTransports()
	ctx := context.Background()
	go func() { _ = srv.Run(ctx, st) }()
	cli := sdk.NewClient(&sdk.Implementation{Name: "t", Version: "v"}, nil)
	sess, err := cli.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

func call(t *testing.T, sess *sdk.ClientSession, name string, args map[string]any) (*sdk.CallToolResult, map[string]any) {
	t.Helper()
	res, err := sess.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	out := map[string]any{}
	if len(res.Content) > 0 {
		if tc, ok := res.Content[0].(*sdk.TextContent); ok {
			_ = json.Unmarshal([]byte(tc.Text), &out)
		}
	}
	return res, out
}

func TestToolsListedAndLifecycle(t *testing.T) {
	sess := startServer(t)
	lt, err := sess.ListTools(context.Background(), &sdk.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"create_sandbox": true, "execute_code": true, "run_command": true,
		"write_file": true, "read_file": true, "list_sandboxes": true,
		"close_sandbox": true,
	}
	if len(lt.Tools) != len(want) {
		t.Fatalf("want %d tools, got %d", len(want), len(lt.Tools))
	}
	for _, tool := range lt.Tools {
		if !want[tool.Name] {
			t.Fatalf("unexpected tool %q", tool.Name)
		}
	}

	// create → execute → write/read → list → close, all as tenant acme.
	res, out := call(t, sess, "create_sandbox", map[string]any{"__rt_tenant": "acme"})
	if res.IsError {
		t.Fatalf("create: %+v", res.Content)
	}
	id, _ := out["sandbox_id"].(string)
	if !strings.HasPrefix(id, "sbx-") {
		t.Fatalf("bad sandbox_id %q", id)
	}

	res, out = call(t, sess, "execute_code", map[string]any{
		"__rt_tenant": "acme", "sandbox_id": id, "code": "print(40+2)",
	})
	if res.IsError {
		t.Fatalf("execute: %+v", res.Content)
	}
	if !strings.Contains(out["stdout"].(string), "python3") {
		t.Fatalf("stdout = %v", out["stdout"])
	}

	res, _ = call(t, sess, "write_file", map[string]any{
		"__rt_tenant": "acme", "sandbox_id": id, "path": "a.txt", "content": "hi",
	})
	if res.IsError {
		t.Fatalf("write: %+v", res.Content)
	}
	res, out = call(t, sess, "read_file", map[string]any{
		"__rt_tenant": "acme", "sandbox_id": id, "path": "a.txt",
	})
	if res.IsError || out["content"] != "hi" {
		t.Fatalf("read: %+v / %v", res.Content, out)
	}

	_, out = call(t, sess, "list_sandboxes", map[string]any{"__rt_tenant": "acme"})
	if n := len(out["sandboxes"].([]any)); n != 1 {
		t.Fatalf("list: %d", n)
	}
	// Cross-tenant list sees nothing.
	_, out = call(t, sess, "list_sandboxes", map[string]any{"__rt_tenant": "globex"})
	if sb, ok := out["sandboxes"].([]any); ok && len(sb) != 0 {
		t.Fatalf("globex sees %d", len(sb))
	}
	// Cross-tenant call: same error text as a missing id.
	resCross, _ := call(t, sess, "execute_code", map[string]any{
		"__rt_tenant": "globex", "sandbox_id": id, "code": "x",
	})
	resMissing, _ := call(t, sess, "execute_code", map[string]any{
		"__rt_tenant": "globex", "sandbox_id": "sbx-nope", "code": "x",
	})
	if !resCross.IsError || !resMissing.IsError {
		t.Fatal("both must be errors")
	}
	crossTxt := resCross.Content[0].(*sdk.TextContent).Text
	missTxt := resMissing.Content[0].(*sdk.TextContent).Text
	if crossTxt != missTxt {
		t.Fatalf("existence leaked: %q vs %q", crossTxt, missTxt)
	}

	res, out = call(t, sess, "close_sandbox", map[string]any{
		"__rt_tenant": "acme", "sandbox_id": id,
	})
	if res.IsError || out["closed"] != true {
		t.Fatalf("close: %+v %v", res.Content, out)
	}
}

func TestToolErrorsAreIsError(t *testing.T) {
	sess := startServer(t)
	// Bad path → isError, not a transport failure.
	res, _ := call(t, sess, "create_sandbox", map[string]any{"__rt_tenant": "acme"})
	var out map[string]any
	_ = json.Unmarshal([]byte(res.Content[0].(*sdk.TextContent).Text), &out)
	id := out["sandbox_id"].(string)
	res, _ = call(t, sess, "write_file", map[string]any{
		"__rt_tenant": "acme", "sandbox_id": id, "path": "../escape", "content": "x",
	})
	if !res.IsError {
		t.Fatal("path escape must be isError")
	}
	// Missing required field.
	res, _ = call(t, sess, "execute_code", map[string]any{"__rt_tenant": "acme"})
	if !res.IsError {
		t.Fatal("missing sandbox_id must be isError")
	}
	_ = time.Second
}
```

If `sdk.NewInMemoryTransports` has a different name in v1.5.0, check with `grep -rn "InMemoryTransport" ~/go/pkg/mod/github.com/modelcontextprotocol/go-sdk@v1.5.0/mcp/transport.go` and use what's there (it exists — the gateway's own unit tests connect in-memory; copy their connection pattern if simpler).

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/sandbox/ -run TestTools -v`
Expected: FAIL — `NewServer` undefined.

- [ ] **Step 3: Implement**

`internal/sandbox/tools.go`:

```go
package sandbox

import (
	"context"
	"encoding/json"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// NewServer builds the sandboxd MCP server: 7 tools over a Manager. Tool
// names are unprefixed — the gateway exposes them as sandbox__<name>. Every
// handler pops the gateway-injected __rt_tenant first; all failures are MCP
// isError results (never Go errors, which would kill the session).
func NewServer(m *Manager) *sdk.Server {
	srv := sdk.NewServer(&sdk.Implementation{Name: "runtime-sandboxd", Version: "m1"}, nil)

	add := func(name, desc string, schema string, h func(ctx context.Context, tenant string, args json.RawMessage) (any, error)) {
		srv.AddTool(&sdk.Tool{
			Name: name, Description: desc, InputSchema: json.RawMessage(schema),
		}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			tenant, rest, err := popTenant(req.Params.Arguments)
			if err != nil {
				return errResult("invalid arguments: " + err.Error()), nil
			}
			out, err := h(ctx, tenant, rest)
			if err != nil {
				return errResult(err.Error()), nil
			}
			b, err := json.Marshal(out)
			if err != nil {
				return errResult(err.Error()), nil
			}
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: string(b)}}}, nil
		})
	}

	const persistNote = " Files in /workspace persist across calls within the same sandbox; Python variables do NOT (each execution is a fresh process) — write intermediate results to files."

	add("create_sandbox",
		"Create an isolated code-execution sandbox (Python 3.12 + numpy/pandas/matplotlib; no network access). Returns a sandbox_id for use with the other sandbox tools."+persistNote,
		`{"type":"object","properties":{}}`,
		func(ctx context.Context, tenant string, _ json.RawMessage) (any, error) {
			s, err := m.Create(ctx, tenant)
			if err != nil {
				return nil, err
			}
			return map[string]any{"sandbox_id": s.ID, "expires_at": s.ExpiresAt.Format(time.RFC3339)}, nil
		})

	add("execute_code",
		"Run Python code in the sandbox (python3 -c, working dir /workspace). Returns stdout, stderr, exit_code."+persistNote,
		`{"type":"object","properties":{
			"sandbox_id":{"type":"string"},
			"code":{"type":"string","description":"Python source to execute"},
			"timeout_s":{"type":"integer","description":"seconds (default 30, max 120)"}
		},"required":["sandbox_id","code"]}`,
		func(ctx context.Context, tenant string, args json.RawMessage) (any, error) {
			var in struct {
				SandboxID string `json:"sandbox_id"`
				Code      string `json:"code"`
				TimeoutS  int    `json:"timeout_s"`
			}
			if err := decode(args, &in); err != nil {
				return nil, err
			}
			if in.SandboxID == "" || in.Code == "" {
				return nil, errMissing("sandbox_id and code")
			}
			res, err := m.ExecCode(ctx, tenant, in.SandboxID, in.Code, in.TimeoutS)
			if err != nil {
				return nil, err
			}
			return execOut(res), nil
		})

	add("run_command",
		"Run a shell command in the sandbox (sh -c, working dir /workspace). No network access."+persistNote,
		`{"type":"object","properties":{
			"sandbox_id":{"type":"string"},
			"command":{"type":"string"},
			"timeout_s":{"type":"integer","description":"seconds (default 30, max 120)"}
		},"required":["sandbox_id","command"]}`,
		func(ctx context.Context, tenant string, args json.RawMessage) (any, error) {
			var in struct {
				SandboxID string `json:"sandbox_id"`
				Command   string `json:"command"`
				TimeoutS  int    `json:"timeout_s"`
			}
			if err := decode(args, &in); err != nil {
				return nil, err
			}
			if in.SandboxID == "" || in.Command == "" {
				return nil, errMissing("sandbox_id and command")
			}
			res, err := m.ExecCommand(ctx, tenant, in.SandboxID, in.Command, in.TimeoutS)
			if err != nil {
				return nil, err
			}
			return execOut(res), nil
		})

	add("write_file",
		"Write a text file into the sandbox workspace (/workspace). Parent directories are created.",
		`{"type":"object","properties":{
			"sandbox_id":{"type":"string"},
			"path":{"type":"string","description":"relative to /workspace"},
			"content":{"type":"string"}
		},"required":["sandbox_id","path","content"]}`,
		func(ctx context.Context, tenant string, args json.RawMessage) (any, error) {
			var in struct {
				SandboxID string `json:"sandbox_id"`
				Path      string `json:"path"`
				Content   string `json:"content"`
			}
			if err := decode(args, &in); err != nil {
				return nil, err
			}
			if in.SandboxID == "" || in.Path == "" {
				return nil, errMissing("sandbox_id and path")
			}
			if err := m.WriteFile(ctx, tenant, in.SandboxID, in.Path, []byte(in.Content)); err != nil {
				return nil, err
			}
			return map[string]any{"path": in.Path, "bytes": len(in.Content)}, nil
		})

	add("read_file",
		"Read a text file from the sandbox workspace (/workspace). Capped at 256 KiB (truncated flag set beyond).",
		`{"type":"object","properties":{
			"sandbox_id":{"type":"string"},
			"path":{"type":"string","description":"relative to /workspace"}
		},"required":["sandbox_id","path"]}`,
		func(ctx context.Context, tenant string, args json.RawMessage) (any, error) {
			var in struct {
				SandboxID string `json:"sandbox_id"`
				Path      string `json:"path"`
			}
			if err := decode(args, &in); err != nil {
				return nil, err
			}
			if in.SandboxID == "" || in.Path == "" {
				return nil, errMissing("sandbox_id and path")
			}
			content, truncated, err := m.ReadFile(ctx, tenant, in.SandboxID, in.Path)
			if err != nil {
				return nil, err
			}
			return map[string]any{"content": string(content), "bytes": len(content), "truncated": truncated}, nil
		})

	add("list_sandboxes",
		"List your live sandboxes.",
		`{"type":"object","properties":{}}`,
		func(_ context.Context, tenant string, _ json.RawMessage) (any, error) {
			var out []map[string]any
			for _, s := range m.List(tenant) {
				out = append(out, map[string]any{
					"sandbox_id":   s.ID,
					"created_at":   s.CreatedAt.Format(time.RFC3339),
					"expires_at":   s.ExpiresAt.Format(time.RFC3339),
					"last_used_at": s.LastUsed.Format(time.RFC3339),
				})
			}
			if out == nil {
				out = []map[string]any{}
			}
			return map[string]any{"sandboxes": out}, nil
		})

	add("close_sandbox",
		"Close a sandbox and discard its workspace. Idempotent.",
		`{"type":"object","properties":{
			"sandbox_id":{"type":"string"}
		},"required":["sandbox_id"]}`,
		func(ctx context.Context, tenant string, args json.RawMessage) (any, error) {
			var in struct {
				SandboxID string `json:"sandbox_id"`
			}
			if err := decode(args, &in); err != nil {
				return nil, err
			}
			if in.SandboxID == "" {
				return nil, errMissing("sandbox_id")
			}
			if err := m.Close(ctx, tenant, in.SandboxID); err != nil {
				return nil, err
			}
			return map[string]any{"closed": true}, nil
		})

	return srv
}

func decode(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, v)
}

func errMissing(what string) error {
	return &missingErr{what}
}

type missingErr struct{ what string }

func (e *missingErr) Error() string { return "missing required argument(s): " + e.what }

func execOut(res ExecResult) map[string]any {
	return map[string]any{
		"stdout":      res.Stdout,
		"stderr":      res.Stderr,
		"exit_code":   res.ExitCode,
		"timed_out":   res.TimedOut,
		"duration_ms": res.Duration.Milliseconds(),
	}
}

func errResult(msg string) *sdk.CallToolResult {
	return &sdk.CallToolResult{
		IsError: true,
		Content: []sdk.Content{&sdk.TextContent{Text: msg}},
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/sandbox/ -v && go vet ./internal/sandbox/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/sandbox/
git commit -m "feat(sandbox): the 7 MCP tools over the session manager"
```

---

### Task 7: Docker backend + cmd/sandboxd

**Files:**
- Create: `internal/sandbox/docker.go`
- Create: `cmd/sandboxd/main.go`
- Modify: `go.mod` (new dep)

The Docker backend cannot be unit-tested hermetically — it is deliberately thin (every method is one or two engine calls) and gets exercised by the live proof. Build-and-vet is this task's bar, plus the existing suite staying green.

- [ ] **Step 1: Add the dependency**

```bash
go get github.com/docker/docker@v28.5.2+incompatible
```

(That version is already in go.sum's module graph; `go get` pins it in go.mod.)

- [ ] **Step 2: Implement the Docker backend**

`internal/sandbox/docker.go`:

```go
package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"path"
	"strconv"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

// sandboxLabel marks every container sandboxd creates, for reap-on-start.
const sandboxLabel = "runtime.sandbox"

// sandboxUID is the uid of the non-root `sandbox` user baked into the
// bundled image (deploy/sandbox.Dockerfile).
const sandboxUID = 1000

// DockerConfig is the container posture for real sandboxes.
type DockerConfig struct {
	Image       string  // default runtime-sandbox:latest
	WorkspaceMB int     // tmpfs /workspace size (default 64)
	MemMB       int64   // memory limit (default 512)
	CPUs        float64 // cpu limit (default 1.0)
	Runtime     string  // optional engine runtime, e.g. "runsc" (gVisor)
}

// dockerBackend implements Backend over the Docker Engine API.
type dockerBackend struct {
	cli *client.Client
	cfg DockerConfig
}

// NewDockerBackend connects to the engine (DOCKER_HOST or default socket).
// The connection is lazy — a dead daemon surfaces on first use, which the
// Manager reports per-call (degrade-don't-fail).
func NewDockerBackend(cfg DockerConfig) (Backend, error) {
	if cfg.Image == "" {
		cfg.Image = "runtime-sandbox:latest"
	}
	if cfg.WorkspaceMB <= 0 {
		cfg.WorkspaceMB = 64
	}
	if cfg.MemMB <= 0 {
		cfg.MemMB = 512
	}
	if cfg.CPUs <= 0 {
		cfg.CPUs = 1.0
	}
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	return &dockerBackend{cli: cli, cfg: cfg}, nil
}

func (d *dockerBackend) Create(ctx context.Context, tenant string) (string, error) {
	pids := int64(128)
	resp, err := d.cli.ContainerCreate(ctx,
		&container.Config{
			Image:      d.cfg.Image,
			Cmd:        []string{"sleep", "infinity"},
			User:       strconv.Itoa(sandboxUID),
			WorkingDir: workspace,
			Labels: map[string]string{
				sandboxLabel:             "1",
				sandboxLabel + ".tenant": tenant,
			},
		},
		&container.HostConfig{
			NetworkMode:    "none",
			ReadonlyRootfs: true,
			Tmpfs: map[string]string{
				workspace: fmt.Sprintf("size=%dm,mode=1777", d.cfg.WorkspaceMB),
				"/tmp":    "size=16m,mode=1777",
			},
			CapDrop:     []string{"ALL"},
			SecurityOpt: []string{"no-new-privileges"},
			Runtime:     d.cfg.Runtime,
			Resources: container.Resources{
				NanoCPUs:  int64(d.cfg.CPUs * 1e9),
				Memory:    d.cfg.MemMB << 20,
				PidsLimit: &pids,
			},
		}, nil, nil, "")
	if err != nil {
		return "", err
	}
	if err := d.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		_ = d.cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		return "", err
	}
	return resp.ID, nil
}

// Exec runs argv wrapped in coreutils `timeout` so a runaway process dies
// in-container without killing the session. Exit 124 = TERM after timeout,
// 137 = KILL after the grace period.
func (d *dockerBackend) Exec(ctx context.Context, id string, argv []string, timeout time.Duration) (ExecResult, error) {
	secs := int(timeout.Seconds())
	wrapped := append([]string{"timeout", "--kill-after=5", strconv.Itoa(secs)}, argv...)
	start := time.Now()

	// Give the API call itself headroom beyond the in-container timeout.
	ctx, cancel := context.WithTimeout(ctx, timeout+15*time.Second)
	defer cancel()

	ex, err := d.cli.ContainerExecCreate(ctx, id, container.ExecOptions{
		Cmd: wrapped, WorkingDir: workspace,
		AttachStdout: true, AttachStderr: true,
	})
	if err != nil {
		return ExecResult{}, err
	}
	att, err := d.cli.ContainerExecAttach(ctx, ex.ID, container.ExecAttachOptions{})
	if err != nil {
		return ExecResult{}, err
	}
	defer att.Close()
	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, att.Reader); err != nil && ctx.Err() == nil {
		return ExecResult{}, err
	}
	ins, err := d.cli.ContainerExecInspect(ctx, ex.ID)
	if err != nil {
		return ExecResult{}, err
	}
	elapsed := time.Since(start)
	timedOut := (ins.ExitCode == 124 || ins.ExitCode == 137) && elapsed >= timeout
	return ExecResult{
		Stdout: stdout.String(), Stderr: stderr.String(),
		ExitCode: ins.ExitCode, TimedOut: timedOut, Duration: elapsed,
	}, nil
}

// WriteFile ships content via the copy API (tar) — no shell, no quoting.
// Parent directories are created first with a plain mkdir exec (argv, not
// shell-interpolated).
func (d *dockerBackend) WriteFile(ctx context.Context, id, p string, content []byte) error {
	if dir := path.Dir(p); dir != workspace {
		if _, err := d.Exec(ctx, id, []string{"mkdir", "-p", dir}, 10*time.Second); err != nil {
			return err
		}
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hdr := &tar.Header{
		Name: p[1:], // tar paths are relative to /
		Mode: 0o644, Size: int64(len(content)),
		Uid: sandboxUID, Gid: sandboxUID,
		ModTime: time.Now(),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	if _, err := tw.Write(content); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return d.cli.CopyToContainer(ctx, id, "/", &buf, container.CopyToContainerOptions{})
}

func (d *dockerBackend) ReadFile(ctx context.Context, id, p string, limit int) ([]byte, bool, error) {
	rc, _, err := d.cli.CopyFromContainer(ctx, id, p)
	if err != nil {
		return nil, false, fmt.Errorf("no such file %s", p)
	}
	defer rc.Close()
	tr := tar.NewReader(rc)
	if _, err := tr.Next(); err != nil {
		return nil, false, err
	}
	content, err := io.ReadAll(io.LimitReader(tr, int64(limit)+1))
	if err != nil {
		return nil, false, err
	}
	if len(content) > limit {
		return content[:limit], true, nil
	}
	return content, false, nil
}

func (d *dockerBackend) Remove(ctx context.Context, id string) error {
	return d.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true})
}

func (d *dockerBackend) ListLeftovers(ctx context.Context) ([]string, error) {
	list, err := d.cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", sandboxLabel+"=1")),
	})
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(list))
	for _, c := range list {
		ids = append(ids, c.ID)
	}
	return ids, nil
}
```

**Export note:** `Backend` and `NewFakeBackend` are already exported (Task 4) precisely so this constructor and main.go can use them.

- [ ] **Step 3: Implement cmd/sandboxd**

`cmd/sandboxd/main.go`:

```go
// Command sandboxd is the Sandboxes M1 MCP server: an isolated, stateful
// code interpreter (one locked-down Docker container per sandbox session)
// exposed as MCP tools over stdio. It is designed to run as a gateway
// upstream (gateway.servers: command: with forward_tenant: true) — the
// gateway injects the calling principal's tenant as __rt_tenant.
//
// Env:
//
//	RUNTIME_SANDBOX_IMAGE           container image (default runtime-sandbox:latest)
//	RUNTIME_SANDBOX_MAX_PER_TENANT  concurrent sandboxes per tenant (default 5)
//	RUNTIME_SANDBOX_IDLE_TTL        idle close, Go duration (default 10m)
//	RUNTIME_SANDBOX_MAX_LIFETIME    hard close, Go duration (default 1h)
//	RUNTIME_SANDBOX_WORKSPACE_MB    tmpfs /workspace size (default 64)
//	RUNTIME_SANDBOX_MEM_MB          memory limit (default 512)
//	RUNTIME_SANDBOX_CPUS            cpu limit (default 1.0)
//	RUNTIME_SANDBOX_RUNTIME         engine runtime, e.g. runsc (default engine default)
//	RUNTIME_SANDBOX_FAKE            "1" ⇒ in-memory fake backend (tests only)
package main

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sausheong/runtime/internal/sandbox"
)

func main() {
	// stdout carries the MCP protocol; all logging goes to stderr.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	var be sandbox.Backend
	var err error
	if os.Getenv("RUNTIME_SANDBOX_FAKE") == "1" {
		slog.Warn("sandboxd: RUNTIME_SANDBOX_FAKE=1 — in-memory fake backend (tests only)")
		be = sandbox.NewFakeBackend()
	} else {
		be, err = sandbox.NewDockerBackend(sandbox.DockerConfig{
			Image:       os.Getenv("RUNTIME_SANDBOX_IMAGE"),
			WorkspaceMB: envInt("RUNTIME_SANDBOX_WORKSPACE_MB", 0),
			MemMB:       int64(envInt("RUNTIME_SANDBOX_MEM_MB", 0)),
			CPUs:        envFloat("RUNTIME_SANDBOX_CPUS", 0),
			Runtime:     os.Getenv("RUNTIME_SANDBOX_RUNTIME"),
		})
		if err != nil {
			slog.Error("sandboxd: docker client", "err", err)
			os.Exit(1)
		}
	}

	m := sandbox.NewManager(be, sandbox.Config{
		MaxPerTenant: envInt("RUNTIME_SANDBOX_MAX_PER_TENANT", 0),
		IdleTTL:      envDur("RUNTIME_SANDBOX_IDLE_TTL", 0),
		MaxLifetime:  envDur("RUNTIME_SANDBOX_MAX_LIFETIME", 0),
	})

	ctx := context.Background()
	if err := m.ReapStartup(ctx); err != nil {
		// Daemon down at startup is NOT fatal: serve MCP anyway;
		// create_sandbox reports the backend error per call.
		slog.Warn("sandboxd: startup reap (daemon down?)", "err", err)
	}
	m.StartReaper(ctx, time.Minute)

	srv := sandbox.NewServer(m)
	slog.Info("sandboxd: serving MCP over stdio")
	if err := srv.Run(ctx, &sdk.StdioTransport{}); err != nil {
		slog.Error("sandboxd: server exited", "err", err)
		os.Exit(1)
	}
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		slog.Warn("sandboxd: bad int env, using default", "key", key)
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
		slog.Warn("sandboxd: bad float env, using default", "key", key)
	}
	return def
}

func envDur(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		slog.Warn("sandboxd: bad duration env, using default", "key", key)
	}
	return def
}
```

- [ ] **Step 4: Build, vet, full unit suite**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all green.

- [ ] **Step 5: Commit**

```bash
git add internal/sandbox/ cmd/sandboxd/ go.mod go.sum
git commit -m "feat(sandbox): Docker backend + cmd/sandboxd stdio server"
```

---

### Task 8: Bundled image + Makefile

**Files:**
- Create: `deploy/sandbox.Dockerfile`
- Modify: `Makefile`

- [ ] **Step 1: Write the Dockerfile**

`deploy/sandbox.Dockerfile`:

```dockerfile
# The bundled sandbox image for Sandboxes M1 (cmd/sandboxd).
# Build: make sandbox-image
# Override at runtime with RUNTIME_SANDBOX_IMAGE.
FROM python:3.12-slim

# Non-root user the containers run as; uid must match sandboxUID in
# internal/sandbox/docker.go.
RUN useradd --uid 1000 --create-home sandbox

# Common analysis libs. `requests` is included deliberately: the library
# exists but the container has no network, so failures demonstrate the
# isolation rather than a missing dependency.
RUN pip install --no-cache-dir numpy pandas matplotlib requests

USER sandbox
WORKDIR /workspace
```

- [ ] **Step 2: Add Makefile targets**

Read the existing `Makefile` first and match its style. Add:

```makefile
sandbox-image: ## Build the bundled sandbox container image
	docker build -f deploy/sandbox.Dockerfile -t runtime-sandbox:latest deploy/
```

and add `cmd/sandboxd` to whatever existing target builds the binaries (find the target that builds `cmd/runtimed`/`cmd/agentd` into `bin/` and add `bin/sandboxd` alongside, same pattern).

- [ ] **Step 3: Verify**

Run: `make sandbox-image` (requires Docker running; if the daemon is unavailable, verify the Dockerfile syntax with `docker build --check -f deploy/sandbox.Dockerfile deploy/` or note it for the live proof) and the binary build target (e.g. `make build`), confirming `bin/sandboxd` appears.

- [ ] **Step 4: Commit**

```bash
git add deploy/sandbox.Dockerfile Makefile
git commit -m "feat(sandbox): bundled python image + build targets"
```

---

### Task 9: Through-serve e2e

**Files:**
- Create: `test/gateway_sandbox_e2e_test.go`

**Context:** Mirror `test/gateway_e2e_test.go` (same build tag, DB setup, binary builds, runtimed spawn, poll-for-tools pattern) and `test/identity_test.go` (identity store setup: `identity.NewStore`, `CreateTenant`, `MintServiceKey`, `InsertServiceKey`, plus the table-drop cleanup that keeps sibling tests in open mode). Requires Postgres at the standard integration DSN. sandboxd runs with `RUNTIME_SANDBOX_FAKE=1` via the gateway server's `env:` map — no Docker needed.

- [ ] **Step 1: Write the test**

`test/gateway_sandbox_e2e_test.go`:

```go
//go:build integration

package test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sausheong/runtime/internal/identity"
)

// TestGatewaySandboxE2E proves the full Sandboxes M1 chain through a real
// runtimed: identity on (two tenants), sandboxd as a forward_tenant stdio
// upstream (fake backend — no Docker), an external MCP client per tenant.
// Asserts: tools federated under sandbox__*, create/execute/files round-trip,
// and tenant isolation (B cannot see or use A's sandbox) enforced by the
// gateway-injected __rt_tenant.
func TestGatewaySandboxE2E(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Skipf("postgres not reachable: %v", err)
	}
	for _, q := range []string{
		`DROP TABLE IF EXISTS service_keys CASCADE`,
		`DROP TABLE IF EXISTS identity_users CASCADE`,
		`DROP TABLE IF EXISTS tenants CASCADE`,
		`DROP TABLE IF EXISTS session_events, sessions, agents CASCADE`,
		`DROP SCHEMA IF EXISTS dbos CASCADE`,
	} {
		mustExec(t, db, q)
	}
	t.Cleanup(func() {
		cdb, err := sql.Open("pgx", dsn)
		if err != nil {
			return
		}
		defer cdb.Close()
		for _, q := range []string{
			`DROP TABLE IF EXISTS service_keys CASCADE`,
			`DROP TABLE IF EXISTS identity_users CASCADE`,
			`DROP TABLE IF EXISTS tenants CASCADE`,
		} {
			_, _ = cdb.Exec(q)
		}
	})

	st, err := identity.NewStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTenant(ctx, "alpha", "Alpha"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTenant(ctx, "beta", "Beta"); err != nil {
		t.Fatal(err)
	}
	alphaKey, err := identity.MintServiceKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.InsertServiceKey(ctx, alphaKey.ID, "alpha", alphaKey.Hash, identity.RoleOperator, "a-op"); err != nil {
		t.Fatal(err)
	}
	betaKey, err := identity.MintServiceKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.InsertServiceKey(ctx, betaKey.ID, "beta", betaKey.Hash, identity.RoleOperator, "b-op"); err != nil {
		t.Fatal(err)
	}

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
	sandboxd := filepath.Join(tmp, "sandboxd")
	if out, err := exec.Command("go", "build", "-o", sandboxd, "../cmd/sandboxd").CombinedOutput(); err != nil {
		t.Fatalf("build sandboxd: %v\n%s", err, out)
	}

	cfgPath := filepath.Join(tmp, "runtime.yaml")
	cfg := "agents:\n" +
		"  - {id: a1, name: A, model: test/scripted, listen_addr: 127.0.0.1:8141, tenant: alpha}\n" +
		"gateway:\n" +
		"  servers:\n" +
		"    - name: sandbox\n" +
		"      command: " + sandboxd + "\n" +
		"      forward_tenant: true\n" +
		"      env: {RUNTIME_SANDBOX_FAKE: \"1\"}\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	ctlAddr := "127.0.0.1:8140"
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
	waitURLAuthed(t, base+"/healthz", alphaKey.Token, 15*time.Second)

	connect := func(token string) *sdk.ClientSession {
		cli := sdk.NewClient(&sdk.Implementation{Name: "e2e", Version: "v0"}, nil)
		tr := &sdk.StreamableClientTransport{
			Endpoint: base + "/gateway/mcp",
			HTTPClient: &http.Client{Transport: &authedRT{token: token}},
		}
		sess, err := cli.Connect(ctx, tr, nil)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		t.Cleanup(func() { _ = sess.Close() })
		return sess
	}

	alpha := connect(alphaKey.Token)
	beta := connect(betaKey.Token)

	// Sandbox tools federate (poll: upstream dial is async).
	deadline := time.Now().Add(10 * time.Second)
	for {
		lt, err := alpha.ListTools(ctx, &sdk.ListToolsParams{})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		names := map[string]bool{}
		for _, tl := range lt.Tools {
			names[tl.Name] = true
		}
		if names["sandbox__create_sandbox"] && names["sandbox__execute_code"] {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sandbox tools not federated: %v", names)
		}
		time.Sleep(200 * time.Millisecond)
	}

	callJSON := func(sess *sdk.ClientSession, name string, args map[string]any) (*sdk.CallToolResult, map[string]any) {
		res, err := sess.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out := map[string]any{}
		if len(res.Content) > 0 {
			if tc, ok := res.Content[0].(*sdk.TextContent); ok {
				_ = json.Unmarshal([]byte(tc.Text), &out)
			}
		}
		return res, out
	}

	// Alpha creates a sandbox; spoofed __rt_tenant must be overridden by the
	// gateway (sandbox lands under alpha, not "beta").
	res, out := callJSON(alpha, "sandbox__create_sandbox", map[string]any{"__rt_tenant": "beta"})
	if res.IsError {
		t.Fatalf("create: %+v", res.Content)
	}
	sbxID, _ := out["sandbox_id"].(string)
	if !strings.HasPrefix(sbxID, "sbx-") {
		t.Fatalf("bad id %q", sbxID)
	}

	// Alpha can use it.
	res, _ = callJSON(alpha, "sandbox__execute_code", map[string]any{
		"sandbox_id": sbxID, "code": "print(42)",
	})
	if res.IsError {
		t.Fatalf("alpha exec: %+v", res.Content)
	}

	// Beta cannot see it...
	_, out = callJSON(beta, "sandbox__list_sandboxes", map[string]any{})
	if sb, ok := out["sandboxes"].([]any); ok && len(sb) != 0 {
		t.Fatalf("beta sees %d sandboxes", len(sb))
	}
	// ...and calling it gets not-found (proves the spoof above didn't land
	// it under beta, AND that cross-tenant use is blocked).
	res, _ = callJSON(beta, "sandbox__execute_code", map[string]any{
		"sandbox_id": sbxID, "code": "print(1)",
	})
	if !res.IsError {
		t.Fatal("beta could use alpha's sandbox")
	}

	// Files round-trip for the owner.
	res, _ = callJSON(alpha, "sandbox__write_file", map[string]any{
		"sandbox_id": sbxID, "path": "r.txt", "content": "result",
	})
	if res.IsError {
		t.Fatalf("write: %+v", res.Content)
	}
	res, out = callJSON(alpha, "sandbox__read_file", map[string]any{
		"sandbox_id": sbxID, "path": "r.txt",
	})
	if res.IsError || out["content"] != "result" {
		t.Fatalf("read: %+v %v", res.Content, out)
	}
}

// authedRT adds the service-key bearer to every request.
type authedRT struct{ token string }

func (a *authedRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r.Header.Set("Authorization", "Bearer "+a.token)
	return http.DefaultTransport.RoundTrip(r)
}
```

Check helper availability before writing: `mustExec`, `waitURL` live in the shared test files (`grep -rn "func mustExec\|func waitURL" test/`). If there is no `waitURLAuthed`, add one next to `waitURL` (same loop, plus the bearer header); if `identity.MintServiceKey`'s returned struct names differ (`Token`/`Hash`/`ID`), check `internal/identity` and use the real names. The `StreamableClientTransport.HTTPClient` field name should be verified against the SDK (`grep -n "HTTPClient" ~/go/pkg/mod/github.com/modelcontextprotocol/go-sdk@v1.5.0/mcp/streamable.go`) — the gateway search e2e already sends authed MCP requests if identity examples are needed.

- [ ] **Step 2: Run it**

Run: `go test -tags integration ./test/ -run TestGatewaySandboxE2E -v -timeout 180s`
Expected: PASS (requires Postgres.app running).

- [ ] **Step 3: Run the whole integration suite to check for cross-test pollution**

Run: `go test -tags integration ./test/ -v -timeout 900s`
Expected: all PASS (the identity-table cleanup matters — without it, sibling tests flip into enforced mode).

- [ ] **Step 4: Commit**

```bash
git add test/gateway_sandbox_e2e_test.go
git commit -m "test(sandbox): through-serve e2e — federation, tenancy, spoof-proofing"
```

---

### Task 10: Live proof (manual, operator-driven)

Not a code task — run after Tasks 1–9 are merged-ready, with Docker Desktop up. Record results for the ROADMAP entry.

- [ ] **Step 1:** `make sandbox-image && make build`
- [ ] **Step 2:** Standalone sandboxd smoke (no runtimed): drive it with any MCP stdio client — or temporarily via a `runtime.yaml` with open mode. Exercise: `create_sandbox` → `write_file` CSV → `execute_code` pandas read+aggregate → `read_file` result. Verify files persisted across calls.
- [ ] **Step 3:** Network isolation: `execute_code` with `import requests; requests.get("https://example.com", timeout=5)` — must fail with a connection error.
- [ ] **Step 4:** Timeout: `execute_code` with `import time; time.sleep(60)` and `timeout_s: 5` — returns `timed_out: true` in ~5s; the sandbox remains usable afterward.
- [ ] **Step 5:** Reaper: restart sandboxd with `RUNTIME_SANDBOX_IDLE_TTL=30s`, create a sandbox, wait, confirm `docker ps` shows it removed and `list_sandboxes` is empty.
- [ ] **Step 6:** End-to-end agent turn: a gateway-enabled agent under runtimed (real LLM via the LiteLLM proxy) asked to "create a sandbox and compute the 20th Fibonacci number with Python" — verify the answer and the `sandbox__*` calls on the access log.
- [ ] **Step 7:** Two-tenant isolation with real keys (mirrors the e2e but live): tenant B's `list_sandboxes` empty while A has one; B calling A's id gets "no such sandbox".

---

## Self-review notes

- **Spec coverage:** §4.2 tools → Task 6; §4.3 posture → Task 7 (docker.go); §4.4 image → Task 8; §4.5 confinement → Task 3; §5 gateway change → Tasks 1–2; §6 lifecycle/degrade → Tasks 5, 7 (main.go startup-reap warn path); §7 hermetic tests → Tasks 3–6, e2e → Task 9, live proof → Task 10. Config validation rule (forward_tenant ⇒ stdio) → Task 1.
- **Known sharp edges called out inline:** SDK in-memory transport name (Task 6), `Backend` export ripple (Task 7 Step 2 note), identity helper names + `HTTPClient` field (Task 9), Makefile style-matching (Task 8).
- **Type consistency:** `Backend`/`NewFakeBackend`/`NewDockerBackend` exported in Task 7 and used by main.go; Manager method names (`ExecCode`, `ExecCommand`, `WriteFile`, `ReadFile`, `List`, `Close`, `ReapOnce`, `ReapStartup`, `StartReaper`) consistent across Tasks 5, 6.
