# Spine A1 — Replica Pools + Session Affinity: Design Spec

**Date:** 2026-06-13
**Status:** Approved (brainstorm complete)
**Sub-project:** Spine (sub-project 1 of 6), hardening item A1

---

## Section 1 — Goal, Scope & the Executor-ID Invariant

**Goal:** Let each *local* agent run `replicas: N` supervised `agentd` processes
behind the one `/agents/{id}` route, with new sessions load-balanced across the
replicas and every session-scoped request pinned to the replica that owns it.

This is the keystone spine-hardening milestone: it unblocks **A2 autoscaling**
(scale a pool by load) and **C2 M2 per-agent-pod scheduling** (a scheduled pod is
one replica of a pool).

### The load-bearing invariant — stable per-replica executor IDs

Each `agentd` is a **DBOS executor**. On `Launch()`, a process recovers *only*
the workflows stamped with its own executor ID
(`recoverPendingWorkflows(c, []string{c.executorID})`, dbos.go:721). DBOS's
default executor ID is the literal string `"local"` (dbos.go:123), overridable
via the `DBOS__VMID` env var. Today every agentd runs as `"local"` — which is
exactly why M1's kill-and-resume works: the restarted process shares the ID and
recovers the pending workflow.

For pools, replica *i* of agent `support` launches with a **stable, derived**
`DBOS__VMID = "support#i"`. This guarantees:

- **No double execution.** Two replicas never share an executor ID, so on launch
  no two replicas recover the same workflow. (If all replicas ran as `"local"`,
  every replica would recover *every* session — catastrophic.)
- **Per-replica durability (M1 preserved).** A crashed replica, restarted by its
  supervisor at the **same index**, gets the **same** executor ID and recovers
  exactly *its own* in-flight sessions. (If replicas used random UUIDs, a
  restarted replica would get a new ID and never recover its prior work —
  silently breaking M1.)

### Three things bind a session to one replica

By construction these three always agree:

1. Its **DBOS workflow** — only the owning executor recovers it.
2. Its **in-memory SSE subscriber set** — `publish()` (agentruntime/serve.go)
   fans events to subscribers held in *that process's* memory; a `/stream`
   routed elsewhere replays Postgres history but never sees live events.
3. Its **persisted `replica` index** — the new column this milestone adds.

Session→replica affinity is therefore a **correctness requirement**, not
load-balancing polish.

### In scope (A1)

- `replicas: N` per local agent (config), default 1 = exact back-compat.
- N supervised agentd replicas per agent, each with a stable derived executor ID
  and derived listen address.
- runtimed round-robins **new** sessions across an agent's replicas.
- runtimed pins **session-scoped** requests (`/sessions/{id}`,
  `/sessions/{id}/stream`) to the owning replica, resolved from a persisted
  `replica` column on the session row.
- Per-replica supervision (restart at the same index ⇒ same executor ID).
- Per-replica health aggregation (an agent is healthy if ≥1 replica answers).
- Per-replica metrics fan-out (a `replica` label, aggregable by agent).
- Owner-down behavior: session-scoped requests 503 (degrade, consistent with
  C3); the supervisor restarts the replica which recovers its own work.

### Out of scope (deferred)

- **Autoscaling (A2)** — changing replica count by load.
- **Graceful drain** — stop-new + finish-in-flight on replica shutdown.
- **Dynamic replica count** — changing N without a restart.
- **Skip-known-down round-robin** — new-session routing is a blind atomic
  counter; a POST landing on a down replica's index fails and the client
  retries. Live-health-aware routing needs health state in the router; deferred.
- **Remote-agent replicas** — `replicas:` is rejected on `url:` agents; C3
  remote attach stays single-entry and untouched.
- **Cross-replica session migration / rebalance** — a session lives on its
  birth replica for life.

### Framing

The data plane is already replica-agnostic — runtimed reverse-proxies plain HTTP
to an agent address (controlplane/proxy.go, api.go). What's single-instance today
is (a) the registry (one `AgentProcess` per id), (b) supervision (one supervisor
per id), and (c) the implicit assumption that any request for an agent can go to
the one process. A1 turns each agent into an ordered **replica set** and teaches
the router the difference between *new-session* (round-robin), *session-scoped*
(affinity), and *replica-agnostic* (any replica) requests.

---

## Section 2 — Components & Data Flow

Every in-process span/metric boundary from obs-M1/M2 is preserved; this section
adds replica fan-out around them.

### Config (`internal/config/config.go`)

`AgentConfig` gains:

