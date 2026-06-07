# Runtime

An on-prem platform for hosting and running LLM agents — the open-source
equivalent of AWS Bedrock AgentCore, built to run on your own hardware.

This repository is the **Runtime spine** (sub-project 1 of 6): serverless-style
agent hosting with **durable, resumable agent loops**. It runs
[harness](https://github.com/sausheong/harness)-based agents as supervised
subprocesses, with each conversation turn checkpointed to Postgres via
[DBOS](https://github.com/dbos-inc/dbos-transact-golang) so a crashed agent
resumes from its last completed turn — no lost work, no duplicated committed
tool calls.

> **Status: Milestone 1 — durable walking skeleton.** A single statically
> configured agent, one subprocess, end-to-end durable resume proven by an
> integration test. Multi-agent registry, identity, sandboxes, observability,
> a tool gateway, and managed memory are later sub-projects. See
> [`docs/superpowers/specs/2026-06-07-runtime-spine-design.md`](docs/superpowers/specs/2026-06-07-runtime-spine-design.md)
> for the full design and
> [`docs/superpowers/plans/2026-06-07-runtime-spine-m1-durable-skeleton.md`](docs/superpowers/plans/2026-06-07-runtime-spine-m1-durable-skeleton.md)
> for this milestone's plan.

## Architecture

```
  runtimectl (CLI) ──HTTP──▶ runtimed (control plane)
                                 │ supervises + reverse-proxies
                                 ▼
                            agentd (agent subprocess)
                                 │ agentruntime.Serve
                                 │  • HTTP/SSE agent contract
                                 │  • harness loop, each turn a DBOS step
                                 ▼
                            Postgres
                              • DBOS checkpoints (durable resume)
                              • sessions + append-only event log
```

- **`agentruntime`** — the SDK an agent author links. `Serve(ctx, Config)`
  binds the HTTP/SSE contract, wraps the harness loop as a DBOS workflow (one
  durable step per turn), and recovers in-flight workflows on boot. The author
  supplies a harness `AgentSpec`, an LLM provider, and a tool registry — no
  durability or HTTP code.
- **`controlplane`** — a supervisor that keeps the agent subprocess alive
  (restart-on-crash with backoff) and a reverse proxy that exposes the agent
  contract.
- **`internal/store`** — the control-plane store (sessions + event log) with an
  in-memory impl for tests and a Postgres impl for production.
- **`cmd/agentd`** — the agent subprocess binary.
- **`cmd/runtimed`** — the control-plane binary.
- **`cmd/runtimectl`** — the operator CLI (`invoke`, `logs`).

The durable loop lives **inside** the subprocess and self-recovers; the control
plane observes and controls it via shared Postgres and the contract.

## Requirements

- **Go 1.25.1+**
- **Postgres** reachable at a DSN you provide (DBOS uses it as its system
  database; the control plane stores sessions + events there).
- A local checkout of [harness](https://github.com/sausheong/harness) as a
  sibling directory (`../harness`) — wired via a `replace` directive in
  `go.mod` during the v0.x line.

## Quick start

1. **Start Postgres.** Either use the bundled Compose file:

   ```bash
   docker compose -f deploy/docker-compose.yml up -d
   ```

   or point at any existing Postgres. The default DSN is
   `postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable`
   (override with `RUNTIME_PG_DSN`).

2. **Build the binaries:**

   ```bash
   go build -o agentd   ./cmd/agentd
   go build -o runtimed ./cmd/runtimed
   go build -o runtimectl ./cmd/runtimectl
   ```

3. **Run the control plane** (it spawns and supervises `agentd`):

   ```bash
   RUNTIME_AGENTD_BIN=./agentd ./runtimed
   ```

4. **Drive a session** from another shell:

   ```bash
   ./runtimectl invoke "hello"
   # prints a session id, then streams the agent's SSE events until 'done'
   # replay a session's events later:
   ./runtimectl logs <session-id>
   ```

> The bundled `agentd` uses a deterministic, network-free **test agent**
> (scripted provider + a `marker` tool) so the durability machinery can be
> exercised without API keys. Wiring a real LLM provider is a one-line change
> in `cmd/agentd/main.go` (swap `testagent.New()` for an `anthropic` /
> `openai` / etc. provider from harness and register real tools).

## Configuration (environment)

| Variable | Used by | Default |
|---|---|---|
| `RUNTIME_PG_DSN` | runtimed, agentd | `postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable` |
| `RUNTIME_CTL_ADDR` | runtimed | `:8080` |
| `RUNTIME_AGENT_ADDR` | runtimed | `127.0.0.1:8081` |
| `RUNTIME_AGENTD_BIN` | runtimed | `./agentd` |
| `RUNTIME_LISTEN_ADDR` | agentd | (set by runtimed) |
| `RUNTIME_AGENT_ID` | agentd | (set by runtimed) |
| `RUNTIME_CTL_URL` | runtimectl | `http://localhost:8080` |

## Testing

```bash
go test ./...        # hermetic unit tests — no Postgres required
go vet ./...
```

The **durable-resume integration test** is the milestone's headline acceptance
criterion: it starts a real `agentd`, kills it mid-turn, restarts it, and
asserts the session resumes via DBOS recovery and completes. It needs a running
Postgres and is gated behind the `integration` build tag so the default run
stays hermetic:

```bash
docker compose -f deploy/docker-compose.yml up -d   # or any Postgres at the DSN
go test -tags integration ./test/ -v -count=1 -timeout 120s
```

It demonstrates the platform's **at-least-once** tool-execution semantics
honestly: a tool that crashes after its side effect but before its turn
checkpoints runs again on resume (the test asserts the marker ran ≥ 1 times and
the session still completes exactly once).

## Milestone 1 scope & limitations

Deliberately deferred to later sub-projects / milestones:

- **Single static agent.** No multi-agent registry, deploy/rollback, or
  subprocess pools yet (Milestone 2).
- **Token auth, RBAC, multi-tenancy** (Milestone 3 / Identity sub-project).
- **Browser & code-interpreter sandboxes**, a **tool/MCP gateway**, **managed
  memory**, and a **tracing/observability** dashboard — each its own
  sub-project.
- **At-least-once tools.** Exactly-once requires tool-side idempotency keys;
  M1 documents the at-least-once contract rather than building dedup machinery.
- **DBOS recovery across a recompiled binary.** Recovery keys on DBOS's
  application version (the agentd binary hash); recovering a workflow across a
  code change would require pinning `DBOS__APPVERSION`. Recovery of the *same*
  binary across a crash/restart (the normal case) works as shown by the
  integration test.

## License

See the workspace license.
