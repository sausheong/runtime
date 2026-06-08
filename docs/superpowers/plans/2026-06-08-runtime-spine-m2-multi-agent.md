# Runtime Spine — Milestone 2: Multi-Agent Platform — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Host many agents on the control plane — each its own supervised subprocess, routed by `/agents/{id}/...` — with a full operator CLI, real session-status tracking, and cross-agent session listing.

**Architecture:** `runtimed` loads a `runtime.yaml` agent list into a `Registry`, starts one M1-style `Supervisor` per agent, and serves a path-prefix router that reverse-proxies `/agents/{id}/*` to that agent's subprocess. The agent subprocess (`agentd` + `agentruntime.Serve`) is unchanged except for (a) session status/turn_count tracking in the durable workflow, (b) populating the `workflow_id` column, and (c) a new `GET /sessions` listing endpoint. Each agent remains an independent durable M1 agent; M2 adds breadth + operability, not new durability mechanics.

**Tech Stack:** Go 1.25.1+, `gopkg.in/yaml.v3` (already in go.sum), stdlib `net/http` (ServeMux path patterns + reverse proxy), Postgres (existing store), DBOS (unchanged). Ground truth is the `go` CLI — IGNORE IDE/LSP diagnostics (the multi-module replace setup confuses gopls; verify with `go build`/`go test`).

**Spec:** `docs/superpowers/specs/2026-06-08-runtime-spine-m2-multi-agent-design.md`.

**Branch:** `feat/runtime-spine-m2` (already created). Commit with `git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "..."`.

**Postgres** for integration tests: `postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable` (running locally).

---

## File Structure

### New
- `internal/config/config.go` — `Config`, `AgentConfig`, `Load(path)`, `Validate()`.
- `internal/config/config_test.go` — parse/validate tests.
- `internal/config/testdata/*.yaml` — valid + invalid fixtures (or inline strings in tests).
- `controlplane/registry.go` — `Registry`, `AgentInfo`, built from `[]config.AgentConfig`.
- `controlplane/registry_test.go`.
- `controlplane/router_test.go` — router dispatch tests.
- `runtime.yaml` — example/default 2-agent config at repo root.

### Modified
- `internal/store/store.go` — change `CreateSession` signature; add `ListSessions`, `IncrementTurn`. (`SetSessionStatus` already exists.)
- `internal/store/memstore.go`, `internal/store/pgstore.go` — implement the above.
- `internal/store/store_test.go` — cover new methods.
- `controlplane/api.go` — `NewAPI(reg *Registry)` path-prefix router + `/agents` + `/healthz`.
- `controlplane/proxy.go` — `AgentProcess` gains nothing structural; add a per-agent proxy cache helper if useful.
- `cmd/runtimed/main.go` — load config → registry → supervisor-per-agent → serve router.
- `agentruntime/serve.go` — status/turn_count tracking in `sessionWorkflow`; update `CreateSession` call.
- `agentruntime/server.go` — add `GET /sessions` listing handler.
- `cmd/runtimectl/main.go` — `agents`, `sessions`, `--agent` flag on `invoke`/`logs`.
- `test/resume_test.go` — unchanged (regression); add `test/multiagent_test.go`.
- `README.md` — M2 usage.

---

## Task 1: Store evolution — workflow_id, ListSessions, IncrementTurn

**Files:** `internal/store/store.go`, `memstore.go`, `pgstore.go`, `store_test.go`.

- [ ] **Step 1: Write failing tests** — append to `internal/store/store_test.go`:

```go
func TestStore_CreateSessionPopulatesWorkflowID(t *testing.T) {
	s := NewMemStore()
	ctx := context.Background()
	id, err := s.CreateSession(ctx, "agentA")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	got, _ := s.GetSession(ctx, id)
	if got.WorkflowID != id {
		t.Fatalf("workflow_id = %q, want = session id %q", got.WorkflowID, id)
	}
	if got.AgentID != "agentA" || got.Status != "created" {
		t.Fatalf("unexpected row: %+v", got)
	}
}

func TestStore_IncrementTurnAndStatus(t *testing.T) {
	s := NewMemStore()
	ctx := context.Background()
	id, _ := s.CreateSession(ctx, "a")
	if err := s.SetSessionStatus(ctx, id, "running"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := s.IncrementTurn(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := s.GetSession(ctx, id)
	if got.Status != "running" || got.TurnCount != 3 {
		t.Fatalf("got status=%q turn=%d, want running/3", got.Status, got.TurnCount)
	}
}

func TestStore_ListSessionsByAgent(t *testing.T) {
	s := NewMemStore()
	ctx := context.Background()
	a1, _ := s.CreateSession(ctx, "agentA")
	_, _ = s.CreateSession(ctx, "agentB")
	a2, _ := s.CreateSession(ctx, "agentA")

	rows, err := s.ListSessions(ctx, "agentA")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("ListSessions(agentA) = %d rows, want 2", len(rows))
	}
	ids := map[string]bool{rows[0].ID: true, rows[1].ID: true}
	if !ids[a1] || !ids[a2] {
		t.Fatalf("missing expected ids; got %+v", rows)
	}
}
```
The existing `TestStore_SessionLifecycle` calls `CreateSession(ctx, "agent1", "wf-123")` — UPDATE it to the new signature `CreateSession(ctx, "agent1")` and drop the `WorkflowID != "wf-123"` assertion (workflow_id now equals the generated id; assert `got.WorkflowID == id` instead).