```go
Replicas int `yaml:"replicas"` // optional; 0/omitted ⇒ 1. Local agents only.
```

For a **local** agent, `listen_addr` is the **base**: replica *i* listens on
`base_host : base_port + i`. So `listen_addr: 127.0.0.1:8101` with `replicas: 3`
derives `127.0.0.1:8101`, `:8102`, `:8103`.

`Validate()` rules:

- `Replicas` defaults to 1 when ≤ 0.
- `Replicas > 1` is rejected on a **remote** agent (`url:` set) — joins the
  existing "spawn-time-only fields rejected on remote" check. (A remote agent's
  replica count is the remote operator's concern.)
- The base `listen_addr` must parse as `host:port` with a **numeric** port (it
  already must be a valid dial address; A1 additionally requires the port be
  parseable as an int because we do `port + i`).
- The **full derived port set** across all local agents must be collision-free.
  Today's `dials` uniqueness map is populated with every derived `host:port`
  (not just the base), so two agents whose ranges overlap (e.g. `:8101` rep 3
  and `:8102` rep 2) are caught at load.
- `replicas: 1` (or omitted) derives exactly one address == today's behavior.

A helper `(a AgentConfig) ReplicaAddrs() ([]string, error)` returns the derived
addresses (used by the registry); it errors on an unparseable base port.

### Registry (`controlplane/registry.go`)

Today `agents map[string]AgentProcess` holds one entry per id. A1 introduces a
**replica set per agent**. The registry stores, per agent, an ordered slice of
`AgentProcess` (one per replica) plus the agent-level info.

`AgentProcess` gains:

```go
ReplicaIndex int    // 0..N-1; 0 for single-replica and remote agents.
DBOSVMID     string // "<AgentID>#<ReplicaIndex>" for local; "" for remote (remote owns its own executor id).
```

`Addr`/`BaseURL` are derived per replica (`base_port + i`). Agent-level fields
(`Tenant`, `GatewayOn/URL/Key`, `Memory`, `broker`, …) are **identical across a
replica set** — replicas are the *same agent*: same tenant, same secrets, same
gateway wiring.

New/changed registry methods:

```go
// Replicas returns the ordered replica set for id (len==1 for single-replica
// and remote agents). Each carries the broker (same copy-on-Get discipline).
func (r *Registry) Replicas(id string) ([]AgentProcess, bool)

// Replica returns one replica by index (for affinity dial). false if id
// unknown or i out of range.
func (r *Registry) Replica(id string, i int) (AgentProcess, bool)

// NextReplica returns the next replica index for a NEW session, round-robin
// via an atomic per-agent counter. Blind to liveness (see deferred).
func (r *Registry) NextReplica(id string) int

// Get returns replica 0 (agent-level info + broker), preserving today's
// callers that want "the agent" rather than a specific replica.
func (r *Registry) Get(id string) (AgentProcess, bool)
```

Remote agents expand to a single-entry replica set (`ReplicaIndex 0`,
`DBOSVMID ""`), so all replica-aware call sites work uniformly and C3 is
untouched.

`SetBroker`/`SetGateway` stamp every replica in every set (they iterate the sets
instead of the flat map).

### Spawn env (`controlplane/proxy.go` `buildEnv`)

Add to the child env:

- `DBOS__VMID=<AgentID>#<ReplicaIndex>` — the stable per-replica executor ID
  (only for local agents; remote agents are never spawned).
- `RUNTIME_AGENT_REPLICA=<ReplicaIndex>` — so agentd knows its own index to
  stamp on sessions.
- `RUNTIME_LISTEN_ADDR` is set to the **replica's derived addr** (already sourced
  from `a.Addr`, which is now per-replica).

Everything else is unchanged and identical across the set.

### agentd (`agentruntime`)

- `Serve` reads `RUNTIME_AGENT_REPLICA` (default 0) into `Manager.replica int`.
- `DBOS__VMID` needs **no agentd code** — DBOS reads it directly from the env at
  `NewDBOSContext`/`Launch`. (Confirmed: dbos.go:114.)
- `startSession` passes `m.replica` to `store.CreateSession`, stamping the owner
  on the row at create time.

### Store (`internal/store`)

- `schema.sql`: `sessions` gains `replica INT NOT NULL DEFAULT 0`. (Additive,
  `IF NOT EXISTS` table create is unchanged; an `ALTER TABLE ... ADD COLUMN IF
  NOT EXISTS replica` migration line covers existing deployments.)
