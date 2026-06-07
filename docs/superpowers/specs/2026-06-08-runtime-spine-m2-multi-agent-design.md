# Runtime Spine — Milestone 2: Multi-Agent Platform — Design Spec

**Date:** 2026-06-08
**Status:** Approved (design phase)
**Builds on:** Milestone 1 (durable walking skeleton) — `2026-06-07-runtime-spine-design.md` §11

---

## 1. Goal

Turn the single-static-agent skeleton into a real **multi-agent platform**: the
control plane hosts many agents, each as its own supervised subprocess, routed
by path prefix, with a full operator CLI and a session-management API that
reports real session state. This is breadth (many agents), not depth (pools,
scaling, console) — those stay in later milestones.

Per the M1 spec §11, Milestone 2 = "registry, multi-agent deploy/list/stop,
subprocess pools, session management across agents, supervisor lifecycle, full
CLI." We scope **pools to a single subprocess per agent** (round-robin pools
deferred) and keep the **web console in M3**, per design decisions below.

## 2. Design decisions (locked)

| Decision | Choice | Rationale |
|---|---|---|
| Agent registration | **Config-file driven** (`runtime.yaml`) | Matches single-binary/Compose ethos; deterministic; deploy = edit + reload. Dynamic POST /agents deferred. |
| Routing | **Path prefix `/agents/{id}/...`** | RESTful; M1 proxy already flagged this as the M2 evolution; CLI uses `--agent`. |
| Pool model | **One subprocess per agent** | M2 = multi-agent breadth. Many concurrent sessions per subprocess (M1's session model). Pools >1 deferred. |
| Console | **CLI + API only** | Console is M3 (operability layer) per spec §11. |
| Session status | **Tracked** (created→running→completed/error, turn_count) | Fixes M1 debt; needed for a useful session API. |
| `workflow_id` column | **Populated** (= session id) | Fixes M1 documented-invariant gap. |
| Cross-agent session listing | **Yes** (`ListSessions(agentID)` + API) | Core M2 "session management across agents". |
| AppendEvent concurrency | **Deferred** | Still one writer per session in M2 (one subprocess, serial publish). Revisit when pools land. |
| Dynamic API deploy, pools, autoscaling, console | **Deferred** | Later milestones. |

## 3. Architecture (delta from M1)

```
  runtimectl --agent <id> ──HTTP──▶ runtimed (control plane)
                                       │ Registry (from runtime.yaml)
                                       │ Router: /agents/{id}/* → that agent's proxy
                                       │ Supervisor per agent (restart-on-crash)
                                       ▼
                          agentd[α]   agentd[β]   agentd[γ]   ... (one per agent)
                              │            │           │
                              └────────────┴───────────┘
                                       ▼
                                   Postgres (shared)
                                   • DBOS checkpoints (per workflow)
                                   • sessions (agent_id, status, turn_count, workflow_id)
                                   • session_events
```

The agent subprocess (`agentd`) and `agentruntime.Serve` are **largely
unchanged** from M1 — each agent is still an independent M1-style durable
agent. M2's new work is in the **control plane** (registry, router,
multi-supervisor) and **store/CLI** surfaces, plus **status tracking** inside
the agent's workflow.

## 4. Components

### 4.1 Agent config (`runtime.yaml`)
A YAML file listing agents. Loaded by `runtimed` at boot.

```yaml
agents:
  - id: support
    name: Support Agent
    model: test/scripted        # M2 keeps the deterministic test agent wireable
    listen_addr: 127.0.0.1:8101
  - id: research
    name: Research Agent
    model: test/scripted
    listen_addr: 127.0.0.1:8102
```

- Each agent gets a distinct `listen_addr` (M2: operator assigns; no dynamic
  port allocation). `id` is unique; duplicates are a load error.
- A new package `internal/config` parses + validates this (required fields,
  unique ids, non-empty addrs). Validation errors abort `runtimed` startup with
  a clear message.
- Default config path `runtime.yaml`, overridable via `RUNTIME_CONFIG` env.

### 4.2 Registry
An in-memory registry (`controlplane.Registry`) built from the parsed config:
`id → AgentProcess`. Exposes `List() []AgentInfo`, `Get(id) (AgentProcess, bool)`.
M2 is read-only after boot (config-driven); the type is shaped so a future
dynamic-deploy milestone can add/remove entries.

### 4.3 Multi-supervisor
`runtimed` starts ONE `Supervisor` (the existing M1 type, unchanged) per agent,
each driving that agent's `AgentProcess.SpawnFunc()`. Each agentd gets its own
`RUNTIME_AGENT_ID` and `RUNTIME_LISTEN_ADDR` from the registry. All supervisors
share the run context; cancelling it stops all.

### 4.4 Router (control API)
`controlplane.NewAPI(registry)` returns a mux that:
- Routes `/agents/{id}/...` → strips the `/agents/{id}` prefix → reverse-proxies
  to that agent's `listen_addr` (reusing M1's `reverseProxy`, FlushInterval=-1
  for SSE). Unknown `{id}` → 404.
- `GET /agents` → JSON list of registered agents (id, name, model, status).
- `GET /healthz` → control-plane liveness (200).