- [ ] **Step 2: Run** `go test ./internal/store/ -run 'TestStore_' -v` → FAIL (signature mismatch / undefined IncrementTurn, ListSessions).

- [ ] **Step 3: Update the interface** in `internal/store/store.go`:

```go
type Store interface {
	CreateSession(ctx context.Context, agentID string) (string, error) // workflow_id := id
	GetSession(ctx context.Context, id string) (SessionRow, error)
	ListSessions(ctx context.Context, agentID string) ([]SessionRow, error)
	SetSessionStatus(ctx context.Context, id, status string) error
	IncrementTurn(ctx context.Context, id string) error
	AppendEvent(ctx context.Context, sessionID, typ string, payload []byte) (int64, error)
	EventsSince(ctx context.Context, sessionID string, afterSeq int64) ([]Event, error)
	Close() error
}
```

- [ ] **Step 4: Update `memstore.go`:**

```go
func (m *memStore) CreateSession(_ context.Context, agentID string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	id := fmt.Sprintf("ses-%d", m.seq)
	m.sessions[id] = &SessionRow{ID: id, AgentID: agentID, WorkflowID: id, Status: "created"}
	return id, nil
}

func (m *memStore) ListSessions(_ context.Context, agentID string) ([]SessionRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []SessionRow
	for _, s := range m.sessions {
		if s.AgentID == agentID {
			out = append(out, *s)
		}
	}
	return out, nil
}

func (m *memStore) IncrementTurn(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return fmt.Errorf("session %q not found", id)
	}
	s.TurnCount++
	return nil
}
```
(Note: memStore `ListSessions` order is map-nondeterministic; the test only checks membership/count, so that's fine. The pgStore orders by created_at.)

- [ ] **Step 5: Update `pgstore.go`:**

```go
func (p *pgStore) CreateSession(ctx context.Context, agentID string) (string, error) {
	id := "ses-" + uuid.NewString()
	_, err := p.db.ExecContext(ctx,
		`INSERT INTO sessions (id, agent_id, workflow_id, status) VALUES ($1,$2,$1,'created')`,
		id, agentID)
	return id, err
}

func (p *pgStore) ListSessions(ctx context.Context, agentID string) ([]SessionRow, error) {
	rows, err := p.db.QueryContext(ctx,
		`SELECT id, agent_id, workflow_id, status, turn_count FROM sessions WHERE agent_id=$1 ORDER BY created_at DESC`,
		agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionRow
	for rows.Next() {
		var s SessionRow
		if err := rows.Scan(&s.ID, &s.AgentID, &s.WorkflowID, &s.Status, &s.TurnCount); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (p *pgStore) IncrementTurn(ctx context.Context, id string) error {
	_, err := p.db.ExecContext(ctx,
		`UPDATE sessions SET turn_count = turn_count + 1, last_active_at = now() WHERE id=$1`, id)
	return err
}
```
Note the `$1,$2,$1` in the INSERT: positional `$1` is reused for both `id` and `workflow_id` (Postgres allows reusing a placeholder). Pass only `(id, agentID)`. Confirm pgx accepts reused placeholders (it does — they map by number).

- [ ] **Step 6: Run** `go test ./internal/store/ -v` → PASS (all, including updated lifecycle test).

- [ ] **Step 7: Fix the lone caller** so the package still builds — in `agentruntime/serve.go:157`, change `m.st.CreateSession(ctx, m.agentID, "")` to `m.st.CreateSession(ctx, m.agentID)`. Then `go build ./...` must pass.

- [ ] **Step 8: Verify** `go build ./... && go vet ./... && go test ./...` → all green.

- [ ] **Step 9: Commit** `internal/store/` + the serve.go one-liner.

```bash
git add internal/store/ agentruntime/serve.go
git commit -m "feat(store): workflow_id population, ListSessions, IncrementTurn"
```

---

## Task 2: Agent config package

**Files:** `internal/config/config.go`, `internal/config/config_test.go`.

- [ ] **Step 1: Write failing tests** — `internal/config/config_test.go`:

```go
package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTmp(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "runtime.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad_Valid(t *testing.T) {
	p := writeTmp(t, `
agents:
  - id: support
    name: Support
    model: test/scripted
    listen_addr: 127.0.0.1:8101
  - id: research
    name: Research
    model: test/scripted
    listen_addr: 127.0.0.1:8102
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Agents) != 2 {
		t.Fatalf("agents = %d, want 2", len(cfg.Agents))
	}
	if cfg.Agents[0].ID != "support" || cfg.Agents[0].ListenAddr != "127.0.0.1:8101" {
		t.Fatalf("bad first agent: %+v", cfg.Agents[0])
	}
}

