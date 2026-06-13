# Spine A1 — Replica Pools + Session Affinity Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let each local agent run `replicas: N` supervised `agentd` processes behind one `/agents/{id}` route, round-robining new sessions and pinning session-scoped requests to the owning replica.

**Architecture:** Each agent expands to an ordered replica set in the registry; each replica gets a stable derived DBOS executor id (`DBOS__VMID="<id>#<i>"`) and a derived listen port (`base_port+i`). The owner replica is persisted on the session row (`replica` column); runtimed reads it to pin session-scoped requests, and round-robins new sessions across the set. Per-replica supervision means a restart at the same index recovers exactly that replica's in-flight workflows (M1 durability, scoped per replica).

**Tech Stack:** Go 1.25, DBOS (`dbos-transact-golang` v0.16.0), Postgres, Prometheus client, `net/http/httputil` reverse proxy.

**Reference spec:** `docs/superpowers/specs/2026-06-13-spine-a1-replica-pools-design.md`

**Conventions:** The `go` CLI is ground truth (ignore IDE/LSP diagnostics from the `replace ../harness` setup). Unit tests are hermetic (`go test ./...`). Integration tests use `//go:build integration` and Postgres.app at `postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable`; they self-clean. Scripted model (`test/scripted`) needs no LLM key.

---

## File Structure

- `internal/config/config.go` — add `Replicas` field, `ReplicaAddrs()` helper, validation (Task 1).
- `internal/store/{schema.sql,store.go,pgstore.go,memstore.go}` — `replica` column, `CreateSession` signature, `SessionReplica` (Task 2).
- `agentruntime/{serve.go,server.go}` — read `RUNTIME_AGENT_REPLICA`, stamp owner on create (Task 3).
- `controlplane/{proxy.go,registry.go}` — `AgentProcess.{ReplicaIndex,DBOSVMID}`, replica-set registry, `Replicas/Replica/NextReplica` (Task 4).
- `controlplane/proxy.go` `buildEnv` — inject `DBOS__VMID` + `RUNTIME_AGENT_REPLICA` (Task 5).
- `controlplane/api.go` — `NewAPI` takes a `store.Store`; request-shape classification + affinity routing (Task 6).
- `cmd/runtimed/main.go` — per-replica supervision (Task 7).
- `internal/obs/{obs.go,fanout.go}` + `cmd/runtimed/main.go` — per-replica metrics fan-out + `replica` label, per-agent health OR (Task 8).
- `test/replica_pools_test.go` — integration test (Task 9).
- `README.md`, `ROADMAP.md` — docs (Task 10).

---

## Task 1: Config — `replicas` field + derived addresses + validation

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing tests**

Add to `internal/config/config_test.go`:

```go
func TestReplicaAddrs_Default(t *testing.T) {
	a := AgentConfig{ID: "x", ListenAddr: "127.0.0.1:8101"}
	got, err := a.ReplicaAddrs()
	if err != nil {
		t.Fatalf("ReplicaAddrs: %v", err)
	}
	if len(got) != 1 || got[0] != "127.0.0.1:8101" {
		t.Fatalf("default replicas: got %v, want [127.0.0.1:8101]", got)
	}
}

func TestReplicaAddrs_Range(t *testing.T) {
	a := AgentConfig{ID: "x", ListenAddr: "127.0.0.1:8101", Replicas: 3}
	got, err := a.ReplicaAddrs()
	if err != nil {
		t.Fatalf("ReplicaAddrs: %v", err)
	}
	want := []string{"127.0.0.1:8101", "127.0.0.1:8102", "127.0.0.1:8103"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("range: got %v, want %v", got, want)
	}
}

func TestValidate_ReplicasRejectedOnRemote(t *testing.T) {
	c := &Config{Agents: []AgentConfig{
		{ID: "r", Name: "R", Model: "m", URL: "http://h:9000", Replicas: 2},
	}}
	if err := c.Validate(); err == nil {
		t.Fatal("expected error: replicas on remote agent")
	}
}

func TestValidate_DerivedPortCollision(t *testing.T) {
	c := &Config{Agents: []AgentConfig{
		{ID: "a", Name: "A", Model: "m", ListenAddr: "127.0.0.1:8101", Replicas: 3},
		{ID: "b", Name: "B", Model: "m", ListenAddr: "127.0.0.1:8102"}, // collides with a#1
	}}
	if err := c.Validate(); err == nil {
		t.Fatal("expected error: derived port collision 127.0.0.1:8102")
	}
}

func TestValidate_BadBasePort(t *testing.T) {
	c := &Config{Agents: []AgentConfig{
		{ID: "a", Name: "A", Model: "m", ListenAddr: "127.0.0.1:notaport", Replicas: 2},
	}}
	if err := c.Validate(); err == nil {
		t.Fatal("expected error: unparseable base port")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/config/ -run 'Replica|DerivedPort|BadBasePort' -v`
Expected: FAIL — `a.ReplicaAddrs undefined`, and the validation tests pass-through (no error) since `Replicas` doesn't exist yet.

- [ ] **Step 3: Add the `Replicas` field**

In `internal/config/config.go`, in `AgentConfig`, after the `Memory` field (line ~24):

```go
	Memory     bool     `yaml:"memory"`  // optional; opt-in to the per-tenant Postgres memory tool. Default false.
	Replicas   int      `yaml:"replicas"` // optional; 0/omitted ⇒ 1. Local agents only: replica i listens on base_port+i.
```

- [ ] **Step 4: Add the `ReplicaAddrs` helper**

Add to `internal/config/config.go` (after `AgentTenants`, end of file). Add `"net"` and `"strconv"` to the import block:

```go
// ReplicaAddrs returns the derived listen addresses for a local agent: replica i
// listens on base_host:base_port+i. Replicas <= 0 means 1. Errors if the base
// listen_addr has no parseable numeric port. Not meaningful for remote agents
// (Validate rejects replicas there); returns the single URL-less base otherwise.
func (a AgentConfig) ReplicaAddrs() ([]string, error) {
	n := a.Replicas
	if n <= 0 {
		n = 1
	}
	host, portStr, err := net.SplitHostPort(a.ListenAddr)
	if err != nil {
		return nil, fmt.Errorf("agent %q listen_addr %q: %w", a.ID, a.ListenAddr, err)
	}
	base, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, fmt.Errorf("agent %q listen_addr %q: port not numeric: %w", a.ID, a.ListenAddr, err)
	}
	out := make([]string, n)
	for i := 0; i < n; i++ {
		out[i] = net.JoinHostPort(host, strconv.Itoa(base+i))
	}
	return out, nil
}
```

- [ ] **Step 5: Wire validation**

In `internal/config/config.go` `Validate()`, the remote-agent branch currently rejects spawn-time-only fields. Update that check (line ~180) to also reject `Replicas > 1`:

```go
			// Local-only fields can't be delivered to a process we don't spawn.
			if len(a.Command) > 0 || a.WorkDir != "" || a.Kind != "" || a.Memory || a.Gateway.Enabled() || a.Replicas > 1 {
				return fmt.Errorf("config: remote agent %q must not set command, workdir, kind, memory, gateway, or replicas (these are spawn-time only)", a.ID)
			}
```