- `SessionRow` gains `Replica int`.
- `CreateSession(ctx, agentID, replica)` — signature gains `replica`; INSERT
  writes it. (All call sites updated; there is exactly one real caller,
  `startSession`, plus tests.)
- New `SessionReplica(ctx, id) (int, error)` — a single indexed read for
  runtimed's affinity lookup; `ErrNoRows` ⇒ a not-found sentinel the router maps
  to 404.
- `GetSession`/`ListSessions` SELECTs add `replica`.
- memstore parity for all of the above.

### Routing (`controlplane/api.go`)

The `/agents/{id}/` handler dispatches by request shape. The handler is given a
`store.Store` (for affinity lookups) in addition to the registry.

- **New session** — `POST` whose rewritten path is exactly `/sessions`:
  `i := reg.NextReplica(id)`; dial `reg.Replica(id, i)`. The accepting replica
  stamps `i` on the row (it already knows `i` from `RUNTIME_AGENT_REPLICA`).
- **Session-scoped** — path matches `/sessions/{sid}` or `/sessions/{sid}/...`:
  `i, err := st.SessionReplica(ctx, sid)`; not-found ⇒ 404; else dial
  `reg.Replica(id, i)`.
- **Replica-agnostic** — `GET /sessions` (list), `/healthz`, `/meta`, and any
  other agent-level path: dial `reg.Replica(id, 0)`. (The list is per-agent in
  Postgres, so any replica returns the same rows — no fan-out needed.)

Path classification reuses the prefix already stripped by the handler
(`/sessions`, `/sessions/{sid}`…). The session id is the path segment after
`/sessions/`.

runtimed obtains a `store.Store` handle (a `pgStore` over the existing `dsn`);
it already calls `sql.Open(dsn)` for identity, so this reuses the same DSN (a
dedicated pool or the shared one — see §3 wiring).

### Data flow (new session, then reconnect)

```
POST /agents/support/sessions
  → router: NextReplica(support)=1 → proxy → support#1 :8102
      → support#1: CreateSession(.., replica=1) → row{replica:1}
      → starts DBOS workflow on executor "support#1"
  ← {session_id: ses-abc}

GET /agents/support/sessions/ses-abc/stream
  → router: SessionReplica(ses-abc)=1 → proxy → support#1 :8102
      → support#1: live SSE from its in-memory subscriber set ✓
```

---

## Section 3 — Supervision, Health, Metrics, Error Behavior

### Supervision (`cmd/runtimed/main.go`)

The boot loop changes from one supervisor per agent to one **per replica**. For
each local agent, iterate `reg.Replicas(id)`:

