# Runtime Spine — Design Spec

**Date:** 2026-06-07
**Status:** Approved (design phase)
**Sub-project:** 1 of 6 in the "on-prem AgentCore" platform

---

## 1. Context & Goal

We are building an on-prem equivalent of AWS Bedrock AgentCore: a platform
for hosting, running, and operating LLM agents, using open-source components
where the infrastructure is genuinely hard, and building the differentiating
core ourselves. It must be deployable on-prem (data residency / air-gapped /
compliance) **and** packageable as a product with a clean install and
developer experience.

The full platform decomposes into six sub-projects built bottom-up on a
foundation of **harness** (our owned Go agent library), **Postgres/pgvector**,
and a container runtime:

1. **Runtime** (this spec) — serverless agent hosting + durable/resumable loops
2. **Gateway** — tools / MCP federation
3. **Memory** — managed multi-tenant memory
4. **Identity** — agent identity, secrets, OAuth broker
5. **Sandboxes** — browser tool, code interpreter
6. **Observability** — tracing, metrics, dashboards

Each sub-project gets its own spec → plan → build cycle. **This spec covers
sub-project 1, the Runtime spine** — nothing else in the platform has meaning
until a harness agent can be hosted, isolated, and run durably.

### Guiding decisions (set during brainstorming)

| Decision | Choice | Rationale |
|---|---|---|
| Primary driver | Production platform **and** shippable product | Raises the bar for both operability and DX |
| Agent model | **Harness-native first, containers later** | Ship a great product for our stack fast; design the contract so containers slot in later |
| Deploy target | **Binary / Compose now, K8s later** | Simplest credible footprint first; swap in scale later |
| Build vs buy | **Build core, integrate heavy infra** | Don't reimplement durable execution, identity, sandboxing, observability |
| Durable execution | **DBOS** (not Temporal) | Library backed by Postgres — no separate cluster; preserves single-binary install |
| Execution model | **Subprocess per agent (harness binary)** | Real isolation now; *is* the container contract later |
| Durability boundary | **Agent-runtime SDK embeds DBOS in the subprocess** | Keeps harness untouched; best author contract; loop self-recovers in-process |
| Wire contract | **HTTP/JSON + SSE** | Debuggable, language-agnostic, maps onto harness's streaming events |
| Session model | **One subprocess (pool) per agent, many concurrent sessions inside** | Efficient, fast session start; logical isolation for the binary phase |
| Control surface | **REST API + CLI first, thin read-only web console** | Fastest path to a usable, demoable product |
| Build scope | **Approach 2 — operable single-node platform** | Usable & demoable; defers identity/sandboxes/rich observability to their sub-projects |
| Step granularity | **Step = whole turn** (model call + tool batch) | Matches harness loop boundary; replay ≤ one turn; tool calls individually checkpointed |
| Tool semantics | **Document at-least-once; guidance only** | Honest, simple, matches real durable-execution systems |

---

## 2. Architecture

One control-plane binary + Postgres. Each deployed agent runs as a supervised
subprocess (a pool of one or more). The durable loop lives **inside** the
subprocess and self-recovers; the control plane observes and controls via
shared Postgres and the HTTP/SSE contract.

```
  CLI ─┐                              ┌─ Web console (read-only)
       │   HTTP/JSON + token auth     │   agents · sessions · live SSE
       ▼                              ▼
  ┌──────────────────────────────────────────────────────────┐
  │ CONTROL PLANE — single Go binary                          │
  │  ┌────────────┐ ┌──────────────┐ ┌────────────┐ ┌───────┐ │
  │  │ Control API│ │ Agent Registry│ │ Supervisor │ │Session│ │
  │  │ REST + SSE │ │ agents+versions│ │spawn/health│ │Router │ │
  │  └────────────┘ └──────────────┘ │restart/drain│ └───────┘ │
  │                                   └────────────┘           │
  └──────────────────────────────────────────────────────────┘
       │ spawns + supervises (local HTTP/SSE)      ▲▼ shared Postgres
       ▼
  ┌───────────────────────────────────┐   ┌──────────────────────────┐
  │ AGENT SUBPROCESS (pool per agent)  │   │ POSTGRES                 │
  │  agent-runtime SDK (on harness):   │   │ • DBOS system tables     │
  │   • HTTP/SSE agent contract        │   │   (checkpoints → resume) │
  │   • harness loop as DBOS workflow  │   │ • agent registry/deploys │
  │     (turn = durable step)          │   │ • session index/metadata │
  │   • many concurrent sessions,      │   │ • session_events log     │
  │     each its own harness Runtime   │   │ • (pgvector for Memory)  │
  │  ↳ harness: loop·tools·compaction  │   └──────────────────────────┘
  │            ·providers              │
  └───────────────────────────────────┘
```

### Control-plane components

- **Control API** — REST endpoints (token-authenticated) for agent
  management, session management, an invoke proxy, and an SSE event stream.
- **Agent Registry** — deployed agents, their config, and versioned
  deployments (supports later rollback).
- **Supervisor** — owns the agent-deployment lifecycle: spawns subprocess
  pools, health-checks (`/healthz`), restarts with capped exponential
  backoff, drains on stop.