Then replace the local-agent dial-uniqueness block. Currently (lines ~195–203):

```go
		dial := a.ListenAddr
		if remote {
			dial = a.URL
		}
		if dials[dial] {
			return fmt.Errorf("config: duplicate agent dial address %q", dial)
		}
		ids[a.ID] = true
		dials[dial] = true
```

Replace with (expands every derived port into the uniqueness set):

```go
		ids[a.ID] = true
		if remote {
			if dials[a.URL] {
				return fmt.Errorf("config: duplicate agent dial address %q", a.URL)
			}
			dials[a.URL] = true
		} else {
			addrs, err := a.ReplicaAddrs()
			if err != nil {
				return fmt.Errorf("config: %w", err)
			}
			for _, addr := range addrs {
				if dials[addr] {
					return fmt.Errorf("config: agent %q derived address %q collides with another agent", a.ID, addr)
				}
				dials[addr] = true
			}
		}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/config/ -v`
Expected: PASS (all, including the pre-existing config tests).

- [ ] **Step 7: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): replicas: N + derived port range with collision check"
```

---

## Task 2: Store — `replica` column, `CreateSession` signature, `SessionReplica`

**Files:**
- Modify: `internal/store/schema.sql`, `internal/store/store.go`, `internal/store/pgstore.go`, `internal/store/memstore.go`
- Test: `internal/store/store_test.go`

- [ ] **Step 1: Write the failing tests**

Add to `internal/store/store_test.go` (these run for the memstore, which the existing tests use; pgstore parity is covered by the integration test):

```go
func TestStore_CreateSessionPersistsReplica(t *testing.T) {
	s := NewMemStore()
	ctx := context.Background()
	id, err := s.CreateSession(ctx, "agentA", 2)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	r, err := s.SessionReplica(ctx, id)
	if err != nil {
		t.Fatalf("SessionReplica: %v", err)
	}
	if r != 2 {
		t.Fatalf("replica: got %d, want 2", r)
	}
	row, _ := s.GetSession(ctx, id)
	if row.Replica != 2 {
		t.Fatalf("GetSession replica: got %d, want 2", row.Replica)
	}
}

func TestStore_SessionReplicaNotFound(t *testing.T) {
	s := NewMemStore()
	if _, err := s.SessionReplica(context.Background(), "nope"); err == nil {
		t.Fatal("expected error for unknown session")
	}
}
```

Also update every existing `CreateSession(ctx, "x")` call in `internal/store/store_test.go` to pass a replica arg, e.g. `CreateSession(ctx, "agent1", 0)`.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/store/ -run 'Replica' -v`
Expected: FAIL — `too many arguments in call to s.CreateSession` and `s.SessionReplica undefined`, `row.Replica undefined`.

- [ ] **Step 3: Update the schema**

In `internal/store/schema.sql`, add the column to the `sessions` table (after `turn_count`):

```sql
CREATE TABLE IF NOT EXISTS sessions (
    id          TEXT PRIMARY KEY,
    agent_id    TEXT NOT NULL,
    workflow_id TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'created',
    turn_count  INT  NOT NULL DEFAULT 0,
    replica     INT  NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_active_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS replica INT NOT NULL DEFAULT 0;
```

(The `ALTER` covers databases created before A1; on a fresh DB it is a harmless no-op.)

- [ ] **Step 4: Update the `Store` interface + `SessionRow`**

In `internal/store/store.go`:

```go
type SessionRow struct {
	ID         string
	AgentID    string
	WorkflowID string
	Status     string // created | running | idle | recovering | closed | failed
	TurnCount  int
	Replica    int
}
```

And the interface:

```go
type Store interface {
	CreateSession(ctx context.Context, agentID string, replica int) (string, error)
	GetSession(ctx context.Context, id string) (SessionRow, error)
	ListSessions(ctx context.Context, agentID string) ([]SessionRow, error)
	SessionReplica(ctx context.Context, id string) (int, error)
	SetSessionStatus(ctx context.Context, id, status string) error
	SetTurnCount(ctx context.Context, id string, n int) error
	AppendEvent(ctx context.Context, sessionID, typ string, payload []byte) (int64, error)
	EventsSince(ctx context.Context, sessionID string, afterSeq int64) ([]Event, error)
	Close() error
}
```

- [ ] **Step 5: Update `pgstore.go`**

In `internal/store/pgstore.go`, change `CreateSession`, add `SessionReplica`, and add `replica` to the two SELECTs:

```go
func (p *pgStore) CreateSession(ctx context.Context, agentID string, replica int) (string, error) {
	id := "ses-" + uuid.NewString()
	_, err := p.db.ExecContext(ctx,
		`INSERT INTO sessions (id, agent_id, workflow_id, status, replica) VALUES ($1,$2,$1,'created',$3)`,
		id, agentID, replica)
	return id, err
}

func (p *pgStore) SessionReplica(ctx context.Context, id string) (int, error) {
	var r int
	err := p.db.QueryRowContext(ctx, `SELECT replica FROM sessions WHERE id=$1`, id).Scan(&r)
	if err == sql.ErrNoRows {
		return 0, fmt.Errorf("session %q not found", id)
	}
	return r, err
}
```

In `ListSessions`, change the query and scan:

```go
	rows, err := p.db.QueryContext(ctx,
		`SELECT id, agent_id, workflow_id, status, turn_count, replica FROM sessions WHERE agent_id=$1 ORDER BY created_at DESC`,
		agentID)
	...
		if err := rows.Scan(&s.ID, &s.AgentID, &s.WorkflowID, &s.Status, &s.TurnCount, &s.Replica); err != nil {
```

In `GetSession`, change the query and scan:

```go
	err := p.db.QueryRowContext(ctx,
		`SELECT id, agent_id, workflow_id, status, turn_count, replica FROM sessions WHERE id=$1`, id).
		Scan(&s.ID, &s.AgentID, &s.WorkflowID, &s.Status, &s.TurnCount, &s.Replica)
```

- [ ] **Step 6: Update `memstore.go`**

In `internal/store/memstore.go`:

```go
func (m *memStore) CreateSession(_ context.Context, agentID string, replica int) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	id := fmt.Sprintf("ses-%d", m.seq)
	m.sessions[id] = &SessionRow{ID: id, AgentID: agentID, WorkflowID: id, Status: "created", Replica: replica}
	return id, nil
}

func (m *memStore) SessionReplica(_ context.Context, id string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return 0, fmt.Errorf("session %q not found", id)
	}
	return s.Replica, nil
}
```

(`GetSession`/`ListSessions` already copy the whole `SessionRow`, so `Replica` rides along once the field exists.)

- [ ] **Step 7: Run the tests to verify they pass**