func TestLoad_DuplicateID(t *testing.T) {
	p := writeTmp(t, `
agents:
  - {id: a, name: A, model: m, listen_addr: 127.0.0.1:8101}
  - {id: a, name: A2, model: m, listen_addr: 127.0.0.1:8102}
`)
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for duplicate id")
	}
}

func TestLoad_DuplicateAddr(t *testing.T) {
	p := writeTmp(t, `
agents:
  - {id: a, name: A, model: m, listen_addr: 127.0.0.1:8101}
  - {id: b, name: B, model: m, listen_addr: 127.0.0.1:8101}
`)
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for duplicate listen_addr")
	}
}

func TestLoad_MissingFields(t *testing.T) {
	p := writeTmp(t, `
agents:
  - {id: a, name: A, model: m}
`)
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for missing listen_addr")
	}
}

func TestLoad_NoAgents(t *testing.T) {
	p := writeTmp(t, `agents: []`)
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for empty agents list")
	}
}
```

- [ ] **Step 2: Run** `go test ./internal/config/ -v` → FAIL (package/undefined).

- [ ] **Step 3: Implement `internal/config/config.go`:**

```go
// Package config loads and validates the runtime.yaml agent list.
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// AgentConfig is one agent entry in runtime.yaml.
type AgentConfig struct {
	ID         string `yaml:"id"`
	Name       string `yaml:"name"`
	Model      string `yaml:"model"`
	ListenAddr string `yaml:"listen_addr"`
}

// Config is the parsed runtime.yaml.
type Config struct {
	Agents []AgentConfig `yaml:"agents"`
}

// Load reads and validates the config file at path.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Validate checks required fields and uniqueness.
func (c *Config) Validate() error {
	if len(c.Agents) == 0 {
		return fmt.Errorf("config: at least one agent is required")
	}
	ids := map[string]bool{}
	addrs := map[string]bool{}
	for i, a := range c.Agents {
		if a.ID == "" || a.Name == "" || a.Model == "" || a.ListenAddr == "" {
			return fmt.Errorf("config: agent[%d] requires id, name, model, listen_addr", i)
		}
		if ids[a.ID] {
			return fmt.Errorf("config: duplicate agent id %q", a.ID)
		}
		if addrs[a.ListenAddr] {
			return fmt.Errorf("config: duplicate listen_addr %q", a.ListenAddr)
		}
		ids[a.ID] = true
		addrs[a.ListenAddr] = true
	}
	return nil
}
```

- [ ] **Step 4: Run** `go test ./internal/config/ -v` → PASS. Then `go mod tidy` (yaml.v3 already present; ensures it's a direct require) and `go build ./...`.

- [ ] **Step 5: Commit** `internal/config/` (+ go.mod/go.sum if changed).

```bash
git add internal/config/ go.mod go.sum
git commit -m "feat(config): runtime.yaml agent config loader + validation"
```

---

## Task 3: Registry + path-prefix router

**Files:** `controlplane/registry.go`, `registry_test.go`, `controlplane/api.go` (rewrite), `router_test.go`.

- [ ] **Step 1: Write failing registry test** — `controlplane/registry_test.go`:

```go
package controlplane

import (
	"testing"

	"github.com/sausheong/runtime/internal/config"
)

func TestRegistry_FromConfig(t *testing.T) {
	cfg := &config.Config{Agents: []config.AgentConfig{
		{ID: "a", Name: "A", Model: "m", ListenAddr: "127.0.0.1:8101"},
		{ID: "b", Name: "B", Model: "m", ListenAddr: "127.0.0.1:8102"},
	}}
	reg := NewRegistry(cfg, "/bin/agentd", "dsn")
	if len(reg.List()) != 2 {
		t.Fatalf("List = %d, want 2", len(reg.List()))
	}
	ap, ok := reg.Get("a")
	if !ok || ap.Addr != "127.0.0.1:8101" || ap.AgentID != "a" {
		t.Fatalf("Get(a) = %+v ok=%v", ap, ok)
	}
	if _, ok := reg.Get("nope"); ok {
		t.Fatal("Get(nope) should be !ok")
	}
}
```

- [ ] **Step 2: Run** `go test ./controlplane/ -run TestRegistry` → FAIL.

- [ ] **Step 3: Implement `controlplane/registry.go`:**

```go
package controlplane

import "github.com/sausheong/runtime/internal/config"

