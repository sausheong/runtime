# Spine A2 — Autoscaling Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an opt-in autoscaled local agent float its replica count between `min` and `max` at runtime — runtimed grows the pool by spawning replica processes and shrinks it by draining-then-stopping the highest replica — driven by active-session load, never sacrificing a durable session.

**Architecture:** A new `controlplane.PoolManager` (one per autoscaled agent) owns that agent's mutable replica set, its Supervisor goroutines, and its scale decisions behind a single mutex. The Registry delegates `Replicas`/`Replica`/`NextReplica` to the PoolManager for autoscaled agents; static agents keep A1's lock-free slice path untouched. A per-PoolManager policy goroutine reads active-sessions-per-replica from Postgres each tick and takes at most one suffix-only step (grow / drain / un-drain), reaping drained-to-zero top replicas.

**Tech Stack:** Go 1.25, Postgres (Postgres.app for integration), DBOS executor-id-per-replica (from A1), Prometheus (`internal/obs`), the scripted model (`test/scripted`, no LLM key).

**Reference spec:** `docs/superpowers/specs/2026-06-13-spine-a2-autoscaling-design.md`

---

## Conventions (every task)

- The **`go` CLI is ground truth.** Ignore IDE/LSP diagnostics — the `replace ../harness` directive makes tooling lag; phantom "undefined" errors that `go build`/`go test` don't report are stale.
- **gofmt-clean before every commit:** `gofmt -w <files>` then `go build ./... && go vet ./...`.
- Hermetic unit tests run under `go test ./...` (no Postgres). The integration test (Task 9) is `//go:build integration` and needs Postgres.app at `postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable`.
- Commit messages end with:
  `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`
- **Branch:** all tasks land on `feat/spine-a2-autoscaling` (created off `master` in Task 1).

---

## File Structure

| File | Responsibility | Tasks |
|---|---|---|
| `internal/config/config.go` | `AutoscaleConfig` type, `Autoscale` field, validation, `max`-range port reservation, `ReplicaAddr(i)` | 1 |
| `internal/store/store.go` + `pgstore.go` + `memstore.go` | `ActiveSessionsByReplica` load read | 2 |
| `internal/obs/obs.go` | autoscale gauges + events counter helpers | 3 |
| `controlplane/poolmanager.go` (new) | PoolManager: set ops, read methods, policy loop | 4, 5, 6 |
| `controlplane/registry.go` | `pools` map + delegation + `SetBroker`/`SetGateway` stamping | 7 |
| `cmd/runtimed/main.go` | branch boot loop: autoscaled agent ⇒ PoolManager start + policy goroutine | 8 |
| `test/autoscale_test.go` (new) | integration test (`//go:build integration`) | 9 |
| `README.md`, `ROADMAP.md`, `runtime.yaml` | docs + example | 10 |

---

## Task 1: Config — `autoscale` block, validation, `max`-range ports, `ReplicaAddr(i)`

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

A1 added `Replicas int` and `ReplicaAddrs() ([]string, error)` (config.go:27, 388). A2 adds the optional `autoscale:` block, refactors `ReplicaAddrs` onto a new single-index `ReplicaAddr(i)`, reserves the whole `max` port range in `Validate`, and rejects `autoscale` on remote agents.

- [ ] **Step 1: Write the failing tests**

Add to `internal/config/config_test.go`:

```go
func TestAutoscaleParsesAndValidates(t *testing.T) {
	yaml := `
agents:
  - id: a
    name: A
    model: m
    listen_addr: 127.0.0.1:9100
    autoscale: {min: 1, max: 3, target_sessions_per_replica: 2}
`
	p := writeTmp(t, yaml)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	a := c.Agents[0]
	if a.Autoscale == nil || a.Autoscale.Min != 1 || a.Autoscale.Max != 3 || a.Autoscale.TargetSessionsPerReplica != 2 {
		t.Fatalf("autoscale not parsed: %+v", a.Autoscale)
	}
}

func TestAutoscaleRejectsBadBounds(t *testing.T) {
	cases := []string{
		`{min: 0, max: 3, target_sessions_per_replica: 2}`,  // min < 1
		`{min: 3, max: 2, target_sessions_per_replica: 2}`,  // min > max
		`{min: 1, max: 3, target_sessions_per_replica: 0}`,  // target < 1
	}
	for _, as := range cases {
		yaml := "agents:\n  - id: a\n    name: A\n    model: m\n    listen_addr: 127.0.0.1:9100\n    autoscale: " + as + "\n"
		if _, err := Load(writeTmp(t, yaml)); err == nil {
			t.Fatalf("expected rejection for autoscale %s", as)
		}
	}
}

func TestAutoscaleRejectedOnRemote(t *testing.T) {
	yaml := `
agents:
  - id: a
    name: A
    model: m
    url: https://h:8443
    autoscale: {min: 1, max: 3, target_sessions_per_replica: 2}
`
	if _, err := Load(writeTmp(t, yaml)); err == nil {
		t.Fatalf("expected autoscale rejected on remote agent")
	}
}

func TestAutoscaleReservesMaxPortRange(t *testing.T) {
	// agent a uses max:3 ⇒ reserves 9100,9101,9102; agent b at 9101 must collide.
	yaml := `
agents:
  - id: a
    name: A
    model: m
    listen_addr: 127.0.0.1:9100
    autoscale: {min: 1, max: 3, target_sessions_per_replica: 2}
  - id: b
    name: B
    model: m
    listen_addr: 127.0.0.1:9101
`
	if _, err := Load(writeTmp(t, yaml)); err == nil {
		t.Fatalf("expected derived-port collision against reserved max range")
	}
}

func TestReplicaAddrSingleIndex(t *testing.T) {
	a := AgentConfig{ID: "a", ListenAddr: "127.0.0.1:9100"}
	got, err := a.ReplicaAddr(2)
	if err != nil || got != "127.0.0.1:9102" {
		t.Fatalf("ReplicaAddr(2) = %q, %v; want 127.0.0.1:9102", got, err)
	}
	if _, err := a.ReplicaAddr(70000); err == nil {
		t.Fatalf("expected out-of-range error for huge index")
	}
}
```

`writeTmp(t, body)` already exists in `internal/config/config_test.go` (it writes the body to `filepath.Join(t.TempDir(), "runtime.yaml")` and returns the path) — reuse it; do not redefine it.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/config/ -run 'Autoscale|ReplicaAddr' -v`
Expected: FAIL (compile error: `Autoscale` field and `ReplicaAddr` method undefined).

- [ ] **Step 3: Add the `AutoscaleConfig` type and field**

In `internal/config/config.go`, add after the `AgentConfig` struct (after line 40):

```go
// AutoscaleConfig, when present on a local agent, makes its replica pool float
// between Min and Max driven by active-session load. Absent (nil) ⇒ the static
// A1 pool (Replicas, or 1). See docs/superpowers/specs/2026-06-13-spine-a2-*.
type AutoscaleConfig struct {
	Min                      int `yaml:"min"`
	Max                      int `yaml:"max"`
	TargetSessionsPerReplica int `yaml:"target_sessions_per_replica"`
}
```

Add the field to `AgentConfig` (after the `Replicas` line, config.go:27):

```go
	Autoscale *AutoscaleConfig `yaml:"autoscale"` // optional; nil ⇒ static A1 behavior (Replicas).