Run: `go test ./internal/store/ -v`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/store/
git commit -m "feat(store): persist session owner replica + SessionReplica lookup"
```

---

## Task 3: agentd — read replica index, stamp owner on session create

**Files:**
- Modify: `agentruntime/serve.go`, `agentruntime/server.go`
- Test: `agentruntime/server_test.go`

- [ ] **Step 1: Write the failing test**

In `agentruntime/server_test.go`, the existing tests build a `Manager` directly. Add a test that a Manager created with a replica stamps it on session create. First check how `startSession` is reached — it is called from the `POST /sessions` handler. The simplest unit assertion is on the Manager field plumb-through. Add:

```go
func TestManager_StampsReplicaOnCreate(t *testing.T) {
	st := store.NewMemStore()
	m := &Manager{agentID: "a", st: st, replica: 3, subscribers: map[string][]chan WireEvent{}}
	id, err := st.CreateSession(context.Background(), "a", m.replica)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	r, _ := st.SessionReplica(context.Background(), id)
	if r != 3 {
		t.Fatalf("replica: got %d, want 3", r)
	}
}
```

(This guards the field exists and the value flows; the real create path is exercised by the integration test.)

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./agentruntime/ -run StampsReplica -v`
Expected: FAIL — `unknown field replica in struct literal`.

- [ ] **Step 3: Add the `replica` field to `Manager`**

In `agentruntime/serve.go`, in the `Manager` struct, after `authToken` (line ~33):

```go
	authToken string
	// replica is this process's 0-based replica index (from
	// RUNTIME_AGENT_REPLICA). Stamped onto each session row at create so the
	// control plane can pin session-scoped requests back to this replica.
	replica int
```

- [ ] **Step 4: Pass `replica` to `CreateSession`**

In `agentruntime/serve.go` `startSession` (line ~265), change:

```go
	sessionID, err := m.st.CreateSession(ctx, m.agentID, m.replica)
```

- [ ] **Step 5: Read the env var in `Serve`**

In `agentruntime/serve.go` `Serve`, where the `Manager` is constructed (line ~320), add the `replica` field. Add `"strconv"` to imports if not present (it is not currently — add it). Just before building `m`:

```go
	replica, _ := strconv.Atoi(os.Getenv("RUNTIME_AGENT_REPLICA")) // "" or bad ⇒ 0
```

Then in the struct literal:

```go
	m := &Manager{
		agentID:     cfg.Spec.ID,
		cfg:         cfg,
		dbosCtx:     dctx,
		st:          st,
		metrics:     obs.NewAgentMetrics(cfg.Spec.ID),
		authToken:   os.Getenv("RUNTIME_AGENT_AUTH_TOKEN"),
		replica:     replica,
		subscribers: map[string][]chan WireEvent{},
	}
```

- [ ] **Step 6: Verify other `CreateSession` callers compile**

`agentruntime/server_test.go` and `agentruntime/serve_test.go` call `st.CreateSession(ctx, "a")`. Update each to `st.CreateSession(ctx, "a", 0)`.

- [ ] **Step 7: Run the tests to verify they pass**

Run: `go test ./agentruntime/ -v`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add agentruntime/
git commit -m "feat(agentd): read RUNTIME_AGENT_REPLICA and stamp session owner"
```

---

## Task 4: Registry — replica sets + Replicas/Replica/NextReplica + AgentProcess fields

**Files:**
- Modify: `controlplane/proxy.go` (AgentProcess fields), `controlplane/registry.go`
- Test: `controlplane/registry_test.go`

- [ ] **Step 1: Write the failing tests**

Add to `controlplane/registry_test.go`:

```go
func TestRegistry_ReplicaSetExpansion(t *testing.T) {
	cfg := &config.Config{Agents: []config.AgentConfig{
		{ID: "support", Name: "S", Model: "m", ListenAddr: "127.0.0.1:8101", Replicas: 3, Tenant: "default"},
	}}
	r := NewRegistry(cfg, "/bin/agentd", "dsn")
	set, ok := r.Replicas("support")
	if !ok || len(set) != 3 {
		t.Fatalf("Replicas: ok=%v len=%d, want 3", ok, len(set))
	}
	for i, ap := range set {
		if ap.ReplicaIndex != i {
			t.Errorf("replica %d: ReplicaIndex=%d", i, ap.ReplicaIndex)
		}
		wantVMID := "support#" + strconv.Itoa(i)
		if ap.DBOSVMID != wantVMID {
			t.Errorf("replica %d: DBOSVMID=%q want %q", i, ap.DBOSVMID, wantVMID)
		}
		wantAddr := "127.0.0.1:" + strconv.Itoa(8101+i)
		if ap.Addr != wantAddr {
			t.Errorf("replica %d: Addr=%q want %q", i, ap.Addr, wantAddr)
		}
	}
}

func TestRegistry_NextReplicaRoundRobin(t *testing.T) {
	cfg := &config.Config{Agents: []config.AgentConfig{
		{ID: "a", Name: "A", Model: "m", ListenAddr: "127.0.0.1:8201", Replicas: 2, Tenant: "default"},
	}}
	r := NewRegistry(cfg, "/bin/agentd", "dsn")
	got := []int{r.NextReplica("a"), r.NextReplica("a"), r.NextReplica("a"), r.NextReplica("a")}
	want := []int{0, 1, 0, 1}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("NextReplica seq: got %v want %v", got, want)
		}
	}
}

func TestRegistry_RemoteSingleReplica(t *testing.T) {
	cfg := &config.Config{Agents: []config.AgentConfig{
		{ID: "rem", Name: "R", Model: "m", URL: "https://h:8443", Tenant: "default"},
	}}
	r := NewRegistry(cfg, "/bin/agentd", "dsn")
	set, ok := r.Replicas("rem")
	if !ok || len(set) != 1 {
		t.Fatalf("remote Replicas: ok=%v len=%d, want 1", ok, len(set))
	}
	if !set[0].Remote || set[0].DBOSVMID != "" || set[0].BaseURL != "https://h:8443" {
		t.Fatalf("remote replica fields wrong: %+v", set[0])
	}
}

func TestRegistry_ReplicaByIndex(t *testing.T) {
	cfg := &config.Config{Agents: []config.AgentConfig{
		{ID: "a", Name: "A", Model: "m", ListenAddr: "127.0.0.1:8301", Replicas: 2, Tenant: "default"},
	}}
	r := NewRegistry(cfg, "/bin/agentd", "dsn")
	ap, ok := r.Replica("a", 1)
	if !ok || ap.ReplicaIndex != 1 {
		t.Fatalf("Replica(a,1): ok=%v idx=%d", ok, ap.ReplicaIndex)
	}
	if _, ok := r.Replica("a", 2); ok {
		t.Fatal("Replica(a,2) should be out of range")
	}
	if _, ok := r.Replica("nope", 0); ok {
		t.Fatal("Replica(nope,0) should be unknown")
	}
}
```

Ensure the test file imports `strconv` and `github.com/sausheong/runtime/internal/config`.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./controlplane/ -run 'Registry_Replica|Registry_NextReplica|Registry_Remote' -v`
Expected: FAIL — `r.Replicas undefined`, `ap.ReplicaIndex undefined`, etc.

- [ ] **Step 3: Add fields to `AgentProcess`**

In `controlplane/proxy.go`, in `AgentProcess`, after the `AuthToken` block (line ~40):

```go
	// ReplicaIndex is this replica's 0-based index within its agent's pool.
	// 0 for single-replica and remote agents. Injected into the child as
	// RUNTIME_AGENT_REPLICA and used to derive the listen port and executor id.
	ReplicaIndex int
	// DBOSVMID is the stable per-replica DBOS executor id "<AgentID>#<index>"
	// (injected as DBOS__VMID). "" for remote agents (the remote owns its own
	// executor id). A restart at the same index reuses this id, so the replica
	// recovers exactly its own in-flight workflows.
	DBOSVMID string
```