// AgentInfo is the public description of a registered agent.
type AgentInfo struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Model string `json:"model"`
}

// Registry holds the agents the control plane hosts, built from config.
// Read-only after construction in M2 (config-driven).
type Registry struct {
	order  []string
	agents map[string]AgentProcess
	infos  map[string]AgentInfo
}

// NewRegistry builds a Registry from parsed config. binPath is the agentd
// binary all agents run; dsn is the shared Postgres DSN.
func NewRegistry(cfg *config.Config, binPath, dsn string) *Registry {
	r := &Registry{agents: map[string]AgentProcess{}, infos: map[string]AgentInfo{}}
	for _, a := range cfg.Agents {
		r.order = append(r.order, a.ID)
		r.agents[a.ID] = AgentProcess{AgentID: a.ID, Addr: a.ListenAddr, BinPath: binPath, PGDSN: dsn}
		r.infos[a.ID] = AgentInfo{ID: a.ID, Name: a.Name, Model: a.Model}
	}
	return r
}

// Get returns the AgentProcess for id.
func (r *Registry) Get(id string) (AgentProcess, bool) {
	ap, ok := r.agents[id]
	return ap, ok
}

// List returns agent infos in config order.
func (r *Registry) List() []AgentInfo {
	out := make([]AgentInfo, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.infos[id])
	}
	return out
}
```

- [ ] **Step 4: Run** `go test ./controlplane/ -run TestRegistry` → PASS.

- [ ] **Step 5: Write failing router test** — `controlplane/router_test.go`:

```go
package controlplane

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sausheong/runtime/internal/config"
)

func TestRouter_DispatchAndList(t *testing.T) {
	// Two fake "agent" backends.
	backendA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "A:"+r.URL.Path)
	}))
	defer backendA.Close()
	backendB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "B:"+r.URL.Path)
	}))
	defer backendB.Close()

	// addrOf strips http:// from the test server URL.
	addrOf := func(s string) string { return strings.TrimPrefix(s, "http://") }
	cfg := &config.Config{Agents: []config.AgentConfig{
		{ID: "a", Name: "A", Model: "m", ListenAddr: addrOf(backendA.URL)},
		{ID: "b", Name: "B", Model: "m", ListenAddr: addrOf(backendB.URL)},
	}}
	reg := NewRegistry(cfg, "/bin/agentd", "dsn")

	srv := httptest.NewServer(NewAPI(reg))
	defer srv.Close()

	// /agents/a/sessions -> backend A, path rewritten to /sessions
	resp, err := http.Get(srv.URL + "/agents/a/sessions")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "A:/sessions" {
		t.Fatalf("dispatch a = %q, want A:/sessions", body)
	}

	// /agents/b/healthz -> backend B, path /healthz
	resp, _ = http.Get(srv.URL + "/agents/b/healthz")
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "B:/healthz" {
		t.Fatalf("dispatch b = %q, want B:/healthz", body)
	}

	// unknown agent -> 404
	resp, _ = http.Get(srv.URL + "/agents/zzz/sessions")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown agent status = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()

	// GET /agents -> lists both
	resp, _ = http.Get(srv.URL + "/agents")
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"id":"a"`) || !strings.Contains(string(body), `"id":"b"`) {
		t.Fatalf("/agents list = %q", body)
	}

	// control-plane healthz
	resp, _ = http.Get(srv.URL + "/healthz")
	if resp.StatusCode != 200 {
		t.Fatalf("/healthz status = %d", resp.StatusCode)
	}
	resp.Body.Close()
}
```

- [ ] **Step 6: Run** `go test ./controlplane/ -run TestRouter` → FAIL (NewAPI signature is still M1's `NewAPI(agentAddr string)`).

- [ ] **Step 7: Rewrite `controlplane/api.go`:**

```go
package controlplane

import (
	"encoding/json"
	"net/http"
	"strings"
)

// NewAPI returns the control-plane HTTP handler routing /agents/{id}/... to
// each agent's subprocess, plus GET /agents and GET /healthz.
func NewAPI(reg *Registry) *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("GET /agents", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(reg.List())
	})

	// /agents/{id}/... → strip prefix → reverse-proxy to that agent.
	mux.HandleFunc("/agents/{id}/", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		ap, ok := reg.Get(id)
		if !ok {
			http.Error(w, "unknown agent "+id, http.StatusNotFound)
			return
		}
		// Rewrite the request path: drop the /agents/{id} prefix so the
		// backend sees its native contract path (e.g. /sessions).
		prefix := "/agents/" + id
		r.URL.Path = strings.TrimPrefix(r.URL.Path, prefix)
		if r.URL.Path == "" {
			r.URL.Path = "/"
		}
		reverseProxy(ap.Addr).ServeHTTP(w, r)
	})

	return mux
}
```
NOTE on the trailing-slash pattern: Go 1.22+ `ServeMux` treats `/agents/{id}/` as a subtree (matches `/agents/a/sessions`, `/agents/a/sessions/x/stream`, etc.). `{id}` binds the single segment. Confirm with the router test that `/agents/a/sessions` rewrites to `/sessions`. (If `r.URL.RawPath` is set it may also need trimming; for these ASCII paths `r.URL.Path` is sufficient. If a test reveals RawPath issues, clear `r.URL.RawPath = ""` after rewriting.)

- [ ] **Step 8: Run** `go test ./controlplane/ -v` → PASS (registry + router + the existing supervisor tests). `go build ./...` will FAIL until Task 4 (runtimed still calls old `NewAPI(agentAddr)`); that's expected — note it and proceed; Task 4 fixes the caller. (To keep the commit green, do Task 4 before committing, OR temporarily it's fine since `go test ./controlplane/` passes; commit controlplane + run full build after Task 4.)