```

- [ ] **Step 4: Add `ReplicaAddr(i)` and refactor `ReplicaAddrs` onto it**

Replace the existing `ReplicaAddrs` method (config.go:388-409) with:

```go
// ReplicaAddr returns the derived host:base_port+i listen address for replica i
// of a local agent. Errors if the base listen_addr has no parseable numeric port
// or the derived port falls outside 1..65535.
func (a AgentConfig) ReplicaAddr(i int) (string, error) {
	host, portStr, err := net.SplitHostPort(a.ListenAddr)
	if err != nil {
		return "", fmt.Errorf("agent %q listen_addr %q: %w", a.ID, a.ListenAddr, err)
	}
	base, err := strconv.Atoi(portStr)
	if err != nil {
		return "", fmt.Errorf("agent %q listen_addr %q: port not numeric: %w", a.ID, a.ListenAddr, err)
	}
	port := base + i
	if base < 1 || port < 1 || port > 65535 {
		return "", fmt.Errorf("agent %q: derived replica port %d (base %d + index %d) out of range (1-65535)", a.ID, port, base, i)
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

// ReplicaAddrs returns the derived listen addresses for a local agent's STATIC
// pool: replica i listens on base_host:base_port+i. Replicas <= 0 means 1. Not
// meaningful for remote agents (Validate rejects replicas there).
func (a AgentConfig) ReplicaAddrs() ([]string, error) {
	n := a.Replicas
	if n <= 0 {
		n = 1
	}
	out := make([]string, n)
	for i := 0; i < n; i++ {
		addr, err := a.ReplicaAddr(i)
		if err != nil {
			return nil, err
		}
		out[i] = addr
	}
	return out, nil
}

// reservedAddrs returns the set of derived listen addresses to reserve in the
// dial-uniqueness map for a local agent. An autoscaled agent reserves its WHOLE
// max range (so a grown replica always finds a free, non-colliding port); a
// static agent reserves only its Replicas addresses.
func (a AgentConfig) reservedAddrs() ([]string, error) {
	if a.Autoscale != nil {
		out := make([]string, a.Autoscale.Max)
		for i := 0; i < a.Autoscale.Max; i++ {
			addr, err := a.ReplicaAddr(i)
			if err != nil {
				return nil, err
			}
			out[i] = addr
		}
		return out, nil
	}
	return a.ReplicaAddrs()
}
```

- [ ] **Step 5: Wire validation + max-range reservation into `Validate`**

In `Validate`, the remote-agent rejection check (config.go:183) currently rejects `a.Replicas > 1`. Extend it to also reject `autoscale`:

```go
		if len(a.Command) > 0 || a.WorkDir != "" || a.Kind != "" || a.Memory || a.Gateway.Enabled() || a.Replicas > 1 || a.Autoscale != nil {
			return fmt.Errorf("config: remote agent %q must not set command, workdir, kind, memory, gateway, replicas, or autoscale (these are spawn-time only)", a.ID)
		}
```

In the local-agent branch of `Validate` (config.go:204-215), replace the `a.ReplicaAddrs()` call with autoscale-bounds validation + `reservedAddrs()`:

```go
		} else {
			if a.Autoscale != nil {
				as := a.Autoscale
				if as.Min < 1 || as.Min > as.Max {
					return fmt.Errorf("config: agent %q autoscale requires 1 <= min <= max (got min=%d max=%d)", a.ID, as.Min, as.Max)
				}
				if as.TargetSessionsPerReplica < 1 {
					return fmt.Errorf("config: agent %q autoscale target_sessions_per_replica must be >= 1 (got %d)", a.ID, as.TargetSessionsPerReplica)
				}
				if a.Replicas > 0 {
					// One source of truth for size in autoscale mode.
					fmt.Fprintf(os.Stderr, "config: agent %q sets both replicas and autoscale; replicas is ignored (autoscale starts at min=%d)\n", a.ID, as.Min)
				}
			}
			addrs, err := a.reservedAddrs()
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

- [ ] **Step 6: Run tests to verify they pass**

Run: `gofmt -w internal/config/config.go internal/config/config_test.go && go test ./internal/config/ -run 'Autoscale|ReplicaAddr' -v`
Expected: PASS. Then `go build ./... && go vet ./...` (clean).

Also run the full config suite to confirm no A1 regression:
Run: `go test ./internal/config/`
Expected: PASS (A1's `Replicas`/`ReplicaAddrs` tests still green).

- [ ] **Step 7: Create the branch and commit**

```bash
git checkout master && git pull --ff-only || true
git checkout -b feat/spine-a2-autoscaling
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): autoscale block + max-range port reservation + ReplicaAddr(i)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 2: Store — `ActiveSessionsByReplica` load read

**Files:**
- Modify: `internal/store/store.go` (interface), `internal/store/pgstore.go`, `internal/store/memstore.go`
- Test: `internal/store/store_test.go`

The policy loop needs per-replica active-session counts. Sessions go `created → running → terminal`, where terminal is `completed` or `error` (verified: no `failed`/`cancelled` are ever written). The query excludes terminal states (so an unknown future status counts as active — fail toward keeping capacity).

- [ ] **Step 1: Write the failing test**

Add to `internal/store/store_test.go` (this is the mem-store-backed table test file; mirror the existing style):

```go
func TestActiveSessionsByReplica(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	// replica 0: two active (created, running); replica 1: one active + one terminal.
	id0a, _ := s.CreateSession(ctx, "ag", 0)
	id0b, _ := s.CreateSession(ctx, "ag", 0)
	_ = s.SetSessionStatus(ctx, id0b, "running")
	id1a, _ := s.CreateSession(ctx, "ag", 1)
	id1done, _ := s.CreateSession(ctx, "ag", 1)
	_ = s.SetSessionStatus(ctx, id1done, "completed")
	// another agent's session must not leak in.
	_, _ = s.CreateSession(ctx, "other", 0)
	_ = id0a
	_ = id1a

	m, err := s.ActiveSessionsByReplica(ctx, "ag")
	if err != nil {
		t.Fatalf("ActiveSessionsByReplica: %v", err)
	}
	if m[0] != 2 {
		t.Fatalf("replica 0 active = %d, want 2", m[0])
	}
	if m[1] != 1 {
		t.Fatalf("replica 1 active = %d, want 1 (terminal excluded)", m[1])
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run ActiveSessionsByReplica -v`
Expected: FAIL (compile error: `ActiveSessionsByReplica` undefined on `Store`).

- [ ] **Step 3: Add the interface method**

In `internal/store/store.go`, add to the `Store` interface (after `SessionReplica`, line 24):

```go
	// ActiveSessionsByReplica returns replica index → count of NON-terminal
	// sessions for the agent (terminal = completed|error). The autoscaler's load
	// read. Replicas with zero active sessions may be absent from the map.
	ActiveSessionsByReplica(ctx context.Context, agentID string) (map[int]int, error)
```

- [ ] **Step 4: Implement in pgStore**

In `internal/store/pgstore.go`, add (after `SessionReplica`, line 81):

```go
func (p *pgStore) ActiveSessionsByReplica(ctx context.Context, agentID string) (map[int]int, error) {
	rows, err := p.db.QueryContext(ctx,
		`SELECT replica, count(*) FROM sessions
		 WHERE agent_id=$1 AND status NOT IN ('completed','error')
		 GROUP BY replica`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]int{}
	for rows.Next() {
		var replica, n int
		if err := rows.Scan(&replica, &n); err != nil {
			return nil, err
		}
		out[replica] = n
	}
	return out, rows.Err()
}
```

- [ ] **Step 5: Implement in memStore**

In `internal/store/memstore.go`, add (after `SessionReplica`, line 37):

```go
func (m *memStore) ActiveSessionsByReplica(_ context.Context, agentID string) (map[int]int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[int]int{}
	for _, s := range m.sessions {
		if s.AgentID != agentID {
			continue
		}
		if s.Status == "completed" || s.Status == "error" {
			continue
		}
		out[s.Replica]++
	}
	return out, nil
}
```

- [ ] **Step 6: Run test to verify it passes**

Run: `gofmt -w internal/store/*.go && go test ./internal/store/ -run ActiveSessionsByReplica -v`
Expected: PASS. Then `go build ./...` (clean — confirms no other Store implementer is now missing the method; if there's a test fake implementing Store, it will fail to compile here and must get the method too).

- [ ] **Step 7: Commit**

```bash
git add internal/store/store.go internal/store/pgstore.go internal/store/memstore.go internal/store/store_test.go
git commit -m "feat(store): ActiveSessionsByReplica load read for autoscaler

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 3: Obs — autoscale gauges + events counter

**Files:**
- Modify: `internal/obs/obs.go`
- Test: `internal/obs/obs_test.go` (or a new `internal/obs/autoscale_test.go`)

Add three per-agent gauges (`desired`, `current`, `active`) and an events counter (`{agent,action}`), all nil-safe like the existing helpers.

- [ ] **Step 1: Write the failing test**

Create `internal/obs/autoscale_test.go`:

```go
package obs

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"net/http"
	"net/http/httptest"
)

func TestAutoscaleMetricsExposed(t *testing.T) {
	c := NewControlMetrics()
	c.AutoscaleDesired("ag", 3)
	c.AutoscaleCurrent("ag", 2)
	c.AutoscaleActive("ag", 5)
	c.AutoscaleEvent("ag", "up")
	c.AutoscaleEvent("ag", "up")

	srv := httptest.NewServer(promhttp.HandlerFor(c.reg, promhttp.HandlerOpts{}))
	defer srv.Close()
	resp, _ := http.Get(srv.URL)
	defer resp.Body.Close()
	buf := new(strings.Builder)
	_, _ = buf.ReadFrom(resp.Body)
	body := buf.String()

	for _, want := range []string{
		`runtime_agent_replicas_desired{agent="ag"} 3`,
		`runtime_agent_replicas_current{agent="ag"} 2`,
		`runtime_agent_active_sessions{agent="ag"} 5`,
		`runtime_autoscale_events_total{action="up",agent="ag"} 2`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in:\n%s", want, body)
		}
	}
}

func TestAutoscaleMetricsNilSafe(t *testing.T) {
	var c *ControlMetrics
	c.AutoscaleDesired("ag", 1) // must not panic
	c.AutoscaleCurrent("ag", 1)
	c.AutoscaleActive("ag", 1)
	c.AutoscaleEvent("ag", "up")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/obs/ -run Autoscale -v`
Expected: FAIL (compile error: helpers + fields undefined).

- [ ] **Step 3: Add the metric fields**

In `internal/obs/obs.go`, add to the `ControlMetrics` struct (after `scrapeSkips`, line 41):

```go
	asDesired *prometheus.GaugeVec
	asCurrent *prometheus.GaugeVec
	asActive  *prometheus.GaugeVec
	asEvents  *prometheus.CounterVec
```

- [ ] **Step 4: Construct and register them**

In `NewControlMetrics`, before the `c.reg.MustRegister(...)` call (line 91), add:

```go
	c.asDesired = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "runtime_agent_replicas_desired",
		Help: "Replica count the autoscaler wants for the agent (clamped to [min,max]).",
	}, []string{"agent"})
	c.asCurrent = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "runtime_agent_replicas_current",
		Help: "Live replica count for the agent (draining replicas included).",
	}, []string{"agent"})
	c.asActive = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "runtime_agent_active_sessions",
		Help: "Non-terminal session count for the agent on the last autoscale tick.",
	}, []string{"agent"})
	c.asEvents = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "runtime_autoscale_events_total",
		Help: "Autoscale actions by agent and action (up/down/undrain/reap/blocked).",
	}, []string{"agent", "action"})
```

And add them to the `MustRegister` call:

```go
	c.reg.MustRegister(c.httpRequests, c.httpDuration, c.agentUp, c.agentReachable, c.agentRestarts,
		c.proxyErrors, c.gwCalls, c.gwDuration, c.gwUp, c.scrapeSkips,
		c.asDesired, c.asCurrent, c.asActive, c.asEvents)
```

- [ ] **Step 5: Add the helpers**

In `internal/obs/obs.go`, add after `ScrapeSkip` (line 180):

```go
// Autoscale action label values for AutoscaleEvent.
const (
	AutoscaleUp      = "up"
	AutoscaleDown    = "down"
	AutoscaleUndrain = "undrain"
	AutoscaleReap    = "reap"
	AutoscaleBlocked = "blocked"
)

func (c *ControlMetrics) AutoscaleDesired(agent string, n int) {
	if c == nil {
		return
	}
	c.asDesired.WithLabelValues(agent).Set(float64(n))
}

func (c *ControlMetrics) AutoscaleCurrent(agent string, n int) {
	if c == nil {
		return
	}
	c.asCurrent.WithLabelValues(agent).Set(float64(n))
}

func (c *ControlMetrics) AutoscaleActive(agent string, n int) {
	if c == nil {
		return
	}
	c.asActive.WithLabelValues(agent).Set(float64(n))
}

func (c *ControlMetrics) AutoscaleEvent(agent, action string) {
	if c == nil {
		return
	}
	c.asEvents.WithLabelValues(agent, action).Inc()
}
```

- [ ] **Step 6: Run test to verify it passes**

Run: `gofmt -w internal/obs/obs.go internal/obs/autoscale_test.go && go test ./internal/obs/ -run Autoscale -v`
Expected: PASS. Then `go test ./internal/obs/` (no regression) and `go build ./...`.

- [ ] **Step 7: Commit**

```bash
git add internal/obs/obs.go internal/obs/autoscale_test.go
git commit -m "feat(obs): autoscale gauges (desired/current/active) + events counter

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 4: PoolManager — construction + set ops (grow/drainTop/undrainTop/reapDrained) + read methods

**Files:**
- Create: `controlplane/poolmanager.go`
- Test: `controlplane/poolmanager_test.go`

This task builds the PoolManager's data structure, the suffix-only set operations, and the read methods — **without** the policy loop (Task 6) and **without** real process spawning in tests (the spawn factory is injected so tests stay hermetic). `grow` uses an injected spawn+wait pair so a unit test never starts a real agentd.

The PoolManager mirrors how A1's `NewRegistry` builds a replica `AgentProcess` (registry.go:67-76): `base` template + per-index `ReplicaIndex`, `Addr`, `BaseURL`, `DBOSVMID`.

- [ ] **Step 1: Write the failing tests**

Create `controlplane/poolmanager_test.go`:

```go
package controlplane

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/sausheong/runtime/internal/config"
)

// newTestPM builds a PoolManager with an injected spawn that never starts a real
// process: grow's readiness wait returns immediately. spawned records indices.
func newTestPM(t *testing.T, min, max, target int) (*PoolManager, *[]int) {
	t.Helper()
	var mu sync.Mutex
	var spawned []int
	base := AgentProcess{AgentID: "ag", BinPath: "/bin/true", PGDSN: "dsn", Tenant: "default"}
	acfg := config.AutoscaleConfig{Min: min, Max: max, TargetSessionsPerReplica: target}
	addrOf := func(i int) (string, error) { return fmt.Sprintf("127.0.0.1:%d", 9100+i), nil }
	pm := newPoolManager("ag", base, acfg, addrOf, nil, nil)
	// Inject a fake spawn+wait: record the index, report healthy instantly.
	pm.startReplica = func(ctx context.Context, ap AgentProcess) (context.CancelFunc, error) {
		mu.Lock()
		spawned = append(spawned, ap.ReplicaIndex)
		mu.Unlock()
		_, cancel := context.WithCancel(ctx)
		return cancel, nil
	}
	return pm, &spawned
}

func TestPoolManagerGrowAppendsSuffix(t *testing.T) {
	pm, _ := newTestPM(t, 1, 3, 2)
	ctx := context.Background()
	if err := pm.grow(ctx); err != nil { // 0 -> 1
		t.Fatal(err)
	}
	if err := pm.grow(ctx); err != nil { // 1 -> 2
		t.Fatal(err)
	}
	reps := pm.Replicas()
	if len(reps) != 2 {
		t.Fatalf("len=%d want 2", len(reps))
	}
	if reps[1].ReplicaIndex != 1 || reps[1].Addr != "127.0.0.1:9101" || reps[1].DBOSVMID != "ag#1" {
		t.Fatalf("replica 1 wrong: %+v", reps[1])
	}
}

func TestPoolManagerGrowRespectsMax(t *testing.T) {
	pm, _ := newTestPM(t, 1, 2, 2)
	ctx := context.Background()
	_ = pm.grow(ctx)
	_ = pm.grow(ctx)
	if err := pm.grow(ctx); err == nil { // 2 -> 3 must fail (max=2)
		t.Fatalf("grow past max should error")
	}
}

func TestPoolManagerDrainAndReap(t *testing.T) {
	pm, _ := newTestPM(t, 1, 3, 2)
	ctx := context.Background()
	_ = pm.grow(ctx) // index 1
	_ = pm.grow(ctx) // index 2; k=3
	pm.drainTop()    // mark index 2 draining
	if !pm.Replicas()[2].draining {
		t.Fatal("top not marked draining")
	}
	// NextReplica must skip the draining top.
	for i := 0; i < 10; i++ {
		if pm.NextReplica() == 2 {
			t.Fatal("NextReplica returned draining replica")
		}
	}
	// reap with index 2 at 0 active ⇒ truncates to k=2.
	pm.reapDrained(map[int]int{0: 1, 1: 1})
	if len(pm.Replicas()) != 2 {
		t.Fatalf("reap did not truncate: k=%d", len(pm.Replicas()))
	}
}

func TestPoolManagerReapOnlyContiguousZero(t *testing.T) {
	pm, _ := newTestPM(t, 1, 3, 2)
	ctx := context.Background()
	_ = pm.grow(ctx) // 1
	_ = pm.grow(ctx) // 2
	pm.drainTop()    // index 2 draining
	// index 2 still has an active session ⇒ must NOT reap.
	pm.reapDrained(map[int]int{2: 1})
	if len(pm.Replicas()) != 3 {
		t.Fatalf("reaped a non-zero replica: k=%d", len(pm.Replicas()))
	}
}

func TestPoolManagerUndrainTop(t *testing.T) {
	pm, _ := newTestPM(t, 1, 3, 2)
	ctx := context.Background()
	_ = pm.grow(ctx)
	pm.drainTop()
	pm.undrainTop()
	if pm.Replicas()[1].draining {
		t.Fatal("undrainTop did not clear draining")
	}
}

func TestPoolManagerReadsRaceClean(t *testing.T) {
	pm, _ := newTestPM(t, 1, 4, 2)
	ctx := context.Background()
	_ = pm.grow(ctx)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = pm.NextReplica()
				_ = pm.Replicas()
				_, _ = pm.Replica(0)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = pm.grow(ctx)
		pm.drainTop()
		pm.reapDrained(map[int]int{0: 1})
	}()
	wg.Wait()
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./controlplane/ -run PoolManager -v`
Expected: FAIL (compile error: `newPoolManager`, `PoolManager`, methods undefined).

- [ ] **Step 3: Write `controlplane/poolmanager.go` (construction + state + set ops + reads)**

```go
package controlplane

import (
	"context"
	"strconv"
	"sync"

	"github.com/sausheong/runtime/internal/config"
	"github.com/sausheong/runtime/internal/obs"
	"github.com/sausheong/runtime/internal/store"
)

// PoolManager owns one autoscaled agent's mutable replica set, the Supervisor
// goroutines behind each replica, and its scale decisions — all serialized by
// one mutex. The Registry delegates Replicas/Replica/NextReplica to it for
// autoscaled agents; static agents never construct one (lock-free slice path).
//
// Invariants (from the executor-id crux, see the A2 design spec):
//   - Suffix-only: only ever append at index k, or remove index k-1.
//   - Drain-before-stop: the top replica is stopped only at 0 active sessions.
type PoolManager struct {
	mu       sync.RWMutex
	agentID  string
	base     AgentProcess
	acfg     config.AutoscaleConfig
	addrOf   func(i int) (string, error) // config.ReplicaAddr bound to this agent
	replicas []replicaSlot               // ordered 0..k-1; len == live count
	rr       uint64                      // new-session round-robin cursor (mu-guarded)

	st      store.Store
	metrics *obs.ControlMetrics

	// startReplica spawns replica ap's Supervisor and returns a cancel that stops
	// it. Real impl (production) starts a controlplane.Supervisor under a child
	// context and waits for /healthz; tests inject a fake. Set by newPoolManager
	// to the production impl; overridable in tests.
	startReplica func(ctx context.Context, ap AgentProcess) (context.CancelFunc, error)
}

type replicaSlot struct {
	ap       AgentProcess
	cancel   context.CancelFunc
	draining bool
}

// newPoolManager builds a PoolManager with zero live replicas. Start (Task 8)
// grows it to min. metrics/st may be nil in unit tests.
func newPoolManager(agentID string, base AgentProcess, acfg config.AutoscaleConfig,
	addrOf func(i int) (string, error), st store.Store, metrics *obs.ControlMetrics) *PoolManager {
	pm := &PoolManager{
		agentID: agentID, base: base, acfg: acfg, addrOf: addrOf,
		st: st, metrics: metrics,
	}
	pm.startReplica = pm.startReplicaProc
	return pm
}

// replicaProcess builds the AgentProcess for index i from the base template,
// mirroring NewRegistry's per-replica construction (registry.go).
func (p *PoolManager) replicaProcess(i int) (AgentProcess, error) {
	addr, err := p.addrOf(i)
	if err != nil {
		return AgentProcess{}, err
	}
	ap := p.base
	ap.ReplicaIndex = i
	ap.Addr = addr
	ap.BaseURL = "http://" + addr
	ap.DBOSVMID = p.agentID + "#" + strconv.Itoa(i)
	return ap, nil
}

// grow appends a replica at index k=len(replicas), spawning THEN publishing so a
// half-started replica is never routable. Errors (and leaves the set unchanged)
// if k would exceed max or spawn/health fails.
func (p *PoolManager) grow(ctx context.Context) error {
	p.mu.Lock()
	k := len(p.replicas)
	p.mu.Unlock()
	if k >= p.acfg.Max {
		return errGrowAtMax
	}
	ap, err := p.replicaProcess(k)
	if err != nil {
		return err
	}
	cancel, err := p.startReplica(ctx, ap)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	// Re-check k under the lock: another grow could have appended meanwhile.
	if len(p.replicas) != k {
		cancel() // discard this spawn; the set moved under us.
		return errGrowRaced
	}
	p.replicas = append(p.replicas, replicaSlot{ap: ap, cancel: cancel})
	return nil
}

// drainTop marks the highest replica draining (no-op if k==0 or already
// draining). New sessions stop routing there immediately; the process keeps
// serving existing sessions and stays supervised.
func (p *PoolManager) drainTop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := len(p.replicas)
	if k == 0 || p.replicas[k-1].draining {
		return
	}
	p.replicas[k-1].draining = true
}

// undrainTop clears the draining flag on the highest replica (the un-drain fast
// path). No-op if the top is not draining.
func (p *PoolManager) undrainTop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := len(p.replicas)
	if k == 0 || !p.replicas[k-1].draining {
		return
	}
	p.replicas[k-1].draining = false
}

// reapDrained stops and removes the contiguous draining suffix whose active
// count (from the supplied per-replica map) is 0. Only contiguous-from-top
// reaping preserves the suffix-only invariant. Never reaps below 1 replica.
func (p *PoolManager) reapDrained(active map[int]int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for len(p.replicas) > 1 {
		k := len(p.replicas)
		top := p.replicas[k-1]
		if !top.draining || active[k-1] > 0 {
			return
		}
		top.cancel()
		p.replicas = p.replicas[:k-1]
		if p.metrics != nil {
			p.metrics.AutoscaleEvent(p.agentID, obs.AutoscaleReap)
		}
	}
}

// Replicas returns a snapshot of the live replica set (broker already on base).
func (p *PoolManager) Replicas() []AgentProcess {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]AgentProcess, len(p.replicas))
	for i := range p.replicas {
		out[i] = p.replicas[i].ap
	}
	return out
}

// Replica returns one replica by index. false if i out of range.
func (p *PoolManager) Replica(i int) (AgentProcess, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if i < 0 || i >= len(p.replicas) {
		return AgentProcess{}, false
	}
	return p.replicas[i].ap, true
}

// NextReplica round-robins over the NON-draining replicas for a new session. If
// every replica is draining (transient; min>=1 keeps one live normally) it falls
// back to index 0.
func (p *PoolManager) NextReplica() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := len(p.replicas)
	if k == 0 {
		return 0
	}
	for tries := 0; tries < k; tries++ {
		idx := int(p.rr % uint64(k))
		p.rr++
		if !p.replicas[idx].draining {
			return idx
		}
	}
	return 0
}
```

Note: the test references `pm.Replicas()[2].draining`, but `Replicas()` returns `[]AgentProcess` (no `draining` field). Fix the test to use an internal accessor instead. Add this test-only accessor to `poolmanager.go`:

```go
// topDraining reports whether the highest replica is marked draining (test
// helper; avoids exposing slot internals).
func (p *PoolManager) topDraining() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	k := len(p.replicas)
	return k > 0 && p.replicas[k-1].draining
}
```

And update the test assertions `pm.Replicas()[2].draining` → `pm.topDraining()` and `pm.Replicas()[1].draining` → `pm.topDraining()` accordingly.

- [ ] **Step 4: Add the sentinel errors and the production spawn impl stub**

Still in `poolmanager.go`, add:

```go
import "errors" // add to the import block

var (
	errGrowAtMax = errors.New("poolmanager: at max replicas")
	errGrowRaced = errors.New("poolmanager: grow raced another grow")
)
```

And the production `startReplicaProc` (real Supervisor + readiness wait). It needs a readiness probe; reuse the same approach main.go uses (`waitAgentHealthy`). To avoid duplicating that helper, accept a readiness function as a field set at Start time:

```go
// startReplicaProc is the production startReplica: it launches a Supervisor for
// ap under a child context and waits until ap answers /healthz (via readyWait).
// readyWait is injected by Start (Task 8) so poolmanager.go does not import the
// readiness helper from package main.
func (p *PoolManager) startReplicaProc(ctx context.Context, ap AgentProcess) (context.CancelFunc, error) {
	rctx, cancel := context.WithCancel(ctx)
	idx := ap.ReplicaIndex
	sup := &Supervisor{
		Spawn:     ap.SpawnFunc(),
		OnRestart: func() { p.metrics.AgentRestart(p.agentID, idx) },
	}
	go sup.Run(rctx)
	if p.readyWait != nil {
		if err := p.readyWait(rctx, ap.Addr); err != nil {
			// Not fatal: the Supervisor keeps retrying. Caller (grow) still
			// publishes; the replica becomes routable once it answers. But for a
			// brand-new top replica we prefer to surface the failure so the tick
			// counts it as blocked and retries next tick — so cancel + error.
			cancel()
			return nil, err
		}
	}
	return cancel, nil
}
```

Add the `readyWait` field to the struct:

```go
	readyWait func(ctx context.Context, addr string) error // injected by Start; nil ⇒ skip
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `gofmt -w controlplane/poolmanager.go controlplane/poolmanager_test.go && go test ./controlplane/ -run PoolManager -v -race`
Expected: PASS (including `-race` on `TestPoolManagerReadsRaceClean`).

- [ ] **Step 6: Commit**

```bash
git add controlplane/poolmanager.go controlplane/poolmanager_test.go
git commit -m "feat(controlplane): PoolManager set ops (grow/drain/undrain/reap) + reads

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 5: PoolManager — decision function (pure, testable)

**Files:**
- Modify: `controlplane/poolmanager.go`
- Test: `controlplane/poolmanager_test.go`

Separate the *decision* (pure arithmetic + cooldown gating) from the *tick* (I/O: DB read + actuation). This task adds the pure decision so it can be table-tested without spawning or DB.

- [ ] **Step 1: Write the failing test**

Add to `controlplane/poolmanager_test.go`:

```go
func TestDecideStep(t *testing.T) {
	acfg := config.AutoscaleConfig{Min: 1, Max: 3, TargetSessionsPerReplica: 2}
	cases := []struct {
		name      string
		active, k int
		topDrain  bool
		upReady   bool // up-cooldown elapsed
		downReady bool
		want      scaleStep
	}{
		{"need up, ready", 5, 1, false, true, true, stepGrow},      // ceil(5/2)=3 > 1
		{"need up, cooling", 5, 1, false, false, true, stepBlocked},
		{"at max", 99, 3, false, true, true, stepBlocked},          // k==max
		{"need down, ready", 1, 3, false, true, true, stepDrain},   // ceil(1/2)=1 < 3
		{"need down, cooling", 1, 3, false, true, false, stepBlocked},
		{"at min", 0, 1, false, true, true, stepNone},              // desired=1==min==k
		{"rebound undrain", 5, 2, true, true, true, stepUndrain},   // desired up & top draining
		{"hold steady", 3, 2, false, true, true, stepNone},         // ceil(3/2)=2==k
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := decideStep(acfg, c.active, c.k, c.topDrain, c.upReady, c.downReady)
			if got != c.want {
				t.Fatalf("decideStep(active=%d,k=%d,drain=%v,up=%v,down=%v)=%v want %v",
					c.active, c.k, c.topDrain, c.upReady, c.downReady, got, c.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./controlplane/ -run TestDecideStep -v`
Expected: FAIL (compile error: `scaleStep`, `decideStep`, step consts undefined).

- [ ] **Step 3: Implement the decision**

Add to `controlplane/poolmanager.go`:

```go
type scaleStep int

const (
	stepNone scaleStep = iota
	stepGrow
	stepDrain
	stepUndrain
	stepBlocked // wanted to act but a cooldown/clamp stopped it (counted, no-op)
)

// decideStep computes the single step to take this tick. Pure: no I/O, no locks.
//   desired = clamp(ceil(active/target), min, max)
//   up:   desired>k && k<max  -> undrain top if draining, else grow
//   down: desired<k && k>min  -> drain top
// Cooldown gates turn a wanted up/down into stepBlocked.
func decideStep(acfg config.AutoscaleConfig, active, k int, topDraining, upReady, downReady bool) scaleStep {
	target := acfg.TargetSessionsPerReplica
	desired := (active + target - 1) / target // ceil
	if desired < acfg.Min {
		desired = acfg.Min
	}
	if desired > acfg.Max {
		desired = acfg.Max
	}
	switch {
	case desired > k && k < acfg.Max:
		if !upReady {
			return stepBlocked
		}
		if topDraining {
			return stepUndrain
		}
		return stepGrow
	case desired < k && k > acfg.Min:
		if !downReady {
			return stepBlocked
		}
		return stepDrain
	default:
		return stepNone
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `gofmt -w controlplane/poolmanager.go controlplane/poolmanager_test.go && go test ./controlplane/ -run TestDecideStep -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add controlplane/poolmanager.go controlplane/poolmanager_test.go
git commit -m "feat(controlplane): pure decideStep autoscale decision (clamped, cooldown-gated)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 6: PoolManager — tick + policy loop (wires decision to actuation)

**Files:**
- Modify: `controlplane/poolmanager.go`
- Test: `controlplane/poolmanager_test.go`

The tick reads load from the store, computes the per-replica active map, calls `decideStep`, actuates (grow/drainTop/undrainTop), records metrics, and always calls `reapDrained`. The loop calls tick on an interval with cooldown bookkeeping. Cooldowns use a monotonic clock injected as `now func() int64` (nanos) so tests are deterministic without sleeping.

- [ ] **Step 1: Write the failing test**

Add to `controlplane/poolmanager_test.go`:

```go
// fakeLoad is a store.Store stub returning a scripted active-by-replica map and
// counting calls. Only ActiveSessionsByReplica is used by tick.
type fakeLoad struct {
	store.Store
	mu  sync.Mutex
	ret map[int]int
	err error
}

func (f *fakeLoad) ActiveSessionsByReplica(_ context.Context, _ string) (map[int]int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	m := map[int]int{}
	for k, v := range f.ret {
		m[k] = v
	}
	return m, nil
}

func TestTickGrowsOnLoad(t *testing.T) {
	pm, spawned := newTestPM(t, 1, 3, 2)
	fl := &fakeLoad{ret: map[int]int{0: 5}} // 5 active on the single replica
	pm.st = fl
	ctx := context.Background()
	// seed the initial min=1 replica.
	if err := pm.grow(ctx); err != nil {
		t.Fatal(err)
	}
	pm.clock = func() int64 { return 1 << 60 } // cooldowns always elapsed
	pm.tick(ctx) // ceil(5/2)=3 > 1 ⇒ one grow
	if got := len(pm.Replicas()); got != 2 {
		t.Fatalf("k=%d want 2 (one step per tick)", got)
	}
	pm.tick(ctx) // still 5 active over 2 replicas? fakeLoad unchanged ⇒ desired 3 > 2 ⇒ grow
	if got := len(pm.Replicas()); got != 3 {
		t.Fatalf("k=%d want 3", got)
	}
	_ = spawned
}

func TestTickFailedReadIsNoOp(t *testing.T) {
	pm, _ := newTestPM(t, 1, 3, 2)
	pm.st = &fakeLoad{err: fmt.Errorf("db down")}
	ctx := context.Background()
	_ = pm.grow(ctx)
	pm.clock = func() int64 { return 1 << 60 }
	pm.tick(ctx) // read fails ⇒ hold
	if got := len(pm.Replicas()); got != 1 {
		t.Fatalf("k=%d want 1 (no scaling on failed read)", got)
	}
}

func TestTickReapsDrainedTop(t *testing.T) {
	pm, _ := newTestPM(t, 1, 3, 2)
	ctx := context.Background()
	_ = pm.grow(ctx) // k=2
	pm.drainTop()    // index 1 draining
	pm.st = &fakeLoad{ret: map[int]int{0: 1}} // index 1 absent ⇒ 0 active
	pm.clock = func() int64 { return 1 << 60 }
	pm.tick(ctx)
	if got := len(pm.Replicas()); got != 1 {
		t.Fatalf("k=%d want 1 (drained top reaped)", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./controlplane/ -run 'TestTick' -v`
Expected: FAIL (compile error: `pm.tick`, `pm.clock` undefined).

- [ ] **Step 3: Implement `tick`, the clock/cooldown fields, and `runPolicy`**

Add the fields to the `PoolManager` struct:

```go
	clock     func() int64 // monotonic nanos; injected (tests) — default time.Now().UnixNano
	lastUp    int64        // last scale-up actuation (nanos)
	lastDown  int64        // last scale-down actuation (nanos)
	upCD      int64        // scale-up cooldown (nanos)
	downCD    int64        // scale-down cooldown (nanos)
	pollEvery int64        // poll interval (nanos)
```

Set defaults in `newPoolManager` (before `return pm`):

```go
	pm.clock = func() int64 { return timeNowNanos() }
	pm.upCD = int64(10 * 1e9)    // 10s default scale-up cooldown
	pm.downCD = int64(30 * 1e9)  // 30s default scale-down cooldown (slower)
	pm.pollEvery = int64(5 * 1e9) // 5s poll
```

Add a tiny indirection so tests don't need real time and `poolmanager.go` keeps one `time` import:

```go
import "time" // add to import block

func timeNowNanos() int64 { return time.Now().UnixNano() }
```

Implement `tick`:

```go
// tick runs one policy iteration: read load, decide, actuate at most one step,
// always reap drained-to-zero top replicas, update gauges. Never scales on a
// failed load read.
func (p *PoolManager) tick(ctx context.Context) {
	active, err := p.st.ActiveSessionsByReplica(ctx, p.agentID)
	if err != nil {
		return // hold current size; never decide on stale data.
	}
	total := 0
	for _, n := range active {
		total += n
	}
	p.mu.RLock()
	k := len(p.replicas)
	topDrain := k > 0 && p.replicas[k-1].draining
	p.mu.RUnlock()

	now := p.clock()
	upReady := now-p.lastUp >= p.upCD
	downReady := now-p.lastDown >= p.downCD

	switch decideStep(p.acfg, total, k, topDrain, upReady, downReady) {
	case stepGrow:
		if err := p.grow(ctx); err != nil {
			p.metrics.AutoscaleEvent(p.agentID, obs.AutoscaleBlocked)
		} else {
			p.lastUp = now
			p.metrics.AutoscaleEvent(p.agentID, obs.AutoscaleUp)
		}
	case stepUndrain:
		p.undrainTop()
		p.lastUp = now
		p.metrics.AutoscaleEvent(p.agentID, obs.AutoscaleUndrain)
	case stepDrain:
		p.drainTop()
		p.lastDown = now
		p.metrics.AutoscaleEvent(p.agentID, obs.AutoscaleDown)
	case stepBlocked:
		p.metrics.AutoscaleEvent(p.agentID, obs.AutoscaleBlocked)
	}

	p.reapDrained(active)

	// Gauges reflect post-step state.
	p.mu.RLock()
	cur := len(p.replicas)
	p.mu.RUnlock()
	target := p.acfg.TargetSessionsPerReplica
	desired := (total + target - 1) / target
	if desired < p.acfg.Min {
		desired = p.acfg.Min
	}
	if desired > p.acfg.Max {
		desired = p.acfg.Max
	}
	p.metrics.AutoscaleActive(p.agentID, total)
	p.metrics.AutoscaleDesired(p.agentID, desired)
	p.metrics.AutoscaleCurrent(p.agentID, cur)
}

// runPolicy ticks every pollEvery until ctx is cancelled.
func (p *PoolManager) runPolicy(ctx context.Context) {
	t := time.NewTicker(time.Duration(p.pollEvery))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.tick(ctx)
		}
	}
}
```

Note: `tick` calls `p.metrics.Autoscale*` and `p.metrics.AutoscaleEvent` — `*obs.ControlMetrics` helpers are nil-safe (Task 3), so a nil `metrics` in unit tests is fine.

- [ ] **Step 4: Run tests to verify they pass**

Run: `gofmt -w controlplane/poolmanager.go controlplane/poolmanager_test.go && go test ./controlplane/ -run 'TestTick|PoolManager|DecideStep' -v -race`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add controlplane/poolmanager.go controlplane/poolmanager_test.go
git commit -m "feat(controlplane): PoolManager tick + policy loop (one step/tick, reap, gauges)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 7: Registry — delegate to PoolManager for autoscaled agents

**Files:**
- Modify: `controlplane/registry.go`
- Test: `controlplane/registry_test.go`

Registry gains a `pools map[string]*PoolManager`, builds one per autoscaled agent in `NewRegistry`, and delegates `Replicas`/`Replica`/`NextReplica`/`Get` to it. `SetBroker`/`SetGateway` must also stamp each PoolManager's `base` so grown replicas inherit them. Static agents are unchanged.

- [ ] **Step 1: Write the failing test**

Add to `controlplane/registry_test.go`:

```go
func TestRegistryDelegatesAutoscaledAgent(t *testing.T) {
	cfg := &config.Config{Agents: []config.AgentConfig{
		{ID: "as", Name: "AS", Model: "m", ListenAddr: "127.0.0.1:9300",
			Autoscale: &config.AutoscaleConfig{Min: 1, Max: 3, TargetSessionsPerReplica: 2}},
		{ID: "st", Name: "ST", Model: "m", ListenAddr: "127.0.0.1:9400", Replicas: 2},
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(cfg, "/bin/true", "dsn")

	// Autoscaled agent has a PoolManager and starts at 0 live replicas (Start
	// grows to min later); static agent keeps the slice path with 2 replicas.
	if reg.pools["as"] == nil {
		t.Fatal("expected PoolManager for autoscaled agent")
	}
	if reg.pools["st"] != nil {
		t.Fatal("static agent must not have a PoolManager")
	}
	st, ok := reg.Replicas("st")
	if !ok || len(st) != 2 {
		t.Fatalf("static replicas = %d, ok=%v; want 2", len(st), ok)
	}

	// Grow the autoscaled pool through its PoolManager, then confirm the Registry
	// read methods delegate (see it).
	pm := reg.pools["as"]
	pm.startReplica = func(ctx context.Context, ap AgentProcess) (context.CancelFunc, error) {
		_, c := context.WithCancel(ctx)
		return c, nil
	}
	if err := pm.grow(context.Background()); err != nil {
		t.Fatal(err)
	}
	reps, ok := reg.Replicas("as")
	if !ok || len(reps) != 1 || reps[0].DBOSVMID != "as#0" {
		t.Fatalf("delegated Replicas wrong: %+v ok=%v", reps, ok)
	}
}

func TestRegistrySetBrokerStampsPool(t *testing.T) {
	cfg := &config.Config{Agents: []config.AgentConfig{
		{ID: "as", Name: "AS", Model: "m", ListenAddr: "127.0.0.1:9300",
			Autoscale: &config.AutoscaleConfig{Min: 1, Max: 2, TargetSessionsPerReplica: 2}},
	}}
	_ = cfg.Validate()
	reg := NewRegistry(cfg, "/bin/true", "dsn")
	reg.SetBroker(stubBroker{})
	pm := reg.pools["as"]
	if pm.base.broker == nil {
		t.Fatal("SetBroker did not stamp the PoolManager base")
	}
}
```

If `stubBroker` doesn't exist in the test file, add a minimal one:

```go
type stubBroker struct{}

func (stubBroker) SecretsFor(context.Context, string) (map[string]string, error) { return nil, nil }
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./controlplane/ -run 'RegistryDelegates|RegistrySetBrokerStamps' -v`
Expected: FAIL (compile error: `reg.pools` undefined).

- [ ] **Step 3: Add `pools` to the Registry and build them in `NewRegistry`**

In `controlplane/registry.go`, add to the struct (after `broker`, line 27):

```go
	pools map[string]*PoolManager // id -> manager (autoscaled agents only)
```

In `NewRegistry`, initialize the map (in the `&Registry{...}` literal, line 35):

```go
		pools: map[string]*PoolManager{},
```

Inside the `for _, a := range cfg.Agents` loop, after the remote-agent `continue` block and before the local replica expansion (registry.go:59-60), add the autoscale branch. The cleanest spot is right after `r.rr[a.ID] = &atomic.Uint64{}` and the `base` build, replacing the local-expansion block with a branch:

```go
		// Autoscaled local agent: a PoolManager owns the mutable set; the static
		// slice stays empty (reads delegate). Capture the config value for addrOf.
		if a.Autoscale != nil {
			ac := a // capture for the addrOf closure
			pm := newPoolManager(a.ID, base, *a.Autoscale,
				func(i int) (string, error) { return ac.ReplicaAddr(i) }, nil, nil)
			r.pools[a.ID] = pm
			r.sets[a.ID] = nil // delegated; no static slice
			continue
		}
```

Place this immediately before the existing `addrs, err := a.ReplicaAddrs()` static-expansion block (registry.go:63). Leave that static block unchanged for non-autoscaled local agents.

Note: `newPoolManager` here passes `nil, nil` for store+metrics; Task 8 (main.go) injects the real store and metrics after construction via small setters (added next step). The broker is stamped via `SetBroker` (Step 5).

- [ ] **Step 4: Add PoolManager dependency setters used by main.go**

In `controlplane/poolmanager.go`, add setters (Start in Task 8 calls these):

```go
// SetDeps injects the store, metrics, and readiness probe before Start. Must be
// called before runPolicy/grow start spawning real replicas.
func (p *PoolManager) SetDeps(st store.Store, m *obs.ControlMetrics, readyWait func(ctx context.Context, addr string) error) {
	p.st = st
	p.metrics = m
	p.readyWait = readyWait
}
```

- [ ] **Step 5: Delegate the read methods + stamp setters**

In `controlplane/registry.go`, update `Get`, `Replicas`, `Replica`, `NextReplica` to delegate when a PoolManager exists. Replace each method body's lookup:

`Get` (registry.go:121):

```go
func (r *Registry) Get(id string) (AgentProcess, bool) {
	if pm, ok := r.pools[id]; ok {
		ap, ok := pm.Replica(0)
		if !ok {
			// Pool not yet grown to min (pre-Start) — synthesize replica 0 for
			// callers that only need agent-level info (tenant, broker).
			ap, aok := pm.replica0Info()
			return r.withBroker(ap), aok
		}
		return r.withBroker(ap), true
	}
	set, ok := r.sets[id]
	if !ok || len(set) == 0 {
		return AgentProcess{}, false
	}
	return r.withBroker(set[0]), true
}
```

`Replicas` (registry.go:130):

```go
func (r *Registry) Replicas(id string) ([]AgentProcess, bool) {
	if pm, ok := r.pools[id]; ok {
		reps := pm.Replicas()
		out := make([]AgentProcess, len(reps))
		for i := range reps {
			out[i] = r.withBroker(reps[i])
		}
		return out, true
	}
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
```

`Replica` (registry.go:144):

```go
func (r *Registry) Replica(id string, i int) (AgentProcess, bool) {
	if pm, ok := r.pools[id]; ok {
		ap, ok := pm.Replica(i)
		if !ok {
			return AgentProcess{}, false
		}
		return r.withBroker(ap), true
	}
	set, ok := r.sets[id]
	if !ok || i < 0 || i >= len(set) {
		return AgentProcess{}, false
	}
	return r.withBroker(set[i]), true
}
```

`NextReplica` (registry.go:154):

```go
func (r *Registry) NextReplica(id string) int {
	if pm, ok := r.pools[id]; ok {
		return pm.NextReplica()
	}
	set, ok := r.sets[id]
	if !ok || len(set) == 0 {
		return 0
	}
	n := r.rr[id].Add(1) - 1
	return int(n % uint64(len(set)))
}
```

Add `replica0Info` to `poolmanager.go` (agent-level info before the pool is grown — used by `Get` and `GET /agents` for a pre-Start or fully-drained pool):

```go
// replica0Info returns the AgentProcess for index 0 derived from the base
// template even when no replica is live yet (pre-Start). Used for agent-level
// info (tenant/broker) and a health dial target. ok=false only if addr derive
// fails.
func (p *PoolManager) replica0Info() (AgentProcess, bool) {
	if ap, ok := p.Replica(0); ok {
		return ap, true
	}
	ap, err := p.replicaProcess(0)
	if err != nil {
		return AgentProcess{}, false
	}
	return ap, true
}
```

Update `SetBroker` (registry.go:85) and `SetGateway` (registry.go:90) to also stamp pools:

```go
func (r *Registry) SetBroker(b SecretBroker) {
	r.broker = b
	for _, pm := range r.pools {
		pm.base.broker = b
	}
}
```

```go
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
	for _, pm := range r.pools {
		if pm.base.GatewayOn {
			pm.base.GatewayURL = url
			pm.base.GatewayKey = keys[pm.base.Tenant]
		}
	}
}
```

(`withBroker` already attaches `r.broker` on read, but stamping `pm.base.broker` too keeps a grown replica's `SpawnFunc` env correct, since grow builds from `base` — and the broker must ride the spawn, not just reads.)

- [ ] **Step 6: Add a `Pools()` accessor for main.go**

main.go (Task 8) needs to iterate PoolManagers to Start them. Add to `registry.go`:

```go
// Pools returns the autoscaled agents' managers, keyed by id. main.go starts
// each (initial min replicas + policy loop).
func (r *Registry) Pools() map[string]*PoolManager { return r.pools }
```

- [ ] **Step 7: Run tests to verify they pass**

Run: `gofmt -w controlplane/registry.go controlplane/poolmanager.go controlplane/registry_test.go && go test ./controlplane/ -v -race`
Expected: PASS (all PoolManager + registry + A1 routing tests). Then `go build ./...`.

- [ ] **Step 8: Commit**

```bash
git add controlplane/registry.go controlplane/poolmanager.go controlplane/registry_test.go
git commit -m "feat(controlplane): Registry delegates reads to PoolManager for autoscaled agents

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 8: main.go — start PoolManagers (initial min + policy loop)

**Files:**
- Modify: `cmd/runtimed/main.go`
- Test: covered by Task 9 integration (main.go wiring is not unit-tested in this codebase; the boot loop is exercised end-to-end).

The boot loop (main.go:282-308) starts a Supervisor per replica for every agent. A2 branches: an autoscaled agent's replicas are owned by its PoolManager, so main.go calls `pm.Start(ctx)` (grow to min through the same sequential readiness gate) + launches `pm.runPolicy(ctx)` instead of the per-replica Supervisor loop.

- [ ] **Step 1: Add `PoolManager.Start`**

In `controlplane/poolmanager.go`, add:

```go
// Start brings the pool from 0 to min live replicas, growing sequentially so
// the first replica creates the DBOS schema before the rest launch (M2's
// first-run schema-init serialization), then launches the policy loop. Deps
// (store, metrics, readyWait) must be set first (SetDeps). Returns an error if
// the very first replica cannot start (a pool that can't reach min is a boot
// failure worth surfacing); subsequent grow failures are logged by the caller.
func (p *PoolManager) Start(ctx context.Context) error {
	for i := 0; i < p.acfg.Min; i++ {
		if err := p.grow(ctx); err != nil {
			if i == 0 {
				return err
			}
			break // partial pool; the policy loop will retry grow up to min.
		}
	}
	go p.runPolicy(ctx)
	return nil
}
```

- [ ] **Step 2: Wire the test-knob env overrides (so Task 9 can use short cooldowns)**

Add to `controlplane/poolmanager.go` a method main.go calls after `SetDeps`:

```go
// ApplyTuning overrides poll interval and cooldowns from operator/test env
// (nanos via seconds floats). Zero/absent ⇒ keep defaults. Called before Start.
func (p *PoolManager) ApplyTuning(pollSec, upCDSec, downCDSec float64) {
	if pollSec > 0 {
		p.pollEvery = int64(pollSec * 1e9)
	}
	if upCDSec > 0 {
		p.upCD = int64(upCDSec * 1e9)
	}
	if downCDSec > 0 {
		p.downCD = int64(downCDSec * 1e9)
	}
}
```

- [ ] **Step 3: Branch the boot loop in main.go**

In `cmd/runtimed/main.go`, the boot loop is at lines 282-308. Replace the inner body so an autoscaled agent is handled by its PoolManager. Insert BEFORE the `for _, info := range reg.List()` loop a one-time setup of pool deps, then handle pools first:

```go
	// Autoscaled agents (Spine A2): each PoolManager owns its replicas + policy
	// loop. Start them with the same readiness gate that serializes DBOS schema
	// init, then launch the policy goroutine. Tuning via env (test/operator).
	pollSec := envFloatOr("RUNTIME_AUTOSCALE_POLL_SECONDS", 0)
	upCDSec := envFloatOr("RUNTIME_AUTOSCALE_UP_COOLDOWN_SECONDS", 0)
	downCDSec := envFloatOr("RUNTIME_AUTOSCALE_DOWN_COOLDOWN_SECONDS", 0)
	for id, pm := range reg.Pools() {
		pm.SetDeps(ctlStore, cm, func(ctx context.Context, addr string) error {
			return waitAgentHealthy(ctx, addr, 30*time.Second)
		})
		pm.ApplyTuning(pollSec, upCDSec, downCDSec)
		if err := pm.Start(ctx); err != nil {
			slog.Error("autoscaled agent failed to reach min replicas", "agent", id, "err", err)
			os.Exit(1)
		}
		slog.Info("autoscaling agent", "agent", id)
	}
```

Then in the existing `for _, info := range reg.List()` loop, skip agents that have a PoolManager (their replicas are pool-owned):

```go
	for _, info := range reg.List() {
		if _, isPool := reg.Pools()[info.ID]; isPool {
			continue // autoscaled: started above via its PoolManager.
		}
		replicas, _ := reg.Replicas(info.ID)
		for _, ap := range replicas {
			// ... existing remote + static-supervisor body unchanged ...
		}
	}
```

Confirm `envFloatOr` exists in main.go (it does — used for `RUNTIME_GATEWAY_SEARCH_FLOOR` at line 106). Confirm `waitAgentHealthy(ctx, addr, dur)` signature (used at line 304).

- [ ] **Step 4: Build + vet**

Run: `gofmt -w cmd/runtimed/main.go controlplane/poolmanager.go && go build ./... && go vet ./...`
Expected: clean.

- [ ] **Step 5: Run the full hermetic suite**

Run: `go test ./...`
Expected: PASS (no integration tag; this confirms nothing else broke).

- [ ] **Step 6: Commit**

```bash
git add cmd/runtimed/main.go controlplane/poolmanager.go
git commit -m "feat(runtimed): start PoolManagers (min replicas + policy loop) with tuning env

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 9: Integration test — distribution, drain, straggler, un-drain, single-writer, back-compat

**Files:**
- Create: `test/autoscale_test.go`
- Reference (read for helper patterns): `test/replica_pools_test.go`, `test/resume_test.go` (defines `dsn`, `mustExec`, `waitHealthy`), `test/multiagent_test.go` (defines `invokeOn`)

This is the milestone gate. It builds runtimed + agentd, runs an autoscaled agent (`min:1,max:3,target:2`) with short cooldowns via the Task 8 env knobs, and asserts the spec's six gates. The test package is **`package test`** (verified against `test/replica_pools_test.go`), under `//go:build integration`. Reuse the shared helpers (`dsn`, `mustExec`, `invokeOn`, `streamURL`) — do NOT redefine them. Add only autoscale-specific helpers, prefixed `as`. The scripted model string is **`test/scripted`** and the scripted kind needs **no `kind:` field** (model `test/scripted` selects it — verified: A1's test config is `{model: test/scripted, ...}` with no `kind:`).

- [ ] **Step 1: Write the integration test**

Create `test/autoscale_test.go`. Model the harness setup on `test/replica_pools_test.go` (binary build, config write, env, DB cleanup). The config:

```go
//go:build integration

package test

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestAutoscaleGrowDrain(t *testing.T) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatalf("ping postgres (is it running at %s?): %v", dsn, err)
	}
	mustExec(t, db, `DROP TABLE IF EXISTS session_events, sessions, agents CASCADE`)
	mustExec(t, db, `DROP SCHEMA IF EXISTS dbos CASCADE`)

	cfg := `
agents:
  - id: pool
    name: Pool
    model: test/scripted
    listen_addr: 127.0.0.1:8710
    autoscale: {min: 1, max: 3, target_sessions_per_replica: 2}
  - id: fixed
    name: Fixed
    model: test/scripted
    listen_addr: 127.0.0.1:8720
    replicas: 2
`
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "runtime.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	// Build runtimed + agentd into dir (mirror replica_pools_test.go's builder).
	runtimed := asBuild(t, dir, "runtimed", "./cmd/runtimed")
	agentd := asBuild(t, dir, "agentd", "./cmd/agentd")

	cmd := exec.Command(runtimed)
	cmd.Env = append(os.Environ(),
		"RUNTIME_PG_DSN="+dsn,
		"RUNTIME_CTL_ADDR=127.0.0.1:8700",
		"RUNTIME_AGENTD_BIN="+agentd,
		"RUNTIME_CONFIG="+cfgPath,
		// Short, deterministic policy cadence for the test.
		"RUNTIME_AUTOSCALE_POLL_SECONDS=0.3",
		"RUNTIME_AUTOSCALE_UP_COOLDOWN_SECONDS=0.3",
		"RUNTIME_AUTOSCALE_DOWN_COOLDOWN_SECONDS=0.3",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()
	base := "http://127.0.0.1:8700"
	asWaitHealthy(t, base)
```

Then the gates (continue the same function):

```go
	// Gate 1 — Scale-up: drive enough concurrent sessions to exceed target=2 so
	// desired climbs toward max=3. Each scripted session completes quickly, so to
	// hold concurrency we open several in a tight burst and assert the replica
	// column eventually spreads across 0,1,2.
	for i := 0; i < 9; i++ {
		_, _ = invokeOn(t, base, "pool")
	}
	if !asEventually(t, 8*time.Second, func() bool {
		return asDistinctReplicas(t, db, "pool") >= 3
	}) {
		t.Fatalf("pool never scaled to 3 replicas; replicas seen=%d", asDistinctReplicas(t, db, "pool"))
	}

	// Gate 6 — Back-compat: the static agent used exactly its 2 configured
	// replicas (never more), proving no PoolManager / autoscale touched it.
	for i := 0; i < 6; i++ {
		_, _ = invokeOn(t, base, "fixed")
	}
	if got := asDistinctReplicas(t, db, "fixed"); got != 2 {
		t.Fatalf("static agent used %d replica indices, want exactly 2", got)
	}

	// Gate 5 — single-writer / no double execution: every session's turn_count is
	// consistent (the scripted model runs a bounded number of turns); no session
	// row shows an impossible count from two executors racing the same workflow.
	var maxTurns int
	if err := db.QueryRow(`SELECT COALESCE(MAX(turn_count),0) FROM sessions WHERE agent_id='pool'`).Scan(&maxTurns); err != nil {
		t.Fatal(err)
	}
	if maxTurns > 4 {
		t.Fatalf("max turn_count=%d implausibly high — possible double execution", maxTurns)
	}

	// Gate 2 — Drain + reap: stop creating new sessions; once all pool sessions
	// are terminal, the policy loop drains the top replicas and reaps them down
	// toward min=1. Assert replicas_current drops (observed via /metrics).
	if !asEventually(t, 12*time.Second, func() bool {
		return asMetricGauge(t, base, `runtime_agent_replicas_current{agent="pool"}`) <= 1
	}) {
		t.Fatalf("pool never drained back to 1; current=%v",
			asMetricGauge(t, base, `runtime_agent_replicas_current{agent="pool"}`))
	}
}
```

Add the autoscale-specific helpers at the bottom of the file:

```go
func asBuild(t *testing.T, dir, name, pkg string) string {
	t.Helper()
	out := dir + "/" + name
	cmd := exec.Command("go", "build", "-o", out, pkg)
	cmd.Dir = ".."
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build %s: %v", name, err)
	}
	return out
}

func asWaitHealthy(t *testing.T, base string) {
	t.Helper()
	if !asEventually(t, 20*time.Second, func() bool {
		resp, err := http.Get(base + "/healthz")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == 200
	}) {
		t.Fatal("control plane never became healthy")
	}
}

func asEventually(t *testing.T, d time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(150 * time.Millisecond)
	}
	return false
}

func asDistinctReplicas(t *testing.T, db *sql.DB, agent string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT count(DISTINCT replica) FROM sessions WHERE agent_id=$1`, agent).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// asMetricGauge fetches /metrics and returns the float value of the first line
// exactly matching series (e.g. `runtime_agent_replicas_current{agent="pool"}`).
// Returns -1 if absent. Self-contained (stdlib only); does not depend on shared
// scrape helpers.
func asMetricGauge(t *testing.T, base, series string) float64 {
	t.Helper()
	resp, err := http.Get(base + "/metrics")
	if err != nil {
		return -1
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return -1
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, series+" ") {
			v, perr := strconv.ParseFloat(strings.TrimSpace(line[len(series)+1:]), 64)
			if perr != nil {
				return -1
			}
			return v
		}
	}
	return -1
}
```

These helpers use `net/http`, `io`, `strings`, and `strconv` — add `"io"`, `"net/http"`, `"strconv"`, `"strings"` to the test file's import block (alongside the imports listed in Step 1).

- [ ] **Step 2: Verify the test compiles under the integration tag**

Run: `go vet -tags integration ./test/`
Expected: clean (or only reveals genuinely missing helpers to add).

- [ ] **Step 3: Run the new integration test**

Run: `go test -tags integration ./test/ -run TestAutoscaleGrowDrain -v -timeout 180s`
Expected: PASS. If scale-up is flaky because scripted sessions complete before concurrency builds, raise the burst count or lower `target` via config — but prefer asserting on the `replica` column spread (durable evidence) over instantaneous gauges.

- [ ] **Step 4: Run the full integration suite (no regression)**

Run: `go test -tags integration ./test/ -timeout 600s`
Expected: PASS (all A1 + prior milestone integration tests still green, including `TestReplicaPoolsAffinity`).

- [ ] **Step 5: Commit**

```bash
git add test/autoscale_test.go
git commit -m "test(integration): autoscale grow/drain/reap + back-compat + single-writer

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 10: Docs — README, ROADMAP, runtime.yaml example

**Files:**
- Modify: `README.md`, `ROADMAP.md`, `runtime.yaml`

- [ ] **Step 1: Add a runtime.yaml example block**

In `runtime.yaml`, add a commented autoscale example near the replica-pool example:

```yaml
  # Autoscaling (Spine A2): float the pool between min and max by active-session
  # load. Opt-in; when omitted the agent uses a static `replicas:` pool. Rejected
  # on remote (url:) agents. listen_addr is the base; replica i listens on
  # base_port+i, and the whole max range is reserved at load.
  # - id: support
  #   name: Support
  #   model: claude-opus-4-8
  #   listen_addr: 127.0.0.1:8101
  #   autoscale:
  #     min: 1
  #     max: 4
  #     target_sessions_per_replica: 5
```

- [ ] **Step 2: Add a README subsection**

In `README.md`, in the replica-pools / scaling section, add an "Autoscaling (A2)" subsection covering: the `autoscale` block, that runtimed is controller+actuator (local), the active-sessions-per-replica signal, drain-only scale-down (a long-lived session blocks one scale-down — by design, durability), suffix-only mutation, the metrics (`runtime_agent_replicas_{desired,current}`, `runtime_agent_active_sessions`, `runtime_autoscale_events_total`), the tuning env vars (`RUNTIME_AUTOSCALE_{POLL,UP_COOLDOWN,DOWN_COOLDOWN}_SECONDS`), and that static `replicas:` is unchanged when `autoscale` is absent.

- [ ] **Step 3: Update ROADMAP**

In `ROADMAP.md`: change the checkpoint date line to note A2; mark spine item **2. Autoscaling** as DONE (2026-06-13) with a paragraph mirroring A1's entry — the design (runtimed local grow/drain, active-sessions signal, drain-only, PoolManager, suffix-only + un-drain), what it unblocks (the K8s direction / C2 M2 now has the decision/actuator split as its signal-only seam), what's deferred (force-kill deadline, richer signals, per-agent cooldown config, signal-only mode), and the spec/plan paths. Note that spine items 4 (`session_events` concurrency) remains safe under A2 by construction (suffix-only + drain-only keep one writer per session).

- [ ] **Step 4: Verify build + full hermetic suite once more**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: clean + PASS.

- [ ] **Step 5: Commit**

```bash
git add README.md ROADMAP.md runtime.yaml
git commit -m "docs: Spine A2 autoscaling — README + ROADMAP + runtime.yaml example

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Final verification (after all tasks)

Before finishing the branch:

- [ ] `gofmt -l .` reports no files (all formatted).
- [ ] `go build ./... && go vet ./...` clean.
- [ ] `go test ./...` PASS (hermetic).
- [ ] `go test -tags integration ./test/ -timeout 600s` PASS (full integration, incl. A1's `TestReplicaPoolsAffinity` and the new `TestAutoscaleGrowDrain`).
- [ ] Spec coverage: every Section-1–6 requirement maps to a task (config/store/obs/poolmanager/registry/main/test/docs).

Then hand off to **superpowers:finishing-a-development-branch** to merge to master.