- [ ] **Step 4: Rewrite the registry to hold replica sets**

Replace `controlplane/registry.go` entirely with:

```go
package controlplane

import (
	"strconv"
	"sync/atomic"

	"github.com/sausheong/runtime/internal/config"
)

// AgentInfo is the public description of a registered agent.
type AgentInfo struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Model  string `json:"model"`
	Tenant string `json:"tenant"`
}

// Registry holds the agents the control plane hosts, built from config. Each
// agent maps to an ordered replica set (len 1 for single-replica and remote
// agents). Read-only after construction except the optional secret broker and
// the gateway stamp (SetGateway); both must complete before serving starts.
type Registry struct {
	order  []string
	sets   map[string][]AgentProcess // id -> ordered replica set
	infos  map[string]AgentInfo
	rr     map[string]*atomic.Uint64 // id -> round-robin counter (new-session routing)
	broker SecretBroker              // optional; injected into each AgentProcess on read.
}

// NewRegistry builds a Registry from parsed config. binPath is the agentd
// binary all local agents run; dsn is the shared Postgres DSN. A local agent
// with replicas: N expands to N AgentProcess entries on derived ports; a remote
// agent (url:) is a single attach-only entry.
func NewRegistry(cfg *config.Config, binPath, dsn string) *Registry {
	r := &Registry{
		sets:  map[string][]AgentProcess{},
		infos: map[string]AgentInfo{},
		rr:    map[string]*atomic.Uint64{},
	}
	for _, a := range cfg.Agents {
		r.order = append(r.order, a.ID)
		r.infos[a.ID] = AgentInfo{ID: a.ID, Name: a.Name, Model: a.Model, Tenant: a.Tenant}
		r.rr[a.ID] = &atomic.Uint64{}

		base := AgentProcess{
			AgentID: a.ID, BinPath: binPath, PGDSN: dsn,
			Kind: a.Kind, Command: a.Command, WorkDir: a.WorkDir, Tenant: a.Tenant,
			Memory: a.Memory, GatewayOn: a.Gateway.Enabled(),
			GatewaySearch: a.Gateway == config.GatewaySearch,
		}
		if a.URL != "" {
			rem := base
			rem.Remote = true
			rem.BaseURL = a.URL
			rem.AuthToken = a.AuthToken
			rem.ReplicaIndex = 0
			r.sets[a.ID] = []AgentProcess{rem}
			continue
		}
		// Local: expand to the derived replica addresses. Validate() has already
		// proven these parse and don't collide, so the error is unreachable; we
		// fall back to the single base addr defensively if it ever fires.
		addrs, err := a.ReplicaAddrs()
		if err != nil {
			addrs = []string{a.ListenAddr}
		}
		set := make([]AgentProcess, len(addrs))
		for i, addr := range addrs {
			ap := base
			ap.ReplicaIndex = i
			ap.Addr = addr
			ap.BaseURL = "http://" + addr
			ap.DBOSVMID = a.ID + "#" + strconv.Itoa(i)
			set[i] = ap
		}
		r.sets[a.ID] = set
	}
	return r
}

// SetBroker installs the secret broker injected into every AgentProcess returned
// by Get/Replicas/Replica. NOT safe to call concurrently with reads: it must
// happen-before the HTTP server and supervisor goroutines start. nil ⇒ no
// brokering.
func (r *Registry) SetBroker(b SecretBroker) { r.broker = b }

// SetGateway records the gateway endpoint URL and per-tenant agent keys, stamped
// onto every gateway-enabled replica. Like SetBroker, must complete before the
// server and supervisor goroutines start.
func (r *Registry) SetGateway(url string, keys map[string]string) {
	for id, set := range r.sets {
		for i := range set {
			if !set[i].GatewayOn {
				continue
			}
			set[i].GatewayURL = url
			set[i].GatewayKey = keys[set[i].Tenant]
		}
		r.sets[id] = set
	}
}

// AgentTenants returns agentID→tenantID for all registered agents.
func (r *Registry) AgentTenants() map[string]string {
	m := make(map[string]string, len(r.order))
	for _, id := range r.order {
		m[id] = r.infos[id].Tenant
	}
	return m
}

// withBroker returns a copy of ap with the registry's broker attached, so the
// broker rides along on what callers get without mutating the stored entry.
func (r *Registry) withBroker(ap AgentProcess) AgentProcess {
	ap.broker = r.broker
	return ap
}

// Get returns replica 0 of id (agent-level info: tenant, gateway, broker),
// preserving callers that want "the agent" rather than a specific replica.
func (r *Registry) Get(id string) (AgentProcess, bool) {
	set, ok := r.sets[id]
	if !ok || len(set) == 0 {
		return AgentProcess{}, false
	}
	return r.withBroker(set[0]), true
}

// Replicas returns the ordered replica set for id (broker attached to each).
func (r *Registry) Replicas(id string) ([]AgentProcess, bool) {
	set, ok := r.sets[id]
	if !ok {
		return nil, false
	}
	out := make([]AgentProcess, len(set))
	for i := range set {
		out[i] = r.withBroker(set[i])
	}
	return out, true
}

// Replica returns one replica by index (broker attached). false if id unknown
// or i out of range.
func (r *Registry) Replica(id string, i int) (AgentProcess, bool) {
	set, ok := r.sets[id]
	if !ok || i < 0 || i >= len(set) {
		return AgentProcess{}, false
	}
	return r.withBroker(set[i]), true
}

// NextReplica returns the next replica index for a NEW session, round-robin via
// an atomic per-agent counter. Blind to liveness. Returns 0 for unknown ids.
func (r *Registry) NextReplica(id string) int {
	set, ok := r.sets[id]
	if !ok || len(set) == 0 {
		return 0
	}
	n := r.rr[id].Add(1) - 1
	return int(n % uint64(len(set)))
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

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./controlplane/ -run 'Registry' -v`
Expected: PASS.

- [ ] **Step 6: Run the whole controlplane package to catch caller breakage**

Run: `go build ./... && go test ./controlplane/ 2>&1 | tail -20`
Expected: It MAY fail in `api.go`/tests because `Get` semantics are preserved but routing isn't replica-aware yet — that's fine for this task as long as it compiles. If `go build ./...` fails, fix only compile errors caused by the registry change (the public method set is a superset of before, so existing `Get`/`List`/`SetBroker`/`SetGateway`/`AgentTenants` callers are unaffected).

- [ ] **Step 7: Commit**

```bash
git add controlplane/registry.go controlplane/proxy.go controlplane/registry_test.go
git commit -m "feat(registry): expand agents into replica sets (Replicas/Replica/NextReplica)"
```

---

## Task 5: Spawn env — inject `DBOS__VMID` + `RUNTIME_AGENT_REPLICA`

**Files:**
- Modify: `controlplane/proxy.go` (`buildEnv`)
- Test: `controlplane/proxy_test.go`

- [ ] **Step 1: Write the failing test**