- [ ] **Step 9: Commit** `controlplane/registry.go`, `registry_test.go`, `api.go`, `router_test.go`. (Build of `cmd/runtimed` is fixed in Task 4; if you want a green `go build ./...` at commit time, combine with Task 4.)

```bash
git add controlplane/
git commit -m "feat(controlplane): registry + /agents/{id} path-prefix router"
```

---

## Task 4: runtimed loads config and supervises N agents

**Files:** `cmd/runtimed/main.go` (rewrite), `runtime.yaml` (new example).

- [ ] **Step 1: Create the example `runtime.yaml`** at repo root:

```yaml
# Example agent registry for runtimed. Override path via RUNTIME_CONFIG.
agents:
  - id: support
    name: Support Agent
    model: test/scripted
    listen_addr: 127.0.0.1:8101
  - id: research
    name: Research Agent
    model: test/scripted
    listen_addr: 127.0.0.1:8102
```

- [ ] **Step 2: Rewrite `cmd/runtimed/main.go`:**

```go
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sausheong/runtime/controlplane"
	"github.com/sausheong/runtime/internal/config"
)

func main() {
	dsn := envOr("RUNTIME_PG_DSN", "postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable")
	ctlAddr := envOr("RUNTIME_CTL_ADDR", ":8080")
	agentBin := envOr("RUNTIME_AGENTD_BIN", "./agentd")
	cfgPath := envOr("RUNTIME_CONFIG", "runtime.yaml")

	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	reg := controlplane.NewRegistry(cfg, agentBin, dsn)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// One supervisor per agent.
	for _, info := range reg.List() {
		ap, _ := reg.Get(info.ID)
		sup := &controlplane.Supervisor{Spawn: ap.SpawnFunc(), Backoff: time.Second}
		go sup.Run(ctx)
		log.Printf("supervising agent %q at %s", ap.AgentID, ap.Addr)
	}

	srv := &http.Server{Addr: ctlAddr, Handler: controlplane.NewAPI(reg)}
	go func() { <-ctx.Done(); _ = srv.Shutdown(context.Background()) }()
	log.Printf("control plane on %s hosting %d agents", ctlAddr, len(reg.List()))
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
```

- [ ] **Step 3: Verify** `go build ./... && go vet ./... && go test ./...` → all green (this fixes the Task 3 caller break).

- [ ] **Step 4: Commit** `cmd/runtimed/main.go` + `runtime.yaml`.

```bash
git add cmd/runtimed/main.go runtime.yaml
git commit -m "feat(runtimed): load runtime.yaml, supervise one subprocess per agent"
```

---

## Task 5: Session status tracking + GET /sessions listing

**Files:** `agentruntime/serve.go`, `agentruntime/server.go`, `agentruntime/serve_test.go` (extend).

- [ ] **Step 1: Wire status/turn into the workflow** — in `agentruntime/serve.go` `sessionWorkflow`. The session id is `wfID`. Add status writes (best-effort, logged on error). After `startSession` already created the row as "created"; here mark running on first turn, increment per applied turn, and set terminal status. Concretely:

In `sessionWorkflow`, right after `canonical := session.NewSession(...)` and before the loop:
```go
	_ = m.st.SetSessionStatus(context.Background(), wfID, "running")
```
After `applyEntries(canonical, out.Entries)` add:
```go
		_ = m.st.IncrementTurn(context.Background(), wfID)
```
Replace the terminal `if out.Done { ... }` block so status is set:
```go
		if out.Done {
			if out.Reason == "completed" {
				_ = m.st.SetSessionStatus(context.Background(), wfID, "completed")
				m.publish(wfID, WireEvent{Type: "done"})
			} else {
				_ = m.st.SetSessionStatus(context.Background(), wfID, "error")
				m.publish(wfID, WireEvent{Type: "error", Err: "turn ended: " + out.Reason})
			}
			return out.Reason, nil
		}
```
And in the `stepErr` path and the max-turns path, set status "error" before returning:
```go
		if stepErr != nil {
			_ = m.st.SetSessionStatus(context.Background(), wfID, "error")
			m.publish(wfID, WireEvent{Type: "error", Err: stepErr.Error()})
			return "error", stepErr
		}
```
```go
		if turn >= maxTurns {
			_ = m.st.SetSessionStatus(context.Background(), wfID, "error")
			m.publish(wfID, WireEvent{Type: "error", Err: "agent exceeded maximum turns"})
			return "error", nil
		}
```
These run in the deterministic workflow body, so on replay they re-run; status is last-write-wins (idempotent) and turn_count converges because the same number of turns are applied. Add a one-line comment noting this.