- **Session Router** — routes `invoke` calls to the correct subprocess,
  tracks session→subprocess assignment and session metadata.

### Layering

`harness` (the loop) → `agent-runtime SDK` (durability + contract) →
`subprocess` → `control plane`. **harness is not modified** — the SDK composes
it.

---

## 3. The Agent Contract (HTTP/JSON + SSE)

The contract every hosted agent satisfies. It is **versioned** (`/meta`
reports the contract version) so containerized agents added later target a
stable surface. This contract *is* the public "agent contract."

**Control plane → agent subprocess:**

| Method & path | Purpose |
|---|---|
| `POST /sessions` | Create or resume a session → `{session_id}` |
| `POST /sessions/{id}/invoke` | Body `{message, images?}` → **SSE stream** of events for a new turn |
| `GET /sessions/{id}/stream` | Re-attach to the SSE stream of an in-flight/recovering turn (no new message); replays buffered `session_events` then continues live |
| `POST /sessions/{id}/cancel` | Abort the running turn |
| `GET /sessions/{id}` | Status, turn count, resumable? |
| `GET /healthz` | Liveness / readiness |
| `GET /meta` | Agent id, version, contract version |

**SSE event types** (mirror harness `ChatEvent`): `text_delta`,
`tool_call_start`, `tool_result`, `turn_done`, `done`, `error`.

**Resume behavior:** subprocess dies mid-invoke → supervisor respawns →
client re-attaches via `GET /sessions/{id}/stream`; DBOS replays the workflow
from its last committed step. No lost work, no double-execution of committed
tool calls. The `session_events` log lets the console/client replay events
emitted before the break, then the stream continues live.

---

## 4. The agent-runtime SDK

A Go library, layered on harness, that lets an author satisfy the entire
contract with almost no code:

```go
agentruntime.Serve(agentruntime.Config{
    Spec:     harness.AgentSpec{ ... },
    Provider: anthropic.New( ... ),
    Tools:    reg,            // a harness tool.Registry
    // Postgres URL + listen addr injected via env by the platform
})
```

Under the hood the SDK:

- Binds the HTTP server implementing the contract in §3.
- Initializes DBOS (`dbos.NewDBOSContext` + `dbos.Launch`), pointed at the
  platform's Postgres (`DBOS_SYSTEM_DATABASE_URL`).
- Per session: builds a harness `Runtime`.
- Registers the harness run loop as a DBOS workflow
  (`dbos.RegisterWorkflow`); **each turn** runs as a durable step
  (`dbos.RunAsStep`). Tool executions within a turn are individually wrapped
  as steps so committed results are never re-run on replay.
- Streams harness events out as SSE.
- On boot, lets DBOS recover any workflows that were mid-flight.

**The author writes zero durability or HTTP code.** They bring an
`AgentSpec`, a provider, and tools.

### Durable-step granularity

Step = one whole turn (one model call + its tool batch) — the natural harness
loop boundary. On crash, at most one turn replays. Tool calls inside the turn
are individually checkpointed (idempotency-guarded against replay).

---

## 5. Data Model & Lifecycle

### Source-of-truth split (deliberate)