Add to `controlplane/proxy_test.go`:

```go
func TestBuildEnv_ReplicaIdentity(t *testing.T) {
	ap := AgentProcess{
		AgentID: "support", Addr: "127.0.0.1:8102", PGDSN: "dsn",
		ReplicaIndex: 1, DBOSVMID: "support#1",
	}
	env, err := ap.buildEnv(context.Background())
	if err != nil {
		t.Fatalf("buildEnv: %v", err)
	}
	want := map[string]string{
		"DBOS__VMID":            "support#1",
		"RUNTIME_AGENT_REPLICA": "1",
		"RUNTIME_LISTEN_ADDR":   "127.0.0.1:8102",
	}
	got := map[string]string{}
	for _, e := range env {
		for k := range want {
			if strings.HasPrefix(e, k+"=") {
				got[k] = strings.TrimPrefix(e, k+"=")
			}
		}
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("env %s: got %q want %q", k, got[k], v)
		}
	}
}
```

Ensure `proxy_test.go` imports `"strings"` and `"context"`.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./controlplane/ -run BuildEnv_ReplicaIdentity -v`
Expected: FAIL — `DBOS__VMID` and `RUNTIME_AGENT_REPLICA` not present.

- [ ] **Step 3: Inject the vars in `buildEnv`**

In `controlplane/proxy.go` `buildEnv`, in the first `append` block (line ~56), add the two vars. Add `"strconv"` to the imports. The block becomes:

```go
	env := append(os.Environ(),
		"RUNTIME_PG_DSN="+a.PGDSN,
		"RUNTIME_LISTEN_ADDR="+a.Addr,
		"RUNTIME_AGENT_ID="+a.AgentID,
		"RUNTIME_AGENT_KIND="+a.Kind,
		"RUNTIME_AGENT_TENANT="+a.Tenant,
		"RUNTIME_AGENT_REPLICA="+strconv.Itoa(a.ReplicaIndex),
		"DBOS__VMID="+a.DBOSVMID,
	)
```

(`a.DBOSVMID` is "" only for remote agents, which are never spawned, so a local spawn always carries a real id. An empty `DBOS__VMID` is ignored by DBOS, which falls back to "local" — harmless and never reached here.)

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./controlplane/ -run BuildEnv -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add controlplane/proxy.go controlplane/proxy_test.go
git commit -m "feat(spawn): inject DBOS__VMID + RUNTIME_AGENT_REPLICA per replica"
```

---

## Task 6: Routing — `NewAPI` takes a store; classify request shape + affinity

**Files:**
- Modify: `controlplane/api.go`, `cmd/runtimed/main.go` (NewAPI caller), `controlplane/api_dial_test.go`, `controlplane/router_test.go`
- Test: `controlplane/router_test.go`

- [ ] **Step 1: Write the failing test**

Replace the body of `controlplane/router_test.go`'s test with a multi-replica routing test. First read the existing `TestRouter_DispatchAndList` to mirror its harness style, then add:

```go
func TestAPI_NewSessionRoundRobinsAndPins(t *testing.T) {
	// Two fake replica backends record which one served each request.
	var hits [2]int32
	mk := func(i int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&hits[i], 1)
			if r.URL.Path == "/sessions" && r.Method == "POST" {
				_ = json.NewEncoder(w).Encode(map[string]string{"session_id": "ses-from-" + strconv.Itoa(i)})
				return
			}
			w.WriteHeader(200)
		}))
	}
	b0, b1 := mk(0), mk(1)
	defer b0.Close()
	defer b1.Close()

	// Build a registry whose two replicas point at the fake backends. Parse the
	// httptest hostports into the replica set by hand.
	reg := twoReplicaRegistry(t, "a", b0.URL, b1.URL)

	st := store.NewMemStore()
	// Pre-create a session owned by replica 1 to test affinity.
	owned, _ := st.CreateSession(context.Background(), "a", 1)

	srv := httptest.NewServer(NewAPI(reg, nil, st))
	defer srv.Close()

	// Two POSTs round-robin across replicas 0 then 1.
	httpPost(t, srv.URL+"/agents/a/sessions")
	httpPost(t, srv.URL+"/agents/a/sessions")
	if atomic.LoadInt32(&hits[0]) == 0 || atomic.LoadInt32(&hits[1]) == 0 {
		t.Fatalf("round-robin: hits=%v, want both replicas hit", hits)
	}

	// A session-scoped GET for `owned` must hit replica 1 only.
	before := atomic.LoadInt32(&hits[1])
	httpGet(t, srv.URL+"/agents/a/sessions/"+owned)
	if atomic.LoadInt32(&hits[1]) != before+1 {
		t.Fatalf("affinity: owned session did not pin to replica 1")
	}

	// Unknown session ⇒ 404 (no proxy).
	code := httpGetCode(t, srv.URL+"/agents/a/sessions/ses-nope")
	if code != http.StatusNotFound {
		t.Fatalf("unknown session: got %d want 404", code)
	}
}
```

Add these test helpers to `router_test.go` (and imports: `net/http/httptest`, `sync/atomic`, `strconv`, `encoding/json`, `context`, `net/url`, `strings`, `github.com/sausheong/runtime/internal/store`, `github.com/sausheong/runtime/internal/config`):

```go
// twoReplicaRegistry builds a registry for one agent whose two replicas dial the
// given full base URLs (httptest servers). It bypasses port derivation by
// constructing the set directly via config + then overriding the dial bases.
func twoReplicaRegistry(t *testing.T, id, base0, base1 string) *Registry {
	t.Helper()
	host0 := strings.TrimPrefix(base0, "http://")
	host1 := strings.TrimPrefix(base1, "http://")
	cfg := &config.Config{Agents: []config.AgentConfig{
		{ID: id, Name: id, Model: "m", ListenAddr: host0, Tenant: "default"},
	}}
	r := NewRegistry(cfg, "/bin/agentd", "dsn")
	// Hand-build the two-replica set pointing at the fakes.
	r.sets[id] = []AgentProcess{
		{AgentID: id, Addr: host0, BaseURL: base0, ReplicaIndex: 0, DBOSVMID: id + "#0", Tenant: "default"},
		{AgentID: id, Addr: host1, BaseURL: base1, ReplicaIndex: 1, DBOSVMID: id + "#1", Tenant: "default"},
	}
	return r
}

func httpPost(t *testing.T, url string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(`{"message":"hi"}`))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	resp.Body.Close()
}

func httpGet(t *testing.T, url string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	resp.Body.Close()
}

func httpGetCode(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}
```

Note: `twoReplicaRegistry` accesses `r.sets` — it is in-package (`package controlplane`), so unexported access is fine.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./controlplane/ -run API_NewSession -v`
Expected: FAIL — `NewAPI` takes 2 args, not 3 (`too many arguments`).

- [ ] **Step 3: Rewrite `NewAPI` with the store + request-shape routing**

Replace `controlplane/api.go` entirely with:

```go
package controlplane

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sausheong/runtime/internal/obs"
	"github.com/sausheong/runtime/internal/store"
)