- [ ] **Step 2: Add `GET /sessions` listing** to `agentruntime/server.go` `newMux()`:

```go
	mux.HandleFunc("GET /sessions", func(w http.ResponseWriter, r *http.Request) {
		rows, err := m.st.ListSessions(r.Context(), m.agentID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		type sessOut struct {
			ID        string `json:"id"`
			Status    string `json:"status"`
			TurnCount int    `json:"turn_count"`
		}
		out := make([]sessOut, 0, len(rows))
		for _, s := range rows {
			out = append(out, sessOut{ID: s.ID, Status: s.Status, TurnCount: s.TurnCount})
		}
		_ = json.NewEncoder(w).Encode(out)
	})
```
(Place it BEFORE the `GET /sessions/{id}` handler registration is irrelevant — ServeMux matches the more specific pattern correctly — but keep both. `encoding/json` is already imported in server.go.)

- [ ] **Step 3: Extend `agentruntime/serve_test.go`** with a hermetic test of the listing handler + status via the in-memory store:

```go
func TestListSessionsEndpoint(t *testing.T) {
	m := newTestManager() // existing helper: Manager with NewMemStore()
	ctx := context.Background()
	id1, _ := m.st.CreateSession(ctx, "a")
	_ = m.st.SetSessionStatus(ctx, id1, "completed")
	_ = m.st.IncrementTurn(ctx, id1)
	_, _ = m.st.CreateSession(ctx, "a")

	srv := httptest.NewServer(m.newMux())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), id1) || !strings.Contains(string(body), `"status":"completed"`) {
		t.Fatalf("/sessions body = %q", body)
	}
}
```
Confirm the existing `newTestManager` helper sets `agentID: "a"` (it does in M1's server_test.go). Add imports `io`, `strings` if not present in the test file.

- [ ] **Step 4: Verify** `go build ./... && go vet ./... && go test ./...` → green.

- [ ] **Step 5: Commit** `agentruntime/serve.go`, `server.go`, test.

```bash
git add agentruntime/
git commit -m "feat(agentruntime): session status/turn tracking + GET /sessions listing"
```

---

## Task 6: CLI — agents, sessions, --agent

**Files:** `cmd/runtimectl/main.go` (rewrite).

- [ ] **Step 1: Rewrite `cmd/runtimectl/main.go`** to add `agents`, `sessions`, and an `--agent` flag (parsed manually from args, stdlib only). The control-plane base is `RUNTIME_CTL_URL`. Routes go through `/agents/{id}/...`.

```go
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	base := envOr("RUNTIME_CTL_URL", "http://localhost:8080")
	cmd := os.Args[1]
	rest := os.Args[2:]
	agent, rest := popAgentFlag(rest)

	switch cmd {
	case "agents":
		listAgents(base)
	case "invoke":
		msg := "hello"
		if len(rest) > 0 {
			msg = rest[0]
		}
		id := resolveAgent(base, agent)
		invoke(base, id, msg)
	case "sessions":
		id := resolveAgent(base, agent)
		listSessions(base, id)
	case "logs":
		if len(rest) < 1 {
			fmt.Fprintln(os.Stderr, "usage: runtimectl logs --agent <id> <session-id>")
			os.Exit(2)
		}
		id := resolveAgent(base, agent)
		stream(base, id, rest[0])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: runtimectl <agents|invoke|sessions|logs> [--agent <id>] [args]")
	os.Exit(2)
}

// popAgentFlag extracts "--agent <id>" from args, returning the id and the rest.
func popAgentFlag(args []string) (string, []string) {
	var agent string
	var rest []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--agent" && i+1 < len(args) {
			agent = args[i+1]
			i++
			continue
		}
		rest = append(rest, args[i])
	}
	return agent, rest
}

// resolveAgent returns the explicit --agent, or the sole agent if exactly one
// is registered, else errors.
func resolveAgent(base, agent string) string {
	if agent != "" {
		return agent
	}
	infos := fetchAgents(base)
	if len(infos) == 1 {
		return infos[0].ID
	}
	fmt.Fprintf(os.Stderr, "error: --agent required (%d agents registered)\n", len(infos))
	os.Exit(2)
	return ""
}

type agentInfo struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Model string `json:"model"`
}

func fetchAgents(base string) []agentInfo {
	resp, err := http.Get(base + "/agents")
	check(err)
	defer resp.Body.Close()
	var infos []agentInfo
	_ = json.NewDecoder(resp.Body).Decode(&infos)
	return infos
}

func listAgents(base string) {
	for _, a := range fetchAgents(base) {
		fmt.Printf("%s\t%s\t%s\n", a.ID, a.Name, a.Model)
	}
}

func invoke(base, agent, msg string) {
	body, _ := json.Marshal(map[string]string{"message": msg})
	resp, err := http.Post(base+"/agents/"+agent+"/sessions", "application/json", bytes.NewReader(body))
	check(err)
	var out struct {
		SessionID string `json:"session_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if out.SessionID == "" {
		fmt.Fprintln(os.Stderr, "error: no session id returned")
		os.Exit(1)
	}
	fmt.Println("session:", out.SessionID)
	stream(base, agent, out.SessionID)
}

