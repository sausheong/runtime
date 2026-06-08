# Runtime

**An on-prem platform for hosting and running LLM agents** — the open-source,
self-hosted equivalent of AWS Bedrock AgentCore. Run durable, resumable agents
on your own hardware, with no cloud dependency.

Runtime hosts [harness](https://github.com/sausheong/harness)-based agents as
supervised subprocesses behind a single control plane. Every conversation turn
is checkpointed to Postgres via [DBOS](https://github.com/dbos-inc/dbos-transact-golang),
so an agent that crashes mid-turn **resumes from its last completed turn** —
no lost work, no duplicated committed tool calls.

```
┌──────────────────────────────────────────────────────────────┐
│  Single binary + Postgres.  Many agents.  Durable by default.  │
└──────────────────────────────────────────────────────────────┘
```

---

## Table of contents

- [What Runtime gives you](#what-runtime-gives-you)
- [Architecture](#architecture)
- [Concepts](#concepts)
- [Quick start](#quick-start)
- [Configuring agents (`runtime.yaml`)](#configuring-agents-runtimeyaml)
- [The CLI (`runtimectl`)](#the-cli-runtimectl)
- [HTTP API reference](#http-api-reference)
- [Writing your own agent (the SDK)](#writing-your-own-agent-the-sdk)
- [How durability works](#how-durability-works)
- [Deployment](#deployment)
- [Configuration reference](#configuration-reference)
- [Testing](#testing)
- [Project layout](#project-layout)
- [Status, scope & limitations](#status-scope--limitations)

---

## What Runtime gives you

| Capability | What it means |
|---|---|
| **Durable agent loops** | Each turn is a DBOS step checkpointed to Postgres. Kill the process mid-turn; on restart the session resumes from the last completed turn. |
| **Multi-agent hosting** | One control plane hosts many agents, each an isolated OS subprocess, declared in a config file. |
| **Path-routed control plane** | A single HTTP endpoint routes `/agents/{id}/...` to the right agent. One URL to operate the whole fleet. |
| **Crash supervision** | Every agent has a supervisor that restarts it (capped backoff) if it dies. One agent crashing never affects the others. |
| **Session management** | Create sessions, stream their events (SSE), re-attach after a disconnect, list an agent's sessions, and see real per-session status + turn counts. |
| **Streaming everything** | Agent output streams as Server-Sent Events end-to-end, through the control-plane proxy, with immediate flush. |
| **Operator CLI** | `runtimectl` to list agents, invoke sessions, stream logs, and list sessions. |
| **BYO agent** | Link the `agentruntime` SDK, hand it a harness `AgentSpec` + provider + tools, and get the durable contract for free — zero durability or HTTP code. |
| **On-prem & self-contained** | Go binaries + Postgres. No cloud, no Kubernetes required, air-gap friendly. |

---

## Architecture

```
                        ┌─────────────────────────────┐
   runtimectl ──HTTP──▶ │  runtimed  (control plane)  │
   (operator CLI)       │                             │
                        │  • Registry (runtime.yaml)  │
                        │  • Router  /agents/{id}/*    │
                        │  • GET /agents, /healthz     │
                        │  • one Supervisor per agent  │
                        └─────────────┬───────────────┘
                       spawns +       │ reverse-proxy (SSE-aware)
                       supervises     │
            ┌──────────────┬──────────┴───────┬──────────────┐
            ▼              ▼                   ▼              ▼
        ┌────────┐    ┌────────┐          ┌────────┐
        │agentd  │    │agentd  │   ...    │agentd  │   (one subprocess per agent)
        │"support"│   │"research"│        │  "..." │
        │        │    │        │          │        │
        │ agentruntime.Serve:             │        │
        │  • HTTP/SSE agent contract      │        │
        │  • harness loop per session     │        │
        │  • each turn = a DBOS step      │        │
        └───┬────┘    └───┬────┘          └───┬────┘
            └─────────────┴───────────────────┘
                          ▼
                  ┌───────────────┐
                  │   Postgres    │
                  │ • DBOS checkpoints (durable resume)
                  │ • sessions (agent_id, status, turn_count, workflow_id)
                  │ • session_events (append-only, for re-attach/replay)
                  └───────────────┘
```

**Three binaries:**

- **`runtimed`** — the control plane. Loads `runtime.yaml`, supervises one
  `agentd` per agent, and serves the routed HTTP API.
- **`agentd`** — an agent subprocess. Runs one agent via `agentruntime.Serve`.
  (The bundled `agentd` uses a deterministic built-in test agent; swap in a real
  LLM provider for production — see [Writing your own agent](#writing-your-own-agent-the-sdk).)
- **`runtimectl`** — the operator CLI.

**One library (the SDK):**

- **`agentruntime`** — what an agent author links. `Serve(ctx, Config)` turns a
  harness agent into a durable, contract-speaking subprocess.

---

## Concepts

- **Agent** — a hosted LLM agent, declared in `runtime.yaml` with an `id`, a
  display name, a model string, and a `listen_addr`. Runs as one `agentd`
  subprocess.
- **Session** — one durable conversation with an agent. Backed by a DBOS
  workflow whose id equals the session id. Has a status
  (`created → running → completed | error`) and a turn count.
- **Turn** — one iteration of the agent loop (a model call plus its tool batch).
  Each turn is the unit of durability: a DBOS step, checkpointed on completion.
- **Event** — a streamed unit of session output (`text`, `tool_result`,
  `done`, `error`), delivered over SSE and persisted to an append-only log so
  clients can re-attach and replay.

---

## Quick start

**Prerequisites:** Go 1.25.1+, a reachable Postgres, and a local checkout of
[harness](https://github.com/sausheong/harness) as a sibling directory
(`../harness`) — wired via a `replace` directive in `go.mod` during the v0.x line.

### 1. Start Postgres

Use the bundled Compose file:

```bash
docker compose -f deploy/docker-compose.yml up -d
```

…or point at any existing Postgres. The default DSN is
`postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable` (override
with `RUNTIME_PG_DSN`). For a non-Docker local Postgres, create the role/db once:

```sql
CREATE ROLE runtime LOGIN PASSWORD 'runtime';
CREATE DATABASE runtime OWNER runtime;
```

### 2. Build the binaries

```bash
go build -o agentd     ./cmd/agentd
go build -o runtimed   ./cmd/runtimed
go build -o runtimectl ./cmd/runtimectl
```

### 3. Define your agents

The repo ships an example `runtime.yaml` with two agents. (See
[Configuring agents](#configuring-agents-runtimeyaml).)

### 4. Run the control plane

```bash
RUNTIME_AGENTD_BIN=./agentd ./runtimed
# control plane on :8080 hosting 2 agents
# supervising agent "support" at 127.0.0.1:8101
# supervising agent "research" at 127.0.0.1:8102
```

### 5. Drive it

```bash
./runtimectl agents
# support   Support Agent   test/scripted
# research  Research Agent  test/scripted

./runtimectl invoke --agent support "hello"
# session: ses-…
# data: {"type":"text","text":"final answer"}
# data: {"type":"done"}

./runtimectl sessions --agent support
# ses-…   completed   turns=2

./runtimectl logs --agent support ses-…   # replay a session's events
```

---

## Configuring agents (`runtime.yaml`)

`runtimed` reads its agent list from a YAML file (default `runtime.yaml`,
override with `RUNTIME_CONFIG`):

```yaml
agents:
  - id: support              # unique; used in URLs and the CLI --agent flag
    name: Support Agent      # human-readable display name
    model: test/scripted     # "provider/model" string
    listen_addr: 127.0.0.1:8101   # unique host:port for this agent's subprocess

  - id: research
    name: Research Agent
    model: test/scripted
    listen_addr: 127.0.0.1:8102
```

**Validation** (enforced at startup; bad config exits non-zero before anything
is spawned):

- at least one agent
- every agent needs `id`, `name`, `model`, and `listen_addr`
- `id`s must be unique
- `listen_addr`s must be unique

To add or remove an agent, edit `runtime.yaml` and restart `runtimed`.

---

## The CLI (`runtimectl`)

`runtimectl` talks to the control plane at `RUNTIME_CTL_URL` (default
`http://localhost:8080`).

| Command | Description |
|---|---|
| `runtimectl agents` | List registered agents (`id  name  model`). |
| `runtimectl invoke [--agent <id>] "<message>"` | Start a session on an agent and stream its events to completion. |
| `runtimectl sessions [--agent <id>]` | List an agent's sessions (`id  status  turns=N`). |
| `runtimectl logs [--agent <id>] <session-id>` | Replay/stream a session's events from the start. |

`--agent` may be omitted when exactly one agent is registered (it's auto-selected);
it's required when there are several.

---

## HTTP API reference

### Control plane (served by `runtimed`)

| Method & path | Description |
|---|---|
| `GET /healthz` | Control-plane liveness. |
| `GET /agents` | JSON list of registered agents: `[{id,name,model}]`. |
| `ANY /agents/{id}/...` | Reverse-proxied to agent `{id}`'s subprocess, with the `/agents/{id}` prefix stripped. Unknown id → `404`. |

### Agent contract (served by each `agentd`; reach it via the proxy prefix)

These are the endpoints each agent exposes. Through the control plane, prefix
them with `/agents/{id}`.

| Method & path | Description |
|---|---|
| `GET /healthz` | Agent liveness/readiness. |
| `GET /meta` | `{agent_id, contract_version}`. The versioned agent contract. |
| `POST /sessions` | Body `{"message": "..."}`. Creates a session, starts the durable workflow, returns `{"session_id": "..."}`. |
| `GET /sessions` | List this agent's sessions: `[{id,status,turn_count}]`. |
| `GET /sessions/{id}` | One session's status snapshot: `{id,status,turn_count}`. |
| `GET /sessions/{id}/stream?since=<seq>` | **SSE stream** of the session's events. Replays buffered events after `since` (default 0), then streams live until a terminal `done`/`error`. Each event carries an SSE `id:` line (its sequence number) so a client can resume with `?since=`. |

**Event types** (the `type` field in each SSE `data:` payload): `text`,
`tool_result`, `done`, `error`.

**Example — invoke and stream directly with curl:**

```bash
# Start a session on the "support" agent
SID=$(curl -s -X POST localhost:8080/agents/support/sessions \
        -H 'content-type: application/json' \
        -d '{"message":"hello"}' | jq -r .session_id)

# Stream it
curl -N localhost:8080/agents/support/sessions/$SID/stream?since=0
# id: 1
# data: {"type":"text","text":"final answer"}
# id: 2
# data: {"type":"done"}
```

---

## Writing your own agent (the SDK)

The bundled `agentd` runs a deterministic **test agent** (no API keys, no
network) so the platform can be exercised out of the box. To run a real agent,
write your own `agentd`-style binary that hands `agentruntime.Serve` a harness
agent. The entire surface is one `Config`:

```go
package main

import (
    "context"
    "os"
    "os/signal"
    "syscall"

    "github.com/sausheong/harness/providers/anthropic"
    hrt "github.com/sausheong/harness/runtime"
    "github.com/sausheong/harness/tool"
    "github.com/sausheong/harness/tools/bash"
    "github.com/sausheong/harness/tools/file"

    "github.com/sausheong/runtime/agentruntime"
)

func main() {
    dsn  := os.Getenv("RUNTIME_PG_DSN")      // injected by runtimed
    addr := os.Getenv("RUNTIME_LISTEN_ADDR") // injected by runtimed
    id   := os.Getenv("RUNTIME_AGENT_ID")    // injected by runtimed

    reg := tool.NewRegistry()
    reg.Register(&file.ReadFileTool{WorkDir: "/work"})
    reg.Register(&bash.BashTool{WorkDir: "/work"})

    ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    defer stop()

    err := agentruntime.Serve(ctx, agentruntime.Config{
        Spec: hrt.AgentSpec{
            ID:           id,
            Name:         "My Agent",
            Model:        "anthropic/claude-sonnet-4-6",
            SystemPrompt: "You are a helpful coding assistant.",
            MaxTurns:     25,
        },
        Provider:    anthropic.NewAnthropicProvider(os.Getenv("ANTHROPIC_API_KEY"), ""),
        Tools:       reg,
        ListenAddr:  addr,
        PostgresDSN: dsn,
    })
    if err != nil {
        panic(err)
    }
}
```

`agentruntime.Serve` does the rest: binds the HTTP/SSE contract, initializes
DBOS, runs the harness loop one durable step per turn, tracks session status,
persists the event log, and recovers in-flight workflows on restart. Point
`runtime.yaml`'s `model`/`name` at your agent and set `RUNTIME_AGENTD_BIN` to
your binary.

**`Config` fields:**

| Field | Type | Purpose |
|---|---|---|
| `Spec` | `harness/runtime.AgentSpec` | Agent identity, model (`provider/model`), system prompt, `MaxTurns`, etc. |
| `Provider` | `harness/llm.LLMProvider` | The resolved LLM client for the model. harness ships Anthropic, OpenAI/Ollama, Gemini, Qwen. |
| `Tools` | `*harness/tool.Registry` | The agent's tools. |
| `ListenAddr` | `string` | HTTP bind address (injected by `runtimed`). |
| `PostgresDSN` | `string` | DBOS system DB + control-plane store DSN (injected by `runtimed`). |

---

## How durability works

1. A session is a **DBOS workflow** whose id equals the session id.
2. The workflow loops, running **one turn per DBOS step** (`RunAsStep`). A step
   executes the harness agent's `RunTurn` and returns the session entries that
   turn produced.
3. When a step completes, DBOS **checkpoints** its result to Postgres.
4. If the process crashes, on restart DBOS **recovers** the in-flight workflow
   and replays it: completed steps return their checkpointed results *without
   re-executing* (no re-calling the LLM, no re-running committed tools), and the
   loop continues from the first uncompleted turn.
5. Client-facing events are derived deterministically from each turn's entries
   and persisted to an append-only log, so a client can re-attach via
   `GET /sessions/{id}/stream?since=<seq>` and see the full stream.

**Tool execution is at-least-once.** A tool that crashes *after* its side effect
but *before* its turn checkpoints will run again on resume. Make tools
idempotent where it matters. (The bundled integration test demonstrates this
honestly rather than hiding it.)

**Recovery is keyed on the agent binary's version.** DBOS recovers a pending
workflow only for a matching executor + application version (the agentd binary
hash). Recovering the *same* binary across a crash/restart — the normal case —
works as shown by the resume integration test. Recovering across a recompiled
binary would require pinning `DBOS__APPVERSION`.

---

## Deployment

### Single host (recommended starting point)

The whole platform is Go binaries + Postgres. A minimal production-ish layout:

1. **Postgres** — managed instance, or the bundled Compose service. Use HA
   Postgres if you need the platform itself to be HA (it is the durability and
   availability floor).
2. **Build** the three binaries (`agentd`, `runtimed`, `runtimectl`) on the
   target architecture, or in CI.
3. **Configure** `runtime.yaml` with your agents and a real agent binary.
4. **Run `runtimed`** under a process manager (systemd, supervisord, a
   container) with the environment set. `runtimed` itself supervises the agent
   subprocesses — you only manage the one `runtimed` process.

Example systemd unit:

```ini
[Unit]
Description=Runtime control plane
After=network.target postgresql.service

[Service]
Environment=RUNTIME_PG_DSN=postgres://runtime:runtime@db:5432/runtime?sslmode=disable
Environment=RUNTIME_CTL_ADDR=:8080
Environment=RUNTIME_AGENTD_BIN=/opt/runtime/agentd
Environment=RUNTIME_CONFIG=/etc/runtime/runtime.yaml
ExecStart=/opt/runtime/runtimed
Restart=always
WorkingDirectory=/opt/runtime

[Install]
WantedBy=multi-user.target
```

### Docker Compose

The bundled `deploy/docker-compose.yml` provisions Postgres (pgvector image).
Add `runtimed` as a service that mounts your `runtime.yaml` and agent binary,
sets `RUNTIME_PG_DSN` to the `postgres` service, and exposes `:8080`. Build the
binaries into the image (multi-stage Go build) so `RUNTIME_AGENTD_BIN` points at
the in-image `agentd`.

### Notes on multi-agent startup

`runtimed` starts agents **sequentially with a readiness gate** (it waits for
each agent's `/healthz` before starting the next). This is deliberate: DBOS's
first-run schema initialization is not safe to run from many processes at once,
so the first agent creates the schema and the rest initialize against it. Cold
start of N agents therefore takes roughly N × (agent boot time); steady state is
unaffected.

### Operational characteristics

- **One agent crashing** is contained: its supervisor restarts it (capped
  backoff); peers are untouched.
- **`runtimed` restart**: durable state lives in Postgres; agents keep running
  while it's down (their loops are self-durable). Restarting `runtimed`
  re-reads config and re-supervises.
- **Postgres down** is the hard dependency: new sessions/turns fail; treat
  Postgres availability as the platform floor.
- **Graceful shutdown**: SIGINT/SIGTERM to `runtimed` cancels all supervisors;
  agents drain via DBOS shutdown.

---

## Configuration reference

### Environment variables

| Variable | Used by | Default | Purpose |
|---|---|---|---|
| `RUNTIME_PG_DSN` | runtimed, agentd | `postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable` | Postgres DSN (DBOS system DB + control-plane store). |
| `RUNTIME_CONFIG` | runtimed | `runtime.yaml` | Path to the agent config file. |
| `RUNTIME_CTL_ADDR` | runtimed | `:8080` | Control-plane HTTP bind address. |
| `RUNTIME_AGENTD_BIN` | runtimed | `./agentd` | Path to the agent subprocess binary to spawn. |
| `RUNTIME_LISTEN_ADDR` | agentd | (set by runtimed per agent) | Agent subprocess HTTP bind address. |
| `RUNTIME_AGENT_ID` | agentd | (set by runtimed per agent) | The agent's id. |
| `RUNTIME_CTL_URL` | runtimectl | `http://localhost:8080` | Control-plane base URL the CLI targets. |

`runtimed` injects `RUNTIME_LISTEN_ADDR` and `RUNTIME_AGENT_ID` into each agent
subprocess from `runtime.yaml`; you don't set them by hand.

### Postgres schema

Applied automatically on startup (under an advisory lock so concurrent agents
don't race): `agents`, `sessions` (with `agent_id`, `status`, `turn_count`,
`workflow_id`), `session_events` (append-only), plus DBOS's own `dbos` schema.
pgvector is provisioned (the Compose image) for a future managed-memory
sub-project but unused here.

---

## Testing

```bash
go test ./...        # hermetic unit tests — no Postgres required
go vet ./...
```

**Integration tests** need a running Postgres and are gated behind the
`integration` build tag so the default run stays hermetic:

```bash
docker compose -f deploy/docker-compose.yml up -d   # or any Postgres at the DSN
go test -tags integration ./test/ -v -count=1 -timeout 200s
```

Two integration tests cover the platform's headline guarantees:

- **`TestResumeAfterKill`** — starts a real `agentd`, kills it mid-turn,
  restarts it, and asserts the session resumes via DBOS recovery and completes
  (demonstrating at-least-once tool semantics honestly).
- **`TestMultiAgentRouting`** — starts `runtimed` with a two-agent config,
  invokes a session on each agent through the router, and asserts both complete,
  sessions are isolated per agent, and status/turn_count are tracked.

---

## Project layout

```
runtime/
├── cmd/
│   ├── runtimed/      # control-plane binary (registry, router, supervisors)
│   ├── agentd/        # agent subprocess binary (bundled test agent)
│   └── runtimectl/    # operator CLI
├── agentruntime/      # the SDK: Serve(), the durable DBOS workflow, the
│                      #   HTTP/SSE agent contract, session manager
├── controlplane/      # Registry, /agents/{id} router, Supervisor, proxy
├── internal/
│   ├── config/        # runtime.yaml loader + validation
│   └── store/         # control-plane store (sessions, event log): pg + in-memory
├── testagent/         # deterministic provider + marker tool (for tests/demo)
├── test/              # //go:build integration end-to-end tests
├── deploy/            # docker-compose.yml (Postgres)
├── runtime.yaml       # example agent config
└── docs/superpowers/  # design specs and implementation plans
```

---

## Status, scope & limitations

Runtime is built in milestones. **Milestone 1** delivered the durable single-agent
spine; **Milestone 2** (current) adds the multi-agent platform: config-driven
registry, path routing, per-agent supervision, session status tracking, and the
full CLI.

**Deliberately not yet implemented** (each is planned, scoped to a later
milestone or sub-project):

- **Web console** — a read-only operator UI (next milestone). Today: CLI + API.
- **Authentication / RBAC / multi-tenancy** — no auth on the control API yet
  (Identity sub-project).
- **Subprocess pools / autoscaling** — one subprocess per agent today; no
  per-agent replicas or load-based scaling.
- **Dynamic deploy** — agents come from `runtime.yaml` at startup; no runtime
  `POST /agents` registration or rollback yet.
- **Sandboxes** (isolated browser / code-interpreter tools), a **tool/MCP
  gateway**, and **managed memory** — each its own sub-project.
- **Containers / Kubernetes** — the agent contract is designed to admit
  containerized agents later; today agents are local subprocesses.
- **`GET /sessions/{id}` over the proxy returns status** but the control plane
  does not yet aggregate cross-agent session views (per-agent only).
- **Hardening details**: control-plane graceful shutdown uses an unbounded
  timeout; the reverse proxy returns the default 502 if a backend is mid-restart;
  `session_events` sequence allocation assumes one writer per session (true
  today). These are tracked for the operability milestone.

See `docs/superpowers/specs/` for the full design and milestone specs.

## License

See the workspace license.