// NewAPI returns the control-plane HTTP handler routing /agents/{id}/... to each
// agent's replica pool, plus GET /agents and GET /healthz. New sessions
// round-robin across replicas; session-scoped requests pin to the owning replica
// (resolved from st); replica-agnostic paths use replica 0. m records
// proxy-error metrics; nil ⇒ no-op. st resolves session→replica affinity.
func NewAPI(reg *Registry, m *obs.ControlMetrics, st store.Store) *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("GET /agents", func(w http.ResponseWriter, r *http.Request) {
		type agentStatus struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Model   string `json:"model"`
			Healthy bool   `json:"healthy"`
		}
		p, hasP := PrincipalFromContext(r.Context())
		infos := reg.List()
		var out []agentStatus
		var mu sync.Mutex
		var wg sync.WaitGroup
		client := &http.Client{Timeout: 1 * time.Second}
		for _, info := range infos {
			if hasP && !p.Superuser && info.Tenant != p.TenantID {
				continue
			}
			replicas, _ := reg.Replicas(info.ID)
			wg.Add(1)
			go func(info AgentInfo, replicas []AgentProcess) {
				defer wg.Done()
				st := agentStatus{ID: info.ID, Name: info.Name, Model: info.Model}
				// An agent is healthy if ANY replica answers /healthz.
				for _, ap := range replicas {
					req, _ := http.NewRequest("GET", ap.baseURL()+"/healthz", nil)
					if ap.AuthToken != "" {
						req.Header.Set("Authorization", "Bearer "+ap.AuthToken)
					}
					resp, err := client.Do(req)
					if err == nil {
						ok := resp.StatusCode == 200
						resp.Body.Close()
						if ok {
							st.Healthy = true
							break
						}
					}
				}
				mu.Lock()
				out = append(out, st)
				mu.Unlock()
			}(info, replicas)
		}
		wg.Wait()
		if out == nil {
			out = []agentStatus{}
		}
		_ = json.NewEncoder(w).Encode(out)
	})

	// Subtree pattern: /agents/{id}/sessions, /agents/{id}/healthz, etc.
	mux.HandleFunc("/agents/{id}/", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if _, ok := reg.Get(id); !ok {
			http.Error(w, "unknown agent "+id, http.StatusNotFound)
			return
		}
		prefix := "/agents/" + id
		r.URL.Path = strings.TrimPrefix(r.URL.Path, prefix)
		if r.URL.Path == "" {
			r.URL.Path = "/"
		}
		r.URL.RawPath = "" // avoid stale encoded-path mismatches after rewrite

		ap, ok := pickReplica(r, reg, st, id)
		if !ok {
			http.Error(w, "unknown session", http.StatusNotFound)
			return
		}
		reverseProxy(ap.baseURL(), ap.AuthToken, func() { m.ProxyError(id) }).ServeHTTP(w, r)
	})

	return mux
}

// pickReplica chooses which replica serves this (already-prefix-stripped)
// request:
//   - POST /sessions (exactly)         → round-robin a new session
//   - /sessions/{sid}[/...]            → pin to the owner replica (from st)
//   - everything else (list, healthz)  → replica 0 (agent-level, replica-agnostic)
//
// Returns ok=false only when a session-scoped path names an unknown session.
func pickReplica(r *http.Request, reg *Registry, st store.Store, id string) (AgentProcess, bool) {
	path := r.URL.Path
	if r.Method == "POST" && path == "/sessions" {
		return reg.Replica(id, reg.NextReplica(id))
	}
	if sid, ok := sessionID(path); ok {
		i, err := st.SessionReplica(r.Context(), sid)
		if err != nil {
			return AgentProcess{}, false
		}
		return reg.Replica(id, i)
	}
	return reg.Replica(id, 0)
}

// sessionID extracts the {sid} from "/sessions/{sid}" or "/sessions/{sid}/...".
// Returns ok=false for "/sessions" and "/sessions/" (the collection, not an
// element).
func sessionID(path string) (string, bool) {
	const p = "/sessions/"
	if !strings.HasPrefix(path, p) {
		return "", false
	}
	rest := path[len(p):]
	if rest == "" {
		return "", false
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	if rest == "" {
		return "", false
	}
	return rest, true
}
```

- [ ] **Step 4: Update the `NewAPI` callers**

In `cmd/runtimed/main.go` (line ~378), the control-plane store handle is needed. Find where `apiMux := controlplane.NewAPI(reg, cm)` is and change it to pass a store. Build the store once near the identity DB setup (after `identityDB` is opened, line ~135 area) — reuse the DSN:

```go
	ctlStore, err := store.NewPGStore(ctx, dsn)
	if err != nil {
		slog.Error("control store init failed", "err", err)
		os.Exit(1)
	}
	defer ctlStore.Close()
```

Add `"github.com/sausheong/runtime/internal/store"` to the imports. Then:

```go
	apiMux := controlplane.NewAPI(reg, cm, ctlStore)
```

In `controlplane/api_dial_test.go` (line ~38), update `NewAPI(reg, nil)` → `NewAPI(reg, nil, store.NewMemStore())` and add the store import. In `controlplane/router_test.go`, the existing `TestRouter_DispatchAndList` call `NewAPI(reg, nil)` → `NewAPI(reg, nil, store.NewMemStore())`.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./controlplane/ -v`
Expected: PASS (all, including the new routing test and the updated existing tests).

- [ ] **Step 6: Verify the whole build**

Run: `go build ./... && go vet ./controlplane/ ./cmd/runtimed/`
Expected: clean.

- [ ] **Step 7: Commit**

```bash
git add controlplane/api.go controlplane/router_test.go controlplane/api_dial_test.go cmd/runtimed/main.go
git commit -m "feat(routing): replica-aware dispatch (round-robin new, pin session-scoped)"
```

---

## Task 7: Supervision — one supervisor per replica

**Files:**
- Modify: `cmd/runtimed/main.go`
- Test: none new (behavior covered by the integration test; this is a wiring change verified by build + the existing remote-agent test).

- [ ] **Step 1: Read the current boot loop**

In `cmd/runtimed/main.go`, the agent-start loop (line ~269) iterates `reg.List()`, calls `reg.Get(info.ID)`, and either monitors a remote or supervises a local with one `Supervisor`.

- [ ] **Step 2: Rewrite the loop to iterate replicas**

Replace the loop body so locals supervise every replica:

```go
	for _, info := range reg.List() {
		replicas, _ := reg.Replicas(info.ID)
		for _, ap := range replicas {
			ap := ap // capture
			if ap.Remote {
				id := ap.AgentID
				hm := &controlplane.HealthMonitor{
					BaseURL: ap.DialBase(), Token: ap.AuthToken,
					OnChange: func(ok bool) { cm.AgentReachable(id, ap.ReplicaIndex, ok) },
				}
				go hm.Run(ctx)
				slog.Info("monitoring remote agent", "agent", ap.AgentID, "url", ap.DialBase())
				continue
			}
			idx := ap.ReplicaIndex
			sup := &controlplane.Supervisor{
				Spawn:     ap.SpawnFunc(),
				Backoff:   time.Second,
				OnRestart: func() { cm.AgentRestart(ap.AgentID, idx) },
			}
			go sup.Run(ctx)
			slog.Info("supervising agent replica", "agent", ap.AgentID, "replica", idx, "addr", ap.Addr)
			if err := waitAgentHealthy(ctx, ap.Addr, 30*time.Second); err != nil {
				slog.Warn("agent replica not healthy yet; continuing", "agent", ap.AgentID, "replica", idx, "err", err)
			}
		}
	}
```

Note: this references `cm.AgentReachable(id, ap.ReplicaIndex, ok)` and `cm.AgentRestart(ap.AgentID, idx)` — the new metric signatures land in Task 8. To keep this task building on its own, Task 8 is sequenced immediately after and they commit together if needed; if building Task 7 alone, temporarily call the old signatures and fix in Task 8. **Preferred:** do Task 7 and Task 8 back to back, building after Task 8.

- [ ] **Step 3: Build**

Run: `go build ./cmd/runtimed/`
Expected: FAILS until Task 8 updates the metric signatures (`AgentRestart`/`AgentReachable` arity). Proceed to Task 8, then build.

- [ ] **Step 4: Commit (after Task 8 builds)**

Defer the commit; commit Task 7 + Task 8 together at the end of Task 8.

---

## Task 8: Metrics — per-replica fan-out + `replica` label

**Files:**
- Modify: `internal/obs/obs.go`, `internal/obs/fanout.go`, `cmd/runtimed/main.go`
- Test: `internal/obs/metrics_test.go` (or wherever obs gauges are tested), plus build

- [ ] **Step 1: Write the failing test**

Add to `internal/obs` test file (e.g. `internal/obs/obs_test.go`; create if absent):

```go
func TestControlMetrics_ReplicaLabels(t *testing.T) {
	c := NewControlMetrics()
	// New signatures take a replica index; must not panic and must register.
	c.AgentRestart("a", 1)
	c.AgentReachable("rem", 0, true)
	// Nil-safe (no panic on nil receiver).
	var n *ControlMetrics
	n.AgentRestart("a", 0)
	n.AgentReachable("a", 0, false)
}
```

(Check the exact constructor name — likely `NewControlMetrics()`. Match the existing tests in the package.)

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/obs/ -run ReplicaLabels -v`
Expected: FAIL — `too many arguments` for `AgentRestart`/`AgentReachable`.

- [ ] **Step 3: Add the `replica` label to the gauges/counters**

In `internal/obs/obs.go`, update the registrations (lines ~62–69):

```go
	c.agentReachable = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Name: "agent_reachable",
		Help: "Remote agent reachability (1=reachable,0=not), per replica.",
	}, []string{"agent", "replica"})
	c.agentRestarts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "agent_restarts_total",
		Help: "Agent replica restarts.",
	}, []string{"agent", "replica"})