func listSessions(base, agent string) {
	resp, err := http.Get(base + "/agents/" + agent + "/sessions")
	check(err)
	defer resp.Body.Close()
	var rows []struct {
		ID        string `json:"id"`
		Status    string `json:"status"`
		TurnCount int    `json:"turn_count"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&rows)
	for _, s := range rows {
		fmt.Printf("%s\t%s\tturns=%d\n", s.ID, s.Status, s.TurnCount)
	}
}

func stream(base, agent, id string) {
	resp, err := http.Get(base + "/agents/" + agent + "/sessions/" + id + "/stream?since=0")
	check(err)
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if line := sc.Text(); line != "" {
			fmt.Println(line)
		}
	}
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
```

- [ ] **Step 2: Verify** `go build ./... && go vet ./...` → clean; `go build -o /tmp/rc ./cmd/runtimectl && echo ok && rm /tmp/rc`.

- [ ] **Step 3: Commit** `cmd/runtimectl/main.go`.

```bash
git add cmd/runtimectl/main.go
git commit -m "feat(cli): multi-agent runtimectl (agents/sessions/--agent)"
```

---

## Task 7: Two-agent integration test + M1 regression

**Files:** `test/multiagent_test.go` (new).

- [ ] **Step 1: Write `test/multiagent_test.go`** (build-tagged `//go:build integration`). It writes a 2-agent config to a temp dir, builds agentd, starts runtimed with that config, invokes a session on EACH agent through the router, asserts each completes ("final answer") and that `GET /agents/{id}/sessions` lists that agent's session (and the two agents' sessions are disjoint).

```go
//go:build integration

package test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestMultiAgentRouting(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("pgx", dsn) // dsn const from resume_test.go (same package)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Clean slate.
	mustExec(t, db, `DROP TABLE IF EXISTS markers`)
	mustExec(t, db, `CREATE TABLE markers (id BIGSERIAL PRIMARY KEY, ran_at TIMESTAMPTZ)`)
	mustExec(t, db, `DROP TABLE IF EXISTS session_events, sessions, agents CASCADE`)
	mustExec(t, db, `DROP SCHEMA IF EXISTS dbos CASCADE`)

	// Build binaries.
	bin := t.TempDir()
	agentd := filepath.Join(bin, "agentd")
	if out, err := exec.Command("go", "build", "-o", agentd, "../cmd/agentd").CombinedOutput(); err != nil {
		t.Fatalf("build agentd: %v\n%s", err, out)
	}
	runtimed := filepath.Join(bin, "runtimed")
	if out, err := exec.Command("go", "build", "-o", runtimed, "../cmd/runtimed").CombinedOutput(); err != nil {
		t.Fatalf("build runtimed: %v\n%s", err, out)
	}

	// 2-agent config.
	cfgPath := filepath.Join(bin, "runtime.yaml")
	cfg := `agents:
  - {id: alpha, name: Alpha, model: test/scripted, listen_addr: 127.0.0.1:8111}
  - {id: beta, name: Beta, model: test/scripted, listen_addr: 127.0.0.1:8112}
`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	// Start runtimed.
	ctlAddr := "127.0.0.1:8120"
	cmd := exec.Command(runtimed)
	cmd.Env = append(os.Environ(),
		"RUNTIME_PG_DSN="+dsn,
		"RUNTIME_CTL_ADDR="+ctlAddr,
		"RUNTIME_AGENTD_BIN="+agentd,
		"RUNTIME_CONFIG="+cfgPath,
	)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill() }()

	base := "http://" + ctlAddr
	// Wait for control plane + both agents to be healthy through the router.
	waitURL(t, base+"/healthz", 10*time.Second)
	waitURL(t, base+"/agents/alpha/healthz", 15*time.Second)
	waitURL(t, base+"/agents/beta/healthz", 15*time.Second)

	// Invoke a session on each agent and assert it completes.
	sa := invokeAgent(t, base, "alpha")
	sb := invokeAgent(t, base, "beta")

	// Each agent lists exactly its own session.
	alphaSessions := listAgentSessions(t, base, "alpha")
	betaSessions := listAgentSessions(t, base, "beta")
	if !contains(alphaSessions, sa) {
		t.Fatalf("alpha sessions %v missing %s", alphaSessions, sa)
	}
	if !contains(betaSessions, sb) {
		t.Fatalf("beta sessions %v missing %s", betaSessions, sb)
	}
	if contains(alphaSessions, sb) || contains(betaSessions, sa) {
		t.Fatalf("cross-agent leak: alpha=%v beta=%v sa=%s sb=%s", alphaSessions, betaSessions, sa, sb)
	}
}

// invokeAgent posts a session, streams to done, asserts "final answer", returns session id.
func invokeAgent(t *testing.T, base, agent string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"message": "go"})
	resp, err := http.Post(base+"/agents/"+agent+"/sessions", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("invoke %s: %v", agent, err)
	}
	var out struct {
		SessionID string `json:"session_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if out.SessionID == "" {
		t.Fatalf("invoke %s: no session id", agent)
	}
	final := streamUntilDone(t, base+"/agents/"+agent+"/sessions/"+out.SessionID+"/stream?since=0", 30*time.Second)
	if !strings.Contains(final, "final answer") {
		t.Fatalf("agent %s session %s did not complete; stream=%s", agent, out.SessionID, final)
	}
	return out.SessionID
}

