# Runtime

**An on-prem platform for hosting and running LLM agents** — the open-source,
self-hosted equivalent of AWS Bedrock AgentCore. Run durable, resumable agents
on your own hardware, with no cloud dependency.

Runtime hosts [harness](https://github.com/sausheong/harness)-based agents as
supervised subprocesses behind a single control plane. Every conversation turn
is checkpointed to Postgres via [DBOS](https://github.com/dbos-inc/dbos-transact-golang),
so an agent that crashes mid-turn **resumes from its last completed turn** —
no lost work, no duplicated committed tool calls.

**Single binary + Postgres. Many agents. Durable by default.**

---

## Table of contents

- [What Runtime gives you](#what-runtime-gives-you)
- [Architecture](#architecture)
- [Concepts](#concepts)
- [Quick start](#quick-start)
- [Configuring agents (`runtime.yaml`)](#configuring-agents-runtimeyaml)
- [Authentication & multi-tenancy](#authentication--multi-tenancy)
- [The CLI (`runtimectl`)](#the-cli-runtimectl)
- [Web console](#web-console)
- [HTTP API reference](#http-api-reference)
- [Contract conformance](#contract-conformance)
- [Writing your own agent (the SDK)](#writing-your-own-agent-the-sdk)
- [Deploying an example agent (SG Nutrition Investigator)](#deploying-an-example-agent-sg-nutrition-investigator)
- [Hosting a foreign-SDK agent (Python shim)](#hosting-a-foreign-sdk-agent-python-shim)
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
| **Operator CLI** | `runtimectl` to list agents, invoke sessions, stream logs, list sessions, and run contract conformance. |
| **Token auth** | Optional bearer-token auth (named tokens in `runtime.yaml`) on the control plane, via header or cookie. Open mode when no tokens are configured. |
| **Read-only web console** | A built-in `/ui` console: fleet overview → per-agent sessions → live SSE session view. Server-rendered, zero JS build step. |
| **Contract conformance** | A reusable Go suite (and `runtimectl conformance`) that verifies any agent satisfies the HTTP/SSE contract — executable proof, CI-ready. |
| **Structured logging** | `slog` everywhere (text or JSON via `RUNTIME_LOG_FORMAT`), with agent/session fields. |
| **BYO agent** | Link the `agentruntime` SDK, hand it a harness `AgentSpec` + provider + tools, and get the durable contract for free — zero durability or HTTP code. |
| **On-prem & self-contained** | Go binaries + Postgres. No cloud, no Kubernetes required, air-gap friendly. Full-stack `docker compose` included. |

---

## Architecture

![Runtime architecture: runtimectl drives runtimed (the control plane), which spawns and supervises one agentd subprocess per agent; each agentd runs agentruntime.Serve and checkpoints to Postgres.](docs/images/architecture.png)

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

## Authentication & multi-tenancy

The control plane enforces **multi-tenant, role-based access control** at the
edge. Every agent belongs to a **tenant**; callers authenticate as a **principal**
(a human via OIDC, or a machine via a platform-issued **service key**) that is
scoped to one tenant with a **role**. All checks happen in `runtimed` — agents
themselves stay loopback-trusting and unmodified.

### Tenants and roles

Tag each agent with a `tenant:` in `runtime.yaml` (absent ⇒ the reserved
`default` tenant):

```yaml
agents:
  - {id: support, name: Support Agent, model: openai/gpt-5.4, listen_addr: 127.0.0.1:8101, tenant: alpha}
  - {id: research, name: Research Agent, model: openai/gpt-5.4, listen_addr: 127.0.0.1:8102, tenant: beta}
```

Three fixed roles, scoped per tenant:

| Role | Can do |
|---|---|
| `viewer` | list/get/stream sessions and agents (read) |
| `operator` | viewer **+** invoke (`POST /sessions`) |
| `admin` | operator **+** manage its tenant's users and service keys |

A request for an agent in **another tenant** returns `404` (existence is hidden,
not `403`). Insufficient role within your tenant returns `403`; a missing/invalid
credential returns `401`. `GET /agents` and the console are tenant-filtered.

### Human login (OIDC)

Point the control plane at an OIDC issuer; humans log into the console via the
authorization-code flow and the validated ID token rides in the `runtime_token`
cookie (re-verified against the issuer's JWKS on every request):

```bash
export RUNTIME_OIDC_ISSUER=https://issuer.example.com
export RUNTIME_OIDC_CLIENT_ID=runtime-console
export RUNTIME_OIDC_CLIENT_SECRET=...                 # for the console code exchange
export RUNTIME_OIDC_REDIRECT_URL=http://localhost:8080/ui/callback   # default
```

A validly-authenticated subject must still be **provisioned** as a user (below)
to gain any access — authentication proves *who*, the platform decides *what*.

### Machine access (service keys) and tenant administration

Tenants, users, and service keys live in Postgres and are managed at runtime via
the `runtimectl admin` API (admin-only; scoped to the caller's tenant):

```bash
# Bootstrap: a one-time superuser key (read from env, never stored) creates the
# first tenant + admin, then is removed from config.
export RUNTIME_ADMIN_BOOTSTRAP=once-only-superuser-secret
export RUNTIME_TOKEN=$RUNTIME_ADMIN_BOOTSTRAP

runtimectl admin tenant create alpha --name "Team Alpha"
runtimectl admin user add alice@corp --tenant alpha --role operator   # subject = OIDC sub/email
runtimectl admin key create --tenant alpha --role operator --label ci
#   → svk-7f3a….<secret>   (shown once — store it now)
runtimectl admin key revoke svk-7f3a…
```

Service keys are sent as `Authorization: Bearer svk-<id>.<secret>` (or the
`runtime_token` cookie). Only a bcrypt hash is stored; revocation is instant.
The CLI sends `Authorization: Bearer $RUNTIME_TOKEN` automatically.

```bash
export RUNTIME_TOKEN=svk-7f3a….<secret>
runtimectl agents          # lists only alpha's agents
```

### Per-tenant secrets (provider credentials)

Each tenant can store its own provider credentials (e.g. `OPENAI_API_KEY`),
encrypted at rest and injected as environment variables into that tenant's agent
subprocesses at spawn time — agents read `os.Getenv` unchanged, so **no agent
code changes** are needed. This lets each tenant bring its own keys instead of
sharing the operator's.

Enable the feature by setting a 32-byte master key (base64):

```bash
export RUNTIME_SECRETS_KEY="$(head -c32 /dev/urandom | base64)"
```

When `RUNTIME_SECRETS_KEY` is unset the feature is disabled and agents inherit
the operator's environment (the prior behavior). A set-but-malformed key is a
fatal startup error.

Manage secrets with `runtimectl` (admin role, scoped to your tenant):

```bash
runtimectl admin secret set OPENAI_API_KEY sk-xxxxxxxx   # set/overwrite
runtimectl admin secret ls                               # names + timestamps (never values)
runtimectl admin secret rm OPENAI_API_KEY                # delete
```

Secrets are **write-only**: the API never returns a stored value (`ls` shows
names and timestamps only). Values are encrypted with AES-256-GCM under the
operator master key. A secret change takes effect on the agent's **next
restart** (resolution happens at spawn). Tenant secrets shadow an inherited
operator var of the same name; a tenant with no secret falls back to the
operator env.

> **Security:** the master key lives in runtimed's environment (operator-managed,
> like the Postgres DSN). Losing it makes existing ciphertext unrecoverable —
> there is no key rotation in this milestone. The `set` value travels as JSON, so
> terminate TLS upstream; it also lands in shell history, so prefer a
> leading-space invocation (`HISTCONTROL=ignorespace`) for real keys. A
> `--value-stdin` flag is a candidate follow-up.

### Open mode & backward compatibility

- **No identity configured** (no OIDC issuer, no service keys, no users, no
  legacy `tokens:`) ⇒ **open mode**: every request passes, with a startup
  warning. Keeps local development friction-free. `GET /healthz` is always
  exempt, as are the console login page and static assets.
- **Legacy `tokens:`** from M3 still work (deprecated): each maps to a
  `default`-tenant superuser so existing deployments keep running after upgrade.
  Prefer service keys; `tokens:` will be removed in a later milestone.

> Service-key secrets and OIDC tokens travel as bearer credentials — terminate
> TLS upstream. Service-key verification is constant-time (bcrypt) and hashed at
> rest. Per-tenant secrets brokering is implemented (see above). Console session
> CSRF hardening (`state`/`nonce`) and secrets key rotation are tracked for later
> Identity milestones (see `ROADMAP.md` §B3).

---

## The CLI (`runtimectl`)

`runtimectl` talks to the control plane at `RUNTIME_CTL_URL` (default
`http://localhost:8080`), sending `RUNTIME_TOKEN` as a bearer token when set.

| Command | Description |
|---|---|
| `runtimectl agents` | List registered agents (`id  name  model`). |
| `runtimectl invoke [--agent <id>] "<message>"` | Start a session on an agent and stream its events to completion. |
| `runtimectl sessions [--agent <id>]` | List an agent's sessions (`id  status  turns=N`). |
| `runtimectl logs [--agent <id>] <session-id>` | Replay/stream a session's events from the start. |
| `runtimectl conformance [--agent <id>]` | Run the contract conformance suite against an agent; exits non-zero on failure. |
| `runtimectl admin tenant create <id> [--name <n>]` | Create a tenant (superuser). |
| `runtimectl admin user add <subject> [--tenant <t>] --role <r>` | Provision an OIDC subject as a user (`--tenant` only for a superuser). |
| `runtimectl admin user ls` | List users in the caller's tenant. |
| `runtimectl admin key create [--tenant <t>] --role <r> [--label <l>]` | Mint a service key; the secret is printed once. |
| `runtimectl admin key ls` | List the tenant's service keys (never the secret). |
| `runtimectl admin key revoke <id>` | Revoke a service key (instant). |
| `runtimectl admin secret set <name> <value> [--tenant <t>]` | Set/overwrite a tenant secret (write-only; injected into the tenant's agents). |
| `runtimectl admin secret ls` | List the tenant's secret names + timestamps (never the values). |
| `runtimectl admin secret rm <name>` | Delete a tenant secret. |

`--agent` may be omitted when exactly one agent is registered (it's auto-selected);
it's required when there are several. The `admin` commands require an `admin`
principal (send it via `RUNTIME_TOKEN`).

---

## Web console

`runtimed` serves a read-only operator console at **`/ui`** (same port as the
API). Open `http://localhost:8080/ui` in a browser.

- **`/ui`** — fleet overview, **filtered to the caller's tenant** (a superuser
  or open mode sees all).
- **`/ui/agents/{id}`** — that agent's sessions (id, status, turn count); `404`
  for an agent in another tenant.
- **`/ui/agents/{id}/sessions/{sid}`** — live session view, streaming events via
  `EventSource`.

When auth is enabled, the console redirects to **`/ui/login`**: with OIDC
configured it bounces to the identity provider and stores the validated token in
the `runtime_token` cookie; otherwise it falls back to a paste-token form. The
console is strictly read-only — it observes agents and sessions but cannot
deploy, stop, or invoke. Server-rendered Go templates with a tiny vanilla-JS/SSE
layer — no build step, embedded in the binary.

---

## HTTP API reference

### Control plane (served by `runtimed`)

| Method & path | Description |
|---|---|
| `GET /healthz` | Control-plane liveness (always auth-exempt). |
| `GET /agents` | JSON list of registered agents with a best-effort health probe: `[{id,name,model,healthy}]`. |
| `ANY /agents/{id}/...` | Reverse-proxied to agent `{id}`'s subprocess, with the `/agents/{id}` prefix stripped. Unknown id → `404`; agent down/restarting → `503`. |
| `GET /ui`, `/ui/...` | The read-only web console (see [Web console](#web-console)). |

All control-plane routes except `/healthz`, `/ui/login`, and `/ui/static/*` are
gated by the identity middleware when auth is enabled (see
[Authentication & multi-tenancy](#authentication--multi-tenancy)).

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

## Contract conformance

The `conformance` package is an executable definition of the agent contract: it
exercises `/healthz`, `/meta`, `POST /sessions`, the SSE stream (to a terminal
`done`), `GET /sessions/{id}`, and `GET /sessions` against any agent at a base
URL. Use it two ways:

**Operator — against a live agent through the control plane:**

```bash
runtimectl conformance --agent support
# ok: healthz: ok
# ok: meta: ok (contract v1)
# ok: create session: ok (ses-…)
# ok: stream: ok
# conformance: PASSED        (exit 0; non-zero on any failure)
```

**Agent author — as a Go test for your own agent binary:**

```go
import "github.com/sausheong/runtime/conformance"

func TestMyAgentConformsToContract(t *testing.T) {
    addr := startMyAgent(t) // however you boot it
    conformance.Run(t, "http://"+addr)
}
```

`conformance.Run(t, baseURL)` takes any `TestingT` (satisfied by `*testing.T`
and the CLI adapter), so the same checks gate CI for new agent binaries and
serve as the operator's live smoke test. This is what makes the contract a gate
rather than prose — and the same suite will validate containerized agents when
those land.

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
    id := os.Getenv("RUNTIME_AGENT_ID") // injected by runtimed

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
        Provider: anthropic.NewAnthropicProvider(os.Getenv("ANTHROPIC_API_KEY"), ""),
        Tools:    reg,
    })
    if err != nil {
        panic(err)
    }
}
```

`Config` is purely about the agent — its identity, model, behavior, and tools.
The operator parameters (where Postgres lives, which address to bind) are *not*
in `Config`: `Serve` reads `RUNTIME_PG_DSN` and `RUNTIME_LISTEN_ADDR` from the
environment `runtimed` injects, so an agent author never handles them.

`agentruntime.Serve` does the rest: reads those two operator env vars, binds the
HTTP/SSE contract, initializes DBOS, runs the harness loop one durable step per
turn, tracks session status, persists the event log, and recovers in-flight
workflows on restart. Point `runtime.yaml`'s `model`/`name` at your agent and set
`RUNTIME_AGENTD_BIN` to your binary.

**`Config` fields** (the entire agent-author surface):

| Field | Type | Purpose |
|---|---|---|
| `Spec` | `harness/runtime.AgentSpec` | Agent identity, model (`provider/model`), system prompt, `MaxTurns`, etc. |
| `Provider` | `harness/llm.LLMProvider` | The resolved LLM client for the model. harness ships Anthropic, OpenAI/Ollama, Gemini, Qwen. |
| `Tools` | `*harness/tool.Registry` | The agent's tools. |

`Serve` additionally reads two operator-injected environment variables (set by
`runtimed`, not by the agent author): `RUNTIME_PG_DSN` (DBOS system DB +
control-plane store) and `RUNTIME_LISTEN_ADDR` (HTTP bind address).

---

## Deploying an example agent (SG Nutrition Investigator)

The repo ships a real, non-trivial example agent under `examples/nutrition-label-go`: the
**SG Nutrition Investigator**, ported from an OpenAI Agents SDK demo into a
harness-native Go agent. Given a Singapore food/drink nutrition label — pasted as
**text** or supplied as a **photo** (vision via image input) — it investigates the
product using four tools: `check_sfa_additive` (resolves additives against the
full SFA permitted-additives table and learns name→E-number aliases across runs),
`recall_product` (recalls prior verdicts), `check_hcs` (queries data.gov.sg for
the Healthier Choice Symbol), and `calculate_nutri_grade` (A/B/C/D for beverages).
It carries cross-run memory in a `nutrition_memory.json` file. The agent is backed
by the OpenAI provider pointed at a LiteLLM proxy. This section walks the full path
from source to a streamed verdict.

### 1. Build the three binaries

```bash
go build -o agentd     ./cmd/agentd
go build -o runtimed   ./cmd/runtimed
go build -o runtimectl ./cmd/runtimectl
```

### 2. The config: `runtime.nutrition.yaml`

A single-agent registry that selects the nutrition agent via the `kind` field
(see [Adding your own agent kind](#adding-your-own-agent-kind)):

```yaml
# Single-agent registry for the SG Nutrition Investigator example.
# Run with: RUNTIME_CONFIG=runtime.nutrition.yaml ./runtimed
# Requires env: OPENAI_API_KEY, OPENAI_BASE_URL, OPENAI_MODEL, RUNTIME_PG_DSN.
agents:
  - id: nutrition
    name: SG Nutrition Investigator
    model: openai/gpt-5.4
    kind: nutrition
    listen_addr: 127.0.0.1:8201
```

### 3. Required environment

`runtimed` inherits these and passes them down to the `agentd` subprocess (the
nutrition builder reads `OPENAI_*` and `RUNTIME_NUTRITION_DATA_DIR` from the
subprocess environment; `runtimed` also injects `RUNTIME_PG_DSN`,
`RUNTIME_LISTEN_ADDR`, `RUNTIME_AGENT_ID`, and `RUNTIME_AGENT_KIND` per agent):

```bash
export OPENAI_API_KEY=sk-...                          # your LiteLLM key
export OPENAI_BASE_URL=https://litellm-stg.aip.gov.sg # LiteLLM proxy base URL
export OPENAI_MODEL=gpt-5.4                            # model name on the proxy
export RUNTIME_PG_DSN=postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable
export RUNTIME_CONFIG=runtime.nutrition.yaml          # this config
export RUNTIME_AGENTD_BIN=./agentd                    # the agent subprocess binary
export RUNTIME_NUTRITION_DATA_DIR=./data              # optional; where nutrition_memory.json is written (default ".")
```

### 4. Run the control plane

```bash
./runtimed
# control plane on :8080 hosting 1 agent
# supervising agent "nutrition" at 127.0.0.1:8201
```

`runtimed` supervises the nutrition `agentd` subprocess (restarting it on crash),
gates startup on the agent's `/healthz`, and serves the routed control plane on
`:8080` (override with `RUNTIME_CTL_ADDR`).

### 5. Invoke with text

`runtimectl invoke` POSTs a session and streams its events to completion:

```bash
./runtimectl invoke --agent nutrition \
  "Investigate this label (text): Product: Milo UHT. Ingredients: ... Sugar 6g/100ml, sat fat 1.5g/100ml. Beverage."
# session: ses-…
# data: {"type":"text","text":"…the verdict…"}
# data: {"type":"done"}
```

### 6. Invoke with an image (photo of a label)

The session contract accepts optional `image_b64` / `image_mime` fields on
`POST /sessions`. Base64-encode the photo and POST it, then stream the returned
session id:

```bash
IMG=$(base64 -i label.jpeg)
curl -s localhost:8080/agents/nutrition/sessions \
  -d "{\"message\":\"Investigate this label.\",\"image_b64\":\"$IMG\",\"image_mime\":\"image/jpeg\"}"
# {"session_id":"ses-…"}

# then stream (use the returned session id):
curl -N "localhost:8080/agents/nutrition/sessions/ses-…/stream?since=0"
# id: 1
# data: {"type":"text","text":"…the verdict…"}
# id: 2
# data: {"type":"done"}
```

### 7. Observe sessions

Every session is visible in the read-only web console at
**`http://localhost:8080/ui`** (drill into the `nutrition` agent to see its
sessions and a live SSE view), and from the CLI:

```bash
./runtimectl sessions --agent nutrition
# ses-…   completed   turns=3
```

### How this maps to the platform

- The agent is a **supervised subprocess** (`agentd`, `kind: nutrition`) behind
  the `/agents/nutrition/...` route on the control plane.
- Each session is a **durable DBOS workflow** whose id equals the session id; it
  runs one turn per DBOS step and survives a process restart mid-run, resuming
  from the last completed turn.
- The **posted image is part of the checkpointed workflow input** (it rides on
  the first turn), so a session started from a photo resumes correctly after a
  crash without re-uploading.
- Events stream over **SSE** and are persisted to an append-only log, so a client
  can re-attach and replay with `?since=<seq>`.

### Adding your own agent kind

The nutrition example is selected by `kind: nutrition`. To add your own kind:

1. Implement a `Builder` — a `func(Deps) (agentruntime.Config, error)` — that
   assembles your agent's `agentruntime.Config` (its `AgentSpec`, LLM provider,
   and tool registry). See `examples/nutrition-label-go` (`BuildConfig` in
   `examples/nutrition-label-go/agent.go`) for a complete reference implementation.
2. Register it in the `builders` map in `internal/agentkind/registry.go` under a
   new kind string (e.g. `"mykind"`).
3. Set `kind: mykind` on an agent in your config. The empty/absent kind (and
   `"testagent"`) selects the bundled deterministic test agent. `runtimed` passes
   the kind to the `agentd` subprocess via `RUNTIME_AGENT_KIND`, where the kind
   registry resolves it to your builder.

### Hosting a foreign-SDK agent (Python shim)

Runtime is not limited to Go agents. An agent entry in `runtime.yaml` may set an
optional `command:` (argv) and `workdir:`; when `command` is present,
`runtimed`'s supervisor execs that process — in `workdir`, with
`RUNTIME_LISTEN_ADDR`/`RUNTIME_AGENT_ID` injected and the parent environment
inherited — instead of the bundled `agentd` binary. The control plane still
routes `/agents/{id}/...`, health-gates on `/healthz`, and restarts on crash, so
the foreign process is a first-class supervised agent. The only requirement is
that it speaks the [agent contract](#agent-contract-served-by-each-agentd-reach-it-via-the-proxy-prefix).

`contrib/shims/python/` ships a reusable Python shim that does exactly this: a
framework-agnostic `runtime_contract` library (the six contract endpoints + SSE
+ `?since=N` replay + a SQLite session/event store). A foreign framework is
hosted by writing a thin **adapter** (the `AgentAdapter` protocol — one `run()`
method that drives the SDK and yields contract events) and an entrypoint that
calls `runtime_contract.serve(adapter)`:

```python
from runtime_contract import serve
from adapter import MyFrameworkAdapter

serve(MyFrameworkAdapter)   # reads RUNTIME_* from env; builds the store + app + server
```

`serve()` is the Python analog of `agentruntime.Serve`: it reads the
operator-injected env (`RUNTIME_LISTEN_ADDR`, `RUNTIME_AGENT_ID`, and the
optional `RUNTIME_SHIM_DB`) itself, so the adapter author never handles
deployment parameters — exactly the same separation as the Go SDK. Adding
another framework is one new adapter file.

The worked example is the SG Nutrition Investigator on the OpenAI Agents SDK
(`examples/nutrition-label-openai/`), which boots under `runtimed` via its
Makefile:

```bash
cd examples/nutrition-label-openai
cp .env.example .env          # fill in OPENAI_API_KEY / OPENAI_BASE_URL / OPENAI_MODEL
make run                      # builds binaries, uv sync, runs the control plane

# in another shell — the same conformance gate that validates Go agents:
make conformance              # runtimectl conformance --agent nutrition-openai
make demo-image IMAGE=milo.jpeg   # base64 a label photo → POST → stream the verdict
```

The shim provides **Level-1 durability** (sessions and their event logs persist
in `shim.db`, so after a restart sessions are listable, replayable, and
conversation memory continues) — but **not** Level-2 in-flight crash resume,
which remains the Go agents' DBOS-backed advantage. See
[`contrib/shims/python/README.md`](contrib/shims/python/README.md) for the full
walkthrough, the adapter seam, and standalone-dev instructions.

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

### Docker Compose (full stack)

`deploy/docker-compose.full.yml` runs the whole platform — Postgres + the
control plane — with one command. It builds `runtimed` + `agentd` via
`deploy/Dockerfile` (a multi-stage Go build).

> **Build context:** the `runtime` module depends on `../harness` via a `replace`
> directive, so the Docker build context must contain BOTH `runtime/` and
> `harness/`. The compose file sets `context: ../..` (the projects root). Run it
> from that root:

```bash
# from the directory that contains both runtime/ and harness/
docker compose -f runtime/deploy/docker-compose.full.yml up --build
# control plane on http://localhost:8080  (Postgres stays internal to the compose network)
```

`deploy/docker-compose.yml` (Postgres only) remains for the local-dev workflow
where you run the Go binaries directly.

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
| `RUNTIME_LOG_FORMAT` | runtimed | `text` | `json` switches `slog` to JSON output. |
| `RUNTIME_CTL_URL` | runtimectl | `http://localhost:8080` | Control-plane base URL the CLI targets. |
| `RUNTIME_TOKEN` | runtimectl | (unset) | Bearer credential (service key, OIDC token, or bootstrap key) sent on every CLI request when set. |
| `RUNTIME_OIDC_ISSUER` | runtimed | (unset) | OIDC issuer URL; enables human login. Empty ⇒ OIDC disabled. |
| `RUNTIME_OIDC_CLIENT_ID` | runtimed | (unset) | OIDC client id (expected token audience). |
| `RUNTIME_OIDC_CLIENT_SECRET` | runtimed | (unset) | OIDC client secret for the console code exchange. |
| `RUNTIME_OIDC_REDIRECT_URL` | runtimed | `http://localhost:8080/ui/callback` | OIDC redirect URL registered with the issuer. |
| `RUNTIME_ADMIN_BOOTSTRAP` | runtimed | (unset) | One-time break-glass superuser service key (read from env, never stored). |
| `RUNTIME_SECRETS_KEY` | runtimed | (unset) | base64 of 32 bytes; enables per-tenant secrets brokering. Unset ⇒ disabled (agents inherit operator env); malformed ⇒ fatal. |
| `RUNTIME_SHIM_DB` | (python shim) | `./shim.db` | SQLite path for the polyglot shim's Level-1 store (not used by runtimed). |

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

Integration tests cover the platform's headline guarantees, including:

- **`TestResumeAfterKill`** — starts a real `agentd`, kills it mid-turn,
  restarts it, and asserts the session resumes via DBOS recovery and completes
  (demonstrating at-least-once tool semantics honestly).
- **`TestMultiAgentRouting`** — starts `runtimed` with a two-agent config,
  invokes a session on each agent through the router, and asserts both complete,
  sessions are isolated per agent, and status/turn_count are tracked.
- **`TestIdentityE2E_TwoTenants`** — two tenants each with a scoped service key:
  a tenant's key reaches its own agent (read + invoke), gets `404` on the other
  tenant's agent, a viewer is `403` on invoke, no credential is `401`, and a
  revoked key is `401`.
- **`TestSecretsE2E_PerTenantInjection`** — proves the whole secrets chain: a
  real Cipher+Broker over Postgres, three tenants (two with their own
  `OPENAI_API_KEY`, one with none), spawned via the `command:` path. Asserts each
  tenant's agent process sees its own secret value, no cross-tenant leak, and the
  no-secret tenant falls back to the inherited operator env.

---

## Project layout

![Runtime project layout: top-level packages under runtime/ and their responsibilities.](docs/images/project-layout.png)

---

## Status, scope & limitations

Runtime is built in milestones. **Milestone 1** delivered the durable single-agent
spine; **Milestone 2** added the multi-agent platform (config-driven registry,
path routing, per-agent supervision, session status, full CLI); **Milestone 3**
added the operability layer (the read-only web console, structured logging, the
contract conformance suite, bounded shutdown, 503-on-restart, per-agent health,
full-stack Docker build). Since then: **polyglot agent hosting** (host foreign-SDK
agents via the contract — see the Python shim) and the **Identity** first
milestone (multi-tenant access control, below).

**Deliberately not yet implemented** (each is planned, scoped to a later
milestone or sub-project):

- **Identity — first two milestones DONE** (see [Authentication & multi-tenancy](#authentication--multi-tenancy)):
  M1 multi-tenant access control with OIDC human login, bcrypt-hashed service keys
  (constant-time verify), and per-agent admin/operator/viewer roles; M2 per-tenant
  **secrets brokering** (AES-256-GCM at rest, injected into the tenant's agents at
  spawn). **Still to come:** secrets key rotation, fine-grained/custom RBAC,
  cross-tenant users + self-service, an admin console UI, optional local password
  accounts, and console CSRF (`state`/`nonce`) hardening.
- **Observability dashboards** — M3 has structured `slog` logs; metrics,
  tracing, and dashboards are the Observability sub-project.
- **Write actions from the console** — the console is read-only; deploy/stop/
  invoke from the UI is future work.
- **Subprocess pools / autoscaling** — one subprocess per agent today; no
  per-agent replicas or load-based scaling.
- **Dynamic deploy** — agents come from `runtime.yaml` at startup; no runtime
  `POST /agents` registration or rollback yet (tokens are config-only too).
- **Sandboxes** (isolated browser / code-interpreter tools), a **tool/MCP
  gateway**, and **managed memory** — each its own sub-project.
- **Containers / Kubernetes** — the agent contract is designed to admit
  containerized agents later (the conformance suite already validates them);
  today agents are local subprocesses.
- **Cross-agent aggregate views** — session listing is per-agent; a fleet-wide
  session view is future console work.
- **Minor hardening**: `session_events` sequence allocation assumes one writer
  per session (true today, since one subprocess owns a session); the console
  cookie is not `Secure` (terminate TLS upstream).

See `docs/superpowers/specs/` for the full design and milestone specs.

## License

See the workspace license.