- **Conversation state** → harness session store (JSONL) inside the
  subprocess (harness's existing mechanism).
- **Durable execution state** → DBOS system tables in Postgres (enables
  mid-turn resume).
- **Platform / operational state** → control-plane tables (below).

The control plane never reaches into harness loop state; it observes via the
event log and DBOS status.

### Postgres schema (control-plane owned; DBOS owns its own system tables)

```
agents(id, name, contract_version, created_at, ...)
agent_deployments(id, agent_id, version, config_json, binary_ref,
                  status, replicas, created_at)      -- versioned; enables rollback
sessions(id, agent_id, deployment_id, status, turn_count,
         created_at, last_active_at, dbos_workflow_id)
session_events(id, session_id, seq, type, payload_json, ts)  -- append-only;
                  -- control-plane-visible mirror for console + client re-attach;
                  -- harness JSONL is the in-subprocess source of truth
api_tokens(id, hash, scope, created_at)
-- DBOS-managed: workflow_status, operation_outputs, ... (not designed by us)
```

### Agent (deployment) lifecycle

`registered → starting → running → (restarting | draining) → stopped`,
with `failed` on exceeding the crash-backoff cap. Supervisor owns
transitions. Sessions survive restarts (DBOS).

### Session lifecycle

`created → running → idle → (recovering) → closed | failed`. **recovering**
is the key state: on respawn, DBOS replays the workflow from its last
committed step and the session returns to running.

---

## 6. Error Handling & Failure Semantics

1. **Subprocess crash mid-turn** (headline): supervisor respawns (capped
   backoff); SDK triggers DBOS recovery; interrupted turn resumes from last
   committed step; committed tool calls are not re-run. Client re-attaches;
   `session_events` replays pre-break events.
2. **Tool failures**: (a) a tool *returning* an error is normal — harness
   feeds it to the model as a tool_result. (b) A non-idempotent tool crashing
   mid-execution falls under **at-least-once** semantics (see §7).
3. **LLM provider failures**: harness retries within a turn (cache-preserving
   fallback). A process crash during a provider call replays the whole turn
   cleanly. Persistent failure → turn `error` → session `failed` but
   **resumable**.
4. **Control-plane crash**: durable state is in Postgres. On restart it reads
   the registry, re-discovers running subprocesses via health probe, resumes
   supervision. Subprocesses keep running (loops are self-durable); only new
   invokes/deploys pause.
5. **Postgres unavailable**: the availability floor. New sessions/turns
   rejected with `503`; in-flight steps block/fail per DBOS. HA Postgres is
   the operator's lever. No invented fallback store.
6. **Poison workflows / runaway loops**: harness `MaxTurns` caps loops; a
   workflow that crash-recovers repeatedly hits a **recovery-attempt cap** →
   `failed` (dead-letter), surfaced in the console, not retried forever.
7. **Cancellation**: `POST /sessions/{id}/cancel` aborts the turn (harness
   honors context cancel); DBOS workflow finalized so it is not recovered on
   next boot.
8. **Graceful shutdown / drain**: stop → `draining` (no new sessions, bounded
   grace for active turns), `dbos.Shutdown` with timeout, exit. In-flight
   beyond grace is killed and resumed on next deploy.

---

## 7. Tool Execution Guarantee

Tool execution is **at-least-once**. A non-idempotent tool that crashes after
its side effect but before its checkpoint commits may run twice on resume.
This is documented prominently in the contract; authors are guided to make
tools idempotent where it matters. No platform-level idempotency machinery is
built in the spine. Exactly-once (via tool-side idempotency keys) is explicit
future guidance, revisited only if real usage demands it.

---

## 8. Control Surface (CLI + API + thin console)

- **CLI** (primary surface): `deploy` (from config), `list`, `invoke`,
  `sessions` (list/inspect/resume), `logs`, `stop`.
- **REST API**: the same operations, token-authenticated; plus the SSE event
  stream proxy.
- **Web console** (read-only this phase): agents, sessions, and a live SSE
  event stream view. Richer UI grows in the Observability sub-project.

---

## 9. Testing Strategy

- **Hermetic unit tests**: registry CRUD, supervisor state-machine
  transitions, session router, contract handlers (table-driven), event-log
  append/replay. A store interface with an in-memory fake keeps these
  Postgres-free (mirrors harness's hermetic-test discipline).
- **Integration tests** (gated, real Postgres via Compose / `dbos postgres
  start`): deploy → invoke → multi-turn event stream; graceful drain;
  recovery-attempt cap → dead-letter.
- **Flagship test — durable resume**: spin up agent + Postgres, start a
  multi-turn invoke, `kill -9` the subprocess mid-turn, assert the session
  resumes from the correct step and committed tool calls are not duplicated.
  This is the single most important acceptance criterion in the sub-project.
- **Contract conformance suite**: a reusable test suite exercising the
  HTTP/SSE contract against *any* agent binary — an executable definition of
  "valid hosted agent," reusable against containers later.

---

## 10. Scope Boundaries (what this sub-project does NOT do)

Deferred to their own sub-projects, by design:

- **Identity** — proper auth, agent identity, secrets, OAuth broker (spine
  has only token auth on the control API).
- **Sandboxes** — browser tool and code interpreter isolation.
- **Observability** — tracing, metrics, rich dashboards (spine has structured
  logs + a thin read-only console + the event log).
- **Gateway** — tool/MCP federation.
- **Memory** — managed multi-tenant memory (pgvector is provisioned but
  unused here).
- **Containers / K8s** — arbitrary-container hosting and orchestrated scale
  (the contract is designed to admit them later).
- **Multi-tenancy, RBAC, autoscaling, deploy/rollback UX** — production
  hardening pulled into later work once Identity/Observability exist to inform
  the boundaries.

---

## 11. Internal Milestones (sequencing hint for planning)

1. **Durable walking skeleton** — one statically-registered agent, one
   subprocess, the SDK wrapping the harness loop as a DBOS workflow, the
   control plane proxying invoke + SSE and restarting on crash, and the
   flagship kill-mid-turn resume test passing. De-risks the DBOS×harness
   integration first.
2. **Multi-agent platform** — registry, multi-agent deploy/list/stop,
   subprocess pools, session management across agents, supervisor lifecycle,
   full CLI.
3. **Operability layer** — token auth, structured logs, thin read-only web
   console, Compose file, contract conformance suite.

---

## Appendix — Key external dependencies

- **harness** (`github.com/sausheong/harness`) — agent loop, tools, sessions,
  compaction, providers. Owned; unmodified by this sub-project.
- **DBOS Transact (Go)** (`github.com/dbos-inc/dbos-transact-golang/dbos`) —
  durable workflows/steps checkpointed to Postgres. Key calls:
  `NewDBOSContext`, `Launch`, `Shutdown`, `RegisterWorkflow`, `RunWorkflow`,
  `RunAsStep`, `WorkflowHandle.GetResult`, `NewWorkflowQueue`.
- **Postgres** (+ **pgvector**, provisioned for the later Memory sub-project).