func listAgentSessions(t *testing.T, base, agent string) []string {
	t.Helper()
	resp, err := http.Get(base + "/agents/" + agent + "/sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var rows []struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&rows)
	var ids []string
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return ids
}

func contains(xs []string, x string) bool {
	for _, e := range xs {
		if e == x {
			return true
		}
	}
	return false
}

func waitURL(t *testing.T, url string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil && resp.StatusCode == 200 {
			resp.Body.Close()
			return
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("url never healthy: %s", url)
}

var _ = fmt.Sprintf // keep fmt import if unused after edits
```
This reuses `mustExec`, `dsn`, and `streamUntilDone` from `resume_test.go` (same `test` package, both behind the `integration` tag). If signatures differ, adapt the calls. Do NOT duplicate those helpers.

- [ ] **Step 2: Run the new test** (Postgres up):

```bash
go test -tags integration ./test/ -run TestMultiAgentRouting -v -count=1 -timeout 180s
```
Expected: PASS — both agents reachable through the router, both sessions complete, session lists are correct and disjoint.

- [ ] **Step 3: Regression — the M1 resume test must still pass:**

```bash
go test -tags integration ./test/ -run TestResumeAfterKill -v -count=1 -timeout 120s
```
Expected: PASS (durability not regressed). NOTE: `resume_test.go` uses a single-agent flow but agentd is unchanged; if `resume_test.go` started agentd directly (not via runtimed), it still works as-is. Confirm it does and leave it unchanged.

- [ ] **Step 4: Commit** `test/multiagent_test.go`.

```bash
git add test/multiagent_test.go
git commit -m "test: two-agent routing + session-listing integration test"
```

---

## Task 8: README + full verification

**Files:** `README.md`.

- [ ] **Step 1: Update `README.md`** — add a "Multiple agents (M2)" section: the `runtime.yaml` format, that `runtimed` now hosts N agents, the `/agents/{id}/...` routing, and the new CLI (`runtimectl agents`, `invoke --agent`, `sessions --agent`, `logs --agent`). Move "single static agent" OUT of the limitations list (now done); keep pools/console/auth as limitations. Update the architecture diagram to show multiple agentd boxes behind the router.

- [ ] **Step 2: Full hermetic verification:**

```bash
go build ./... && go vet ./... && go test ./...
```
Expected: all green.

- [ ] **Step 3: Full integration verification** (Postgres up):

```bash
go test -tags integration ./test/ -v -count=1 -timeout 180s
```
Expected: both `TestResumeAfterKill` and `TestMultiAgentRouting` PASS.

- [ ] **Step 4: Commit** `README.md`.

```bash
git add README.md
git commit -m "docs: M2 multi-agent usage and runbook"
```

---

## Definition of Done (Milestone 2)

- [ ] `runtimed` loads `runtime.yaml` and hosts N agents, each a supervised subprocess.
- [ ] `/agents/{id}/...` routes to the right agent; unknown id → 404; `GET /agents` lists them.
- [ ] Sessions report real status (created→running→completed/error) + turn_count; `workflow_id` column populated.
- [ ] `GET /agents/{id}/sessions` lists an agent's sessions; CLI exposes `agents`, `invoke --agent`, `sessions --agent`, `logs --agent`.
- [ ] Two-agent integration test passes; **M1 resume test still passes** (no durability regression).
- [ ] All hermetic tests + vet green.