The per-agent contract (`POST /sessions`, `GET /sessions/{id}/stream`, etc.)
is served verbatim under the prefix, exactly as M1 — the proxy is transparent.

### 4.5 Store additions
- `SessionRow` gains nothing new (already has AgentID, Status, TurnCount,
  WorkflowID fields).
- **Populate `workflow_id`**: change `CreateSession` call so the row stores the
  session id as workflow_id (they are equal by construction). Cleanest: a new
  signature `CreateSession(ctx, agentID) (id string, err error)` that sets
  `workflow_id = id` internally. (Drop the always-`""` second param.)
- **`SetSessionStatus`** + a new **`IncrementTurn(ctx, id)`** (or
  `SetSessionStatus` plus a `turn_count` update) — wired into the workflow.
- **`ListSessions(ctx, agentID) ([]SessionRow, error)`** — ordered by
  created_at desc; backs the session-listing API.
- Both `memStore` and `pgStore` implement all of the above; store contract
  tests cover them against `memStore`.

### 4.6 Session status tracking (in the agent workflow)
In `agentruntime.sessionWorkflow`:
- On entry (first turn): `SetSessionStatus(id, "running")`.
- After each applied turn: increment turn_count.
- On clean finish: `SetSessionStatus(id, "completed")`; on error/abort:
  `"error"`. (Status writes are best-effort, logged on failure — they are
  operational metadata, not the durability backbone.)
- These run in the deterministic workflow body (like `publish`), so they re-run
  on replay — idempotent for status (last-write-wins), and turn_count uses the
  applied-entries count so it converges to the same value. Document this.

### 4.7 CLI (`runtimectl`) — full operator loop
- `runtimectl agents` → list agents (from `GET /agents`).
- `runtimectl invoke --agent <id> "<msg>"` → start a session on that agent + stream.
- `runtimectl logs --agent <id> <session-id>` → replay/stream a session.
- `runtimectl sessions --agent <id>` → list sessions for an agent (from
  `GET /agents/{id}/sessions`).
- `--agent` defaults to the sole agent if exactly one is registered (ergonomics);
  required when >1.
- Still stdlib-only (no cobra). `RUNTIME_CTL_URL` unchanged.

### 4.8 Session-listing API
`GET /agents/{id}/sessions` on the AGENT contract (served by `agentruntime`,
proxied through), returning `[]{id, status, turn_count, created_at}` from
`ListSessions`. Routed transparently like the rest.

## 5. What stays unchanged from M1
- The durable DBOS workflow mechanics (turn=step, headless RunTurn, resume).
- The agent HTTP/SSE contract endpoints (now multiplied across agents + the new
  `/sessions` listing).
- `Supervisor`, `reverseProxy`, the `agentd` binary shape, the test agent.
- The flagship resume guarantee (M2 must not regress it).

## 6. Error handling
- **Config errors** (missing fields, dup ids, dup addrs) → `runtimed` exits
  non-zero with a clear message before starting anything.
- **Unknown agent id** in a request → 404 from the router.
- **One agent crashing** → only its supervisor restarts it; other agents
  unaffected (independent subprocesses + supervisors).
- **Status-write failures** → logged, non-fatal (operational metadata).
- **Port conflict** (two agents same addr) → caught at config validation.

## 7. Testing strategy
- **Hermetic unit tests**: config parse/validate (valid, missing fields, dup
  ids, dup addrs); registry List/Get; router dispatch (`/agents/{id}/...` →
  correct backend, unknown → 404) using `httptest` backends; store additions
  (ListSessions, status, turn_count, workflow_id populated) against memStore.
- **Integration test (gated `integration`)**: a **two-agent** end-to-end —
  load a 2-agent config, start runtimed, invoke a session on each agent through
  the router, assert each completes and `GET /agents/{id}/sessions` lists its
  own session (and not the other's). Reuses the deterministic test agent +
  real Postgres.
- **Regression**: the M1 flagship resume test must still pass unchanged
  (durability not regressed).

## 8. Scope boundaries (M2 does NOT do)
Deferred to later milestones: subprocess pools/replicas >1, autoscaling,
dynamic API-driven deploy/rollback, the web console (M3), token auth/RBAC
(M3/Identity), AppendEvent concurrency hardening, dynamic port allocation.

## 9. Internal milestones (sequencing hint for planning)
1. **Store + config foundations** — config package, store additions
   (workflow_id, ListSessions, status/turn helpers), all hermetic.
2. **Control plane multi-agent** — registry, multi-supervisor, path-prefix
   router; runtimed loads config and hosts N agents.
3. **Status tracking + session API + CLI** — wire status/turn_count into the
   workflow, add the session-listing endpoint, expand the CLI.
4. **Two-agent integration test** + regression of the M1 resume test.

## Appendix — key signatures to evolve
- `store.CreateSession(ctx, agentID) (string, error)` (drop workflowID param;
  set workflow_id = id internally). Update the one caller in
  `agentruntime/serve.go`.
- `store.ListSessions(ctx, agentID) ([]SessionRow, error)` (new).
- `controlplane.NewAPI(reg *Registry) *http.ServeMux` (was `NewAPI(agentAddr)`).
- `controlplane.Registry` (new) from `internal/config`.
- `runtimed` main: parse config → build registry → start a supervisor per
  agent → serve router.