```

(Use the exact `Namespace`/`Help` text already present; only the label slice changes — append `"replica"`.)

- [ ] **Step 4: Update the methods**

In `internal/obs/obs.go`:

```go
func (c *ControlMetrics) AgentReachable(agent string, replica int, reachable bool) {
	if c == nil {
		return
	}
	v := 0.0
	if reachable {
		v = 1
	}
	c.agentReachable.WithLabelValues(agent, strconv.Itoa(replica)).Set(v)
}

func (c *ControlMetrics) AgentRestart(agent string, replica int) {
	if c == nil {
		return
	}
	c.agentRestarts.WithLabelValues(agent, strconv.Itoa(replica)).Inc()
}
```

Ensure `strconv` is imported in `obs.go` (it is — used by `HTTPObserved`).

- [ ] **Step 5: Update the scrape-target fan-out**

In `internal/obs/fanout.go`, add a `Replica` field to `ScrapeTarget`:

```go
type ScrapeTarget struct {
	Agent   string
	Replica int    // 0-based replica index, surfaced as the "replica" label
	BaseURL string
	Token   string
}
```

Then find where the agent label is injected per scrape (`injectAgentLabel(mf, res.agent)`, line ~110, and `injectAgentLabel` at ~148). Generalize so the replica label is also injected. Change the call site to pass the target's replica and update `injectAgentLabel` to also set `replica`:

```go
// at the call site (~line 110): pass replica through
				injectTargetLabels(mf, res.agent, res.replica)
```

Rename/extend `injectAgentLabel` to `injectTargetLabels` (and update the per-result struct to carry `replica int`). Implementation:

```go
const replicaLabel = "replica"

// injectTargetLabels overwrites (or appends) agent=<agent> and replica=<replica>
// on every metric in mf, then re-sorts labels. The registered target identity is
// authoritative; agents are not trusted to label themselves.
func injectTargetLabels(mf *dto.MetricFamily, agent string, replica int) {
	aName, rName := agentLabel, replicaLabel
	aVal := agent
	rVal := strconv.Itoa(replica)
	for _, m := range mf.Metric {
		setLabel(m, aName, aVal)
		setLabel(m, rName, rVal)
		sort.Slice(m.Label, func(i, j int) bool {
			return m.Label[i].GetName() < m.Label[j].GetName()
		})
	}
}

// setLabel overwrites an existing label of the given name or appends it.
func setLabel(m *dto.Metric, name, val string) {
	for _, lp := range m.Label {
		if lp.GetName() == name {
			lp.Value = &val
			return
		}
	}
	n, v := name, val
	m.Label = append(m.Label, &dto.LabelPair{Name: &n, Value: &v})
}
```

Add `"strconv"` to `fanout.go` imports if not present. Update the internal per-scrape result struct (search for the struct that holds `agent` + the parsed families, e.g. `res`) to carry `replica int`, set from the `ScrapeTarget.Replica` when the sub-scrape is launched.

- [ ] **Step 6: Update the scrape-target builder in main.go**

In `cmd/runtimed/main.go` `mountMetrics` (line ~243), iterate replicas:

```go
	handler = mountMetrics(handler, cm, func() []obs.ScrapeTarget {
		var ts []obs.ScrapeTarget
		for _, info := range reg.List() {
			replicas, _ := reg.Replicas(info.ID)
			for _, ap := range replicas {
				ts = append(ts, obs.ScrapeTarget{
					Agent: ap.AgentID, Replica: ap.ReplicaIndex,
					BaseURL: ap.DialBase(), Token: ap.AuthToken,
				})
			}
		}
		return ts
	})
```

- [ ] **Step 7: Build and run tests**

Run: `go build ./... && go test ./internal/obs/ ./controlplane/ ./cmd/runtimed/ -v 2>&1 | tail -30`
Expected: PASS; the build now succeeds (Task 7's metric calls resolve).

- [ ] **Step 8: Commit Task 7 + Task 8 together**

```bash
git add internal/obs/ cmd/runtimed/main.go
git commit -m "feat(supervision,metrics): per-replica supervisors + replica-labeled metrics"
```

---

## Task 9: Integration test — pools, affinity, per-replica durability

**Files:**
- Create: `test/replica_pools_test.go`
- Reference: `test/remote_agent_test.go` and `test/resume_test.go` for the harness pattern (process spawn, DB setup, helpers).

- [ ] **Step 1: Read the existing harness**

Read `test/remote_agent_test.go` and `test/resume_test.go` to reuse their patterns: building agentd, writing a temp `runtime.yaml`, starting runtimed, polling `/agents`, and self-cleaning the DB + `dbos` schema.

- [ ] **Step 2: Write the integration test**

Create `test/replica_pools_test.go`:

```go
//go:build integration