- Each replica gets its own `Supervisor{Spawn: ap.SpawnFunc(), …}` (the spawn
  closure now carries the replica's `DBOS__VMID` + derived addr).
- Each gets its own readiness wait on the replica's `Addr`.
- The DBOS first-run schema-init serialization (M2 invariant: concurrent
  first-run schema creation is unsafe) is preserved by keeping the **sequential**
  start gate: start replica, wait healthy, start next. The first replica to boot
  creates the schema; the rest find it present.

Remote agents are unchanged: a single `HealthMonitor`, no replicas.

### Health (`GET /agents`, `/agents/{id}/healthz`)

- An agent is reported **healthy** if **at least one replica** answers `/healthz`
  (a pool tolerates a single dead replica). The `GET /agents` status loop fans
  out across `reg.Replicas(id)` and ORs the results.
- A proxied `/agents/{id}/healthz` dials replica 0 (cheap liveness probe); the
  aggregate health view is `GET /agents`.

### Metrics (`internal/obs` fan-out + gauges)

- The scrape-target builder (cmd/runtimed/main.go `mountMetrics`) emits one
  `obs.ScrapeTarget` **per replica** (iterate `reg.Replicas`), each carrying a
  `Replica int` field surfaced as a `replica` Prometheus label alongside the
  existing `agent` label. Series stay per-replica but aggregate by agent in
  Grafana.
- C3's `AgentReachable` and the existing `AgentRestart` counter gain a `replica`
  label (so a flapping replica is attributable). Signature:
  `AgentRestart(agent string, replica int)`, `AgentReachable(agent string, replica int, ok bool)`.

### Owner-down behavior (degrade — consistent with C3)

If a session's owner replica is down, its session-scoped requests proxy to a
dead address and the existing `reverseProxy` `ErrorHandler` returns **503**.
Rerouting is impossible *by design*: only the owner's executor can resume that
workflow (the executor-ID invariant). When the supervisor restarts the replica
at the same index (same `DBOS__VMID`), it recovers the in-flight workflow and the
503s clear. This is the M1 guarantee, now scoped per replica — honest and
correct.

New sessions use a **blind** atomic round-robin counter. A POST that lands on a
down replica's index fails and the client retries (next index, likely live).
Live-health-aware skip-down routing is deferred (needs health state in the
router).

---

## Section 4 — Testing & Live Proof

### Hermetic unit tests

| Area | File | Cases |
|---|---|---|
| Config | `internal/config/config_test.go` | `replicas: N` derives the right port set; `replicas>1` rejected on remote; derived-port collision across agents caught; omitted/1 == one address back-compat; unparseable base port errors |
| Registry | `controlplane/registry_test.go` | `replicas: 3` ⇒ 3 entries, indices 0–2, derived addrs, `DBOSVMID="id#i"`; `NextReplica` round-robins (incl. concurrent atomic); `Replica(id,i)` bounds; remote stays single-entry; `SetBroker`/`SetGateway` stamp all replicas |
| Spawn env | `controlplane/proxy_test.go` | replica env carries `DBOS__VMID=id#i`, `RUNTIME_AGENT_REPLICA=i`, derived `RUNTIME_LISTEN_ADDR`; agent-level vars identical across set |
| Store | `internal/store/store_test.go` | `CreateSession` persists `replica`; `SessionReplica` round-trips; not-found sentinel; `GetSession`/`ListSessions` expose `replica`; memstore parity |
| Routing | `controlplane/router_test.go` (or `api_*_test.go`) | fake multi-replica backend: `POST /sessions` round-robins; `/sessions/{sid}` + `/stream` hit persisted owner; replica-agnostic paths hit replica 0; unknown session ⇒ 404; owner-down ⇒ 503 |

### Integration test (`//go:build integration`, Postgres.app)

`test/replica_pools_test.go` — `TestReplicaPoolsAffinity`: one agent
`replicas: 2`, scripted model. Gates:

1. **Distribution** — create several sessions; both replica indices appear in the
   `replica` column.
2. **Affinity** — each session's follow-up `GET /sessions/{id}` and `/stream`
   reach the owning replica and observe **live** events (not just Postgres
   replay).
3. **Per-replica durability** — kill replica 1 mid-turn: its sessions 503;
   replica 0's sessions keep serving; the supervisor restarts replica 1 (same
   `id#1` executor) which recovers its in-flight workflow and resumes; assert
   replica 0 never recovered replica 1's work (turn counts prove no double
   execution).
4. **Back-compat** — a second sub-case with `replicas: 1` behaves identically to
   a pre-A1 single agent.

Integration tests self-clean their DB + the `dbos` schema, per workspace
convention.

### Live proof (milestone gate)

1. `runtime.yaml` with one agent `replicas: 3`; bring up the stack
   (`go run ./cmd/runtimed`, scripted model — no LLM key needed).
2. Create ~9 sessions via `runtimectl`; show `SELECT id, replica FROM sessions`
   spread across 0/1/2, and logs showing three distinct
   `executor_id=agent#0/1/2`.
3. Reconnect `/stream` on a few in-flight sessions; show each receives live
   events from its owner replica.
4. `kill -9` one replica's PID mid-turn; show its sessions 503 while the others
   serve; show the supervisor respawn with the **same** `DBOS__VMID` recover its
   sessions; 503s clear.
5. Prometheus (`/metrics`) shows three per-replica series under the one agent
   (the `replica` label distinguishes them).
6. Flip to `replicas: 1`; show byte-identical single-process behavior.

### Conventions honored

- The `go` CLI is ground truth (ignore IDE/LSP diagnostics from the
  `replace ../harness` setup).
- Integration tests use Postgres.app at
  `postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable` and
  self-clean (DB rows + `dbos` schema).
- No secrets or content in logs.
- Scripted model (`test/scripted`) so no LLM key is needed.

---

## Open items deferred to later spine milestones

- **A2 autoscaling** — scale replica count by observed load (needs A1's pool +
  the metrics this milestone labels per replica).
- **Graceful drain** — stop-new + finish-in-flight on planned replica shutdown.
- **Dynamic replica count** — change N without a process restart.
- **Skip-known-down round-robin** — liveness-aware new-session routing.
- **C2 M2 per-agent-pod scheduling** — a K8s-scheduled pod is one replica whose
  lifecycle the orchestrator (not runtimed's Supervisor) owns; A1 + C3 together
  make this expressible.