package test

import (
	"context"
	"database/sql"
	"testing"
	"time"
	// plus the helpers/imports used by remote_agent_test.go / resume_test.go
)

// TestReplicaPoolsAffinity proves: new sessions distribute across replicas; a
// session-scoped request pins to its owner; killing one replica 503s only its
// sessions and, on restart at the same index (same DBOS__VMID), it recovers its
// own in-flight work without the other replica double-executing.
func TestReplicaPoolsAffinity(t *testing.T) {
	dsn := testDSN(t)         // matches the helper name used elsewhere
	resetDB(t, dsn)          // self-clean: drop app rows + dbos schema
	defer resetDB(t, dsn)

	// runtime.yaml: one agent, replicas: 2, scripted model.
	cfg := `
agents:
  - id: pool
    name: Pool Agent
    model: test/scripted
    listen_addr: 127.0.0.1:8701
    replicas: 2
`
	// Start runtimed with this config (reuse the spawn helper from the suite).
	stop := startRuntimed(t, dsn, cfg) // returns a cancel/cleanup
	defer stop()

	waitCtlHealthy(t, "http://127.0.0.1:8080", 30*time.Second)

	// Gate 1 — distribution: create several sessions, assert both replica
	// indices appear in the sessions table.
	var ids []string
	for i := 0; i < 6; i++ {
		ids = append(ids, createSession(t, "http://127.0.0.1:8080", "pool", "hello"))
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seen := map[int]int{}
	for _, id := range ids {
		var r int
		if err := db.QueryRow(`SELECT replica FROM sessions WHERE id=$1`, id).Scan(&r); err != nil {
			t.Fatalf("read replica for %s: %v", id, err)
		}
		seen[r]++
	}
	if seen[0] == 0 || seen[1] == 0 {
		t.Fatalf("distribution: replicas not both used: %v", seen)
	}

	// Gate 2 — affinity: a session-scoped GET returns the same session's status
	// (proves it reached the owner; a wrong replica would 404/empty since the
	// workflow + subscriber set live on the owner).
	for _, id := range ids {
		st := getSessionStatus(t, "http://127.0.0.1:8080", "pool", id)
		if st == "" {
			t.Fatalf("affinity: empty status for %s (likely wrong replica)", id)
		}
	}

	// Gate 3 — per-replica durability: documented as the kill/restart scenario.
	// Drive a session on replica 1 mid-turn, kill that replica's PID, assert its
	// sessions 503 while replica 0 serves, then let the supervisor restart it
	// (same id#1) and assert it recovers. (Implement using the same PID-kill
	// approach as resume_test.go; if the suite spawns replicas as child PIDs of
	// runtimed, locate the child by listen port 8702.)
	// NOTE: keep this gate honest — if the harness cannot isolate a single
	// replica PID, assert the weaker invariant that turn counts never exceed the
	// number of user turns (no double execution), which the executor-id split
	// guarantees.
}
```

Fill in the helper names to match the suite's actual helpers (read step 1). The test MUST compile and run under `-tags integration`. Keep Gate 3 honest per the NOTE.

- [ ] **Step 3: Run the integration test**

Ensure Postgres.app is running, then:

Run: `go test -tags integration ./test/ -run TestReplicaPoolsAffinity -v`
Expected: PASS (all gates).

- [ ] **Step 4: Run the full integration suite (regression)**

Run: `go test -tags integration ./test/ 2>&1 | tail -30`
Expected: PASS — especially `TestRemoteAgentAttach` (registry change) and the resume test (executor identity). Fix any fallout.

- [ ] **Step 5: Commit**

```bash
git add test/replica_pools_test.go
git commit -m "test(integration): replica pools distribution + affinity + durability"
```

---

## Task 10: Docs — README + ROADMAP

**Files:**
- Modify: `README.md`, `ROADMAP.md`, `runtime.yaml`

- [ ] **Step 1: Document `replicas:` in `runtime.yaml`**

Add a commented example to `runtime.yaml` near the agent list:

```yaml
  # Replica pool (Spine A1): run N agentd processes for one agent. Replica i
  # listens on base_port+i (so listen_addr is the base). New sessions round-robin
  # across replicas; each session pins to its owner replica for life. Omit or 1 =
  # single process (default). Remote (url:) agents may not set replicas.
  #   replicas: 3
```

- [ ] **Step 2: Add a README subsection**

In `README.md`, near the agent-config / spine section, add a "Replica pools & session affinity" subsection: what `replicas: N` does (derived ports, round-robin new sessions, owner-pinned session-scoped requests), the executor-id invariant (each replica is a stable DBOS executor `<id>#<i>`; restart-at-same-index recovers its own work), and the owner-down behavior (session-scoped requests 503 until the owner restarts; new sessions round-robin blind). State the deferrals (autoscaling, drain, dynamic count, skip-down routing, remote replicas).

- [ ] **Step 3: Add the ROADMAP milestone entry**

In `ROADMAP.md`, under section A (Spine hardening), add a "**A1 — DONE (2026-06-13)**" entry mirroring the C3 entry's style: what shipped (replica sets, derived ports, `DBOS__VMID` per replica, `replica` column + affinity routing, per-replica supervision/health/metrics), the executor-id correctness crux, what's deferred (A2/drain/dynamic/skip-down/remote replicas), and the spec/plan paths. Update the checkpoint line at the top.

- [ ] **Step 4: Verify nothing else references the old behavior**

Run: `grep -rn "one subprocess per agent\|one supervisor per agent\|single agentd" README.md ROADMAP.md`
Update any now-stale phrasing.

- [ ] **Step 5: Commit**

```bash
git add README.md ROADMAP.md runtime.yaml
git commit -m "docs: replica pools (Spine A1) — README + ROADMAP + runtime.yaml example"
```

---

## Final verification (after all tasks)

- [ ] `go build ./...` — clean
- [ ] `go vet ./...` — clean
- [ ] `go test ./...` — all hermetic tests pass
- [ ] `go test -tags integration ./test/ 2>&1 | tail -30` — all integration tests pass (Postgres.app up)
- [ ] Live proof per spec §4 (replicas: 3; distribution; affinity stream; kill+recover; per-replica metrics; replicas:1 back-compat)

---

## Spec coverage check

- §1 executor-id invariant → Tasks 4 (DBOSVMID), 5 (inject), 7 (per-replica supervise), 9 (durability gate).
- §1 three bindings (workflow/SSE/persisted) → Tasks 2 (column), 3 (stamp), 6 (affinity route).
- §2 config → Task 1. registry → Task 4. spawn env → Task 5. agentd → Task 3. store → Task 2. routing → Task 6.
- §3 supervision → Task 7. health OR (`GET /agents` any-replica-healthy) → Task 6 (the api.go rewrite). metrics fan-out + `replica` label → Task 8. owner-down 503 → Task 6 (reverseProxy ErrorHandler, unchanged) + verified in Task 9.
- §4 unit tests → Tasks 1–8. integration → Task 9. live proof → final verification.
- Deferrals → documented in Task 10.
