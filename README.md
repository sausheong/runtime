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
- [MCP Gateway](#mcp-gateway)
- [Code-interpreter sandbox](#code-interpreter-sandbox)
- [Browser sandbox](#browser-sandbox)
- [Observability](#observability)
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
| **MCP gateway** | A central `/gateway/mcp` endpoint: MCP federation + semantic tool search + REST/OpenAPI adapters. Stdio, Streamable HTTP, and plain REST upstreams (one tool per OpenAPI operation), namespaced, tenant-filtered, discoverable by embedding-ranked search. |
| **Code-interpreter sandbox** | An isolated, stateful Python + shell execution environment per session — one locked-down Docker container (no network, read-only rootfs, resource limits), tenant-scoped, delivered to every agent through the MCP gateway. |
| **Observability** | One Prometheus `/metrics` endpoint for the whole fleet (control plane + every agent, merged), `X-Request-ID` correlation end-to-end through proxy, logs, and durable workflow, and a bundled Prometheus + Grafana compose overlay with a provisioned dashboard. |
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

Managing secrets requires **identity to be configured** (OIDC, a service key, or
`RUNTIME_ADMIN_BOOTSTRAP`): the `/admin/secrets` API is part of the admin surface,
which is not mounted in open mode. With a master key set but identity open,
runtimed logs a warning and brokering into spawns still works, but no secret can
be created. A tenant admin manages its own tenant's secrets; a superuser can
`set` a secret for a target tenant via `--tenant`, but `ls`/`rm` are scoped to
the caller's own tenant (cross-tenant `ls`/`rm` is tracked in `ROADMAP.md` §B3).

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

> **Security:** the keyring keys live in runtimed's environment (operator-managed,
> like the Postgres DSN). Losing **all** keys makes existing ciphertext
> unrecoverable. The `set` value travels as JSON, so terminate TLS upstream; it
> also lands in shell history, so prefer a leading-space invocation
> (`HISTCONTROL=ignorespace`) for real keys. A `--value-stdin` flag is a candidate
> follow-up.

#### Key rotation

The secrets master key is a **keyring** of one or more 32-byte keys, one
designated primary. New secrets are sealed under the primary; each stored blob is
self-describing (it carries the id of the key that sealed it) and is bound to its
`(tenant, name)` so a ciphertext cannot be swapped to another row. Configure the
keyring with:

- `RUNTIME_SECRETS_KEYS` — `id:base64key,id:base64key` (each key is base64 of 32
  bytes; ids are operator-chosen, e.g. `v1`, `2026q2`).
- `RUNTIME_SECRETS_PRIMARY` — the id new secrets are sealed under (required when
  `RUNTIME_SECRETS_KEYS` is set).
- `RUNTIME_SECRETS_KEY` — the legacy single key (still honored). On its own it is
  the keyring `{v1: key}`. Alongside `RUNTIME_SECRETS_KEYS` it names the key that
  decrypts pre-rotation (version-less) rows; its bytes must match a keyring entry.

To rotate:

```bash
# 1. Add a new key, make it primary, keep the old key in the ring, restart runtimed.
export RUNTIME_SECRETS_KEYS="v1:$OLD_B64,v2:$NEW_B64"
export RUNTIME_SECRETS_PRIMARY=v2
# 2. Re-encrypt the backlog (superuser: all tenants; or --tenant <t>).
runtimectl admin secret rotate
# 3. Once rotate reports 0 failures, drop the old key and restart:
export RUNTIME_SECRETS_KEYS="v2:$NEW_B64"
```

`rotate` is idempotent and re-runnable, and exits non-zero if any row failed.
Legacy single-key (`RUNTIME_SECRETS_KEY`-only) deployments keep working unchanged;
their rows decrypt transparently until rotated. Base64 uses standard encoding
(not URL-safe).

### Per-tenant agent memory

An agent can opt into durable, per-tenant memory: set `memory: true` on its
`runtime.yaml` entry and it gets harness's `memory` tool
(`save`/`update`/`remove`/`list`/`get`), backed by Postgres. All agents owned by
a tenant share one memory pool; a tenant can never read another tenant's memory.
Entries survive restarts and are shared across the tenant's agents.

```yaml
agents:
  - id: assistant
    name: Assistant
    model: anthropic/claude-sonnet-4-6
    listen_addr: "127.0.0.1:8081"
    tenant: acme
    memory: true        # opt in to durable per-tenant memory
```

Retrieval in this milestone is by tag and by id (the durable store). Semantic
recall (embeddings + vector search) is a planned follow-up via harness's
`KnowledgeGraph` seam. Memory is disabled by default; agents without the flag are
unaffected. The platform injects `RUNTIME_AGENT_TENANT` (and, when enabled,
`RUNTIME_AGENT_MEMORY=1`) into the agent subprocess; agentd constructs a
tenant-pinned store so memory is isolated by construction.

#### Semantic recall

When embeddings are configured, a memory-enabled agent also gets **automatic
semantic recall**: each saved entry is embedded, and at the start of every turn
the most similar past memories are retrieved and injected into the prompt — no
agent code, no explicit lookup. Recall is tenant-isolated (same boundary as the
store) and best-effort (a slow or failing embedding service never breaks a turn).

Enable it by pointing the platform at an OpenAI-compatible embeddings endpoint
(the same proxy used for chat) and choosing a model + dimension:

```bash
export RUNTIME_EMBED_MODEL=text-embedding-3-small
export RUNTIME_EMBED_DIM=1536          # must match the model's output dimension
# reuses OPENAI_BASE_URL / OPENAI_API_KEY
# optional tuning:
export RUNTIME_EMBED_RECALL_K=5         # max memories injected per turn (default 5)
export RUNTIME_EMBED_RECALL_FLOOR=0.25  # min cosine similarity to inject (default 0.25)
```

**Tuning the recall floor per embedding model.** `RUNTIME_EMBED_RECALL_FLOOR`
is the minimum cosine similarity a stored memory must have to the user's query
to be injected. The right value depends heavily on the embedding model, because
a *question* and the *declarative fact* it should recall are worded very
differently and so score lower than two similar facts would. Measured against
`text-embedding-3-small`, a relevant query→memory pair scores only ~0.25–0.40,
while unrelated text sits near 0 — so the default is **0.25**. Guidance:

| Embedding family | Suggested `RECALL_FLOOR` | Notes |
|---|---|---|
| OpenAI `text-embedding-3-small` / `-3-large`, `ada-002` | **0.20–0.35** (default 0.25) | Question↔fact pairs rarely exceed ~0.4; 0.7 silently suppresses all recall. |
| Cohere `embed-v3`, Gemini `gemini-embedding-001` | start ~0.3, calibrate | Similar low question↔fact range; measure on your data. |
| Normalized / symmetric-similarity models | 0.5–0.7 | If your model reports high cosines for related pairs, raise the floor. |

If recall never returns anything, your floor is almost certainly too high:
embed a representative query and a memory it should match, compute their cosine,
and set the floor a little below that. Too low injects marginally-related
memories (noise); too high injects nothing.

**Postgres prerequisite:** semantic recall needs the pgvector extension. The
deploy image (`pgvector/pgvector:pg16`) ships it, but the extension must be
**created once by a Postgres superuser** in each target database
(`CREATE EXTENSION IF NOT EXISTS vector;`) — the unprivileged `runtime` role
cannot create it. If it is missing, an agent with embeddings enabled fails to
start (the store's DDL errors). With the extension present, startup is idempotent.

If `RUNTIME_EMBED_MODEL` is unset, memory works exactly as before (tag/id
retrieval, no recall). If an embedding call fails on save, the entry is still
stored (durable) but is invisible to recall until re-embedded (e.g. on its next
update). Changing the embedding model/dimension requires re-embedding (a
documented migration).

#### Auto-ingestion

When semantic recall is enabled **and** `RUNTIME_INGEST_ENABLED` is set, the
agent also *captures* memories automatically. After each completed chat exchange,
a background extractor reads the conversation, pulls out durable facts, dedups them against
existing memory, and saves the new ones — which embed-on-save makes recallable on
the next turn. The agent does not have to call the memory tool to remember.

```bash
export RUNTIME_INGEST_ENABLED=1
export RUNTIME_INGEST_MODEL=gpt-4o-mini   # chat model for fact extraction
# reuses OPENAI_BASE_URL / OPENAI_API_KEY
# optional tuning:
export RUNTIME_INGEST_MIN_MESSAGES=2      # growth gate: min thread messages (default 2)
export RUNTIME_INGEST_MAX_INFLIGHT=4      # max concurrent extractions, drop over (default 4)
export RUNTIME_INGEST_DEDUP_FLOOR=0.85    # skip a fact this similar to an existing one (default 0.85)
export RUNTIME_INGEST_MAX_FACTS=10        # cap on facts saved per turn (default 10)
```

It is best-effort and never affects a turn: extraction runs in a bounded
background goroutine after the response is delivered, and any failure
(extraction, embedding, save) degrades silently. A trivial turn is skipped by a
cheap message-count gate; when too many extractions are already in flight, a
turn's ingest is dropped rather than queued. Auto-captured entries carry origin
`ingest` and the `auto` tag, distinguishing them from tool-saved memories.

Ingestion is per-turn (no whole-session synthesis) and append-or-skip (a
near-duplicate is skipped, not merged). It requires semantic recall: if
`RUNTIME_INGEST_ENABLED` is set without embeddings configured, it is ignored
(with a warning); if set with embeddings but no `RUNTIME_INGEST_MODEL`, agent
startup fails. Conversation content is sent to the same proxy used for chat and
embeddings — no new egress.

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
> rest. Per-tenant secrets brokering and key rotation are implemented (see above).
> Console session CSRF hardening (`state`/`nonce`) is tracked for later
> Identity milestones (see `ROADMAP.md` §B3).

---

## MCP Gateway

The platform can host a **central MCP endpoint** — `/gateway/mcp`, speaking
Streamable HTTP — that **federates upstream MCP servers** behind one URL. An
upstream is a **stdio** server (a `command` the platform spawns), a remote
**Streamable HTTP** server (a `url`), or a plain **REST API** described by an
OpenAPI document (an `openapi:` spec — see
[REST / OpenAPI upstreams](#rest--openapi-upstreams)). Each upstream's tools
are namespaced as `<server>__<tool>` on the gateway; an agent consuming them
through the gateway sees them as `mcp__gateway__<server>__<tool>`. Operators
configure tools once, centrally, instead of wiring the same MCP servers into
every agent.

### Configuration

Add an optional top-level `gateway:` section to `runtime.yaml`:

```yaml
gateway:
  self_url: http://127.0.0.1:8080      # base URL agents use; default derives from RUNTIME_CTL_ADDR
  agent_keys:                          # tenant → service key for gateway:true agents (identity on)
    default: ${GW_DEFAULT_KEY}
  servers:
    - name: fs                         # tools exposed as fs__<tool> (agents see mcp__gateway__fs__<tool>)
      command: npx
      args: ["-y", "@modelcontextprotocol/server-filesystem", "/data"]
    - name: search
      url: https://mcp.example.com/mcp
      headers: {Authorization: "Bearer ${SEARCH_TOKEN}"}
      tenants: [acme]                  # omit ⇒ visible to all tenants
```

Each server requires a unique `name` and **exactly one** of `command` (stdio;
with optional `args` and `env`), `url` (Streamable HTTP; with optional
`headers` for auth), or `openapi` (a REST API described by an OpenAPI 3.x
document — see [REST / OpenAPI upstreams](#rest--openapi-upstreams)).
`headers`, `env`, `agent_keys`, `openapi`, and `base_url` values support
`${VAR}` expansion from the operator environment at load time, so secrets stay
out of the YAML file; referencing an unset/empty variable is a fatal config
error. Values may not contain a literal `$` — put such values in an env var
and reference them as `${VAR}`.

### Authentication and tenancy

When identity is configured, gateway callers authenticate like any other
machine client: `Authorization: Bearer svk-…` (a platform service key). With
no identity configured, the gateway — like the rest of the control plane —
runs in **open mode**. MCP sessions are **principal-bound per call**: every
`tools/call` is re-checked against the live request's principal, so a session
id replayed by a different principal is rejected rather than inheriting the
creator's view.

Tenancy follows the platform model:

- A per-upstream `tenants:` allowlist scopes that upstream's tools to those
  tenants (absent ⇒ visible to all tenants). A non-visible upstream's tools
  are **absent from `tools/list`**, and calling one returns the MCP
  tool-not-found error — existence is hidden, mirroring the `404`-not-`403`
  rule elsewhere.
- **Role gate:** any authenticated principal can list tools; *calling* a tool
  requires `operator` or `admin`.
- `GET /gateway/status` reports per-upstream connection state, scoped to the
  caller's tenant; a `viewer` gets `403`.

### Agent opt-in (`gateway: true`)

An agent consumes the gateway by setting `gateway: true` on its `runtime.yaml`
entry:

```yaml
agents:
  - id: assistant
    name: Assistant
    model: anthropic/claude-sonnet-4-6
    listen_addr: 127.0.0.1:8081
    tenant: acme
    gateway: true       # opt in to the platform MCP gateway
```

The platform injects `RUNTIME_GATEWAY_URL` (and, when identity is on,
`RUNTIME_GATEWAY_KEY` — the `agent_keys` entry for the agent's tenant) into
the agent subprocess, which connects to the gateway like any other MCP server.
Foreign (shim) agents get the same env vars. Startup is **fail-closed**: when
identity is on and a `gateway: true` agent's tenant has no `agent_keys` entry,
`runtimed` refuses to start rather than spawn an agent that cannot
authenticate.

### Search mode

When the federated catalog is large, listing every tool in every agent prompt
gets expensive. In **search mode** a consumer's `tools/list` contains exactly
one tool — `search_tools`. The agent describes what it needs ("read a file's
contents") and gets back the matching tools — name, description, full input
schema, and a relevance score — ranked by embedding cosine similarity. Any
returned tool is callable directly by name: the whole (tenant-scoped) catalog
stays callable, it's just unlisted.

Enable it per agent with `gateway: search` in `runtime.yaml`:

```yaml
agents:
  - id: assistant
    gateway: search     # list only search_tools; discover the rest by query
```

The platform appends `?mode=search` to the injected `RUNTIME_GATEWAY_URL`.
External MCP clients opt in the same way: connect to `/gateway/mcp?mode=search`.

**Requirements.** Search mode needs platform embeddings — the same envs as
Memory: `RUNTIME_EMBED_MODEL`/`RUNTIME_EMBED_DIM` plus
`OPENAI_BASE_URL`/`OPENAI_API_KEY`. This is fail-fast: `runtimed` refuses to
start if any `gateway: search` agent exists without embeddings configured,
and a session requesting `?mode=search` on a gateway without embeddings gets
HTTP 400.

**`search_tools` contract.** Input: `{"query": string}` (required) and
optional `k` (default 5, capped at 20). Output: a JSON array of matches; zero
matches returns an empty array plus a "try a broader query" hint.

**Tunables.**

| Variable | Default | Meaning |
|---|---|---|
| `RUNTIME_GATEWAY_SEARCH_FLOOR` | `0.2` | minimum cosine similarity for a match |
| `RUNTIME_GATEWAY_SEARCH_K` | `5` | default result count when `k` is omitted |

The floor is low on purpose: question↔description cosine similarity runs
around 0.25–0.40 on OpenAI-family embeddings — the same calibration lesson as
the memory recall floor.

**Notes.** Viewers can search (it's a read, like `tools/list`) but still
cannot call tools. Tool embeddings are computed lazily on the first search
after an upstream (re)connects — one embed call per distinct tool text per
process. Embed failures degrade rather than break: a tool whose embedding
fails is skipped from search results but remains callable by name, and a
query-embed failure returns an `isError` "search temporarily unavailable"
result.

### REST / OpenAPI upstreams

The gateway can also federate a **plain REST API** — no MCP server required.
Point a server entry at an OpenAPI 3.x document and every selected operation
becomes an ordinary federated tool: named, tenant-filtered, searchable
(search mode indexes generated tools automatically), metered, and callable by
any `gateway: true|search` agent with zero agent-side changes.

```yaml
gateway:
  servers:
    - name: orders
      openapi: http://orders.internal:9000/openapi.yaml  # file path or URL
      base_url: http://orders.internal:9000   # optional; default = first spec servers[] entry
      headers: {Authorization: "Bearer ${ORDERS_TOKEN}"}  # ${VAR} expansion as usual
      operations: ["listOrders", "GET /orders/*"]  # optional allowlist; omit ⇒ all
      tenants: [acme]                          # tenancy works as for any upstream
```

`base_url` and `operations:` are valid only with `openapi:`; `operations:`
entries are bare operationIds or `METHOD /path-glob` patterns.
`forward_tenant: true` remains stdio-only.

**Tool generation.** One tool per selected operation, named
`<server>__<operationId>` (when an operation has no `operationId`, a
`method_path` slug is used; `__` inside a name collapses to `_` — it is the
reserved gateway separator). Descriptions are `"METHOD /path — summary"`
(capped at 1024 chars). The input schema is one object merging path parameters
(required), query parameters, spec-declared headers (prefixed `header_`), and
the JSON request body under a `body` property. Component `$ref`s are
**deep-inlined** into plain JSON Schema; an operation with a genuinely cyclic
schema is skipped with a WARN, and a spec using external (cross-file) `$ref`s
fails at dial — security posture. Operations whose *required* request body
has no JSON media type are skipped (an optional non-JSON body is just
dropped). A spec generating more than 50 tools logs a WARN nudging you toward
`operations:`; a filter matching zero operations connects with 0 tools plus a
WARN (an operator typo should be visible, not fatal).

**Calling a tool.** Every HTTP exchange comes back as one JSON envelope —
HTTP 4xx/5xx are *results* the agent can reason about, not tool errors:

```json
{"status": 404, "headers": {"content-type": "application/json"},
 "body": {"error": "no such order"}, "truncated": false}
```

`body` is parsed JSON when the response is JSON, otherwise the raw string;
responses are capped at 1 MiB with `"truncated": true` beyond that. Requests
time out at 30s. Tool errors are reserved for input validation failures,
traversal/header-override rejections, transport failures, and timeouts.

**Security posture.**

- The agent controls only parameter *values* — never host, scheme, or path
  structure. Path-parameter values containing `/` or `..` (including encoded
  forms) are rejected.
- Configured headers are inviolable: a `header_*` argument that matches a
  configured header (case-insensitive) is rejected — an agent can never
  override `Authorization`.
- Redirects are followed same-host only (max 3), for API calls **and** the
  spec fetch — a compromised upstream cannot bounce gateway credentials to
  another host.
- Liveness: `HEAD base_url` (GET fallback on 405); *any* HTTP response means
  alive — only transport errors mark the upstream down. Reconnect re-fetches
  the spec, so spec drift heals on the next redial.

**Limitations (this milestone):** JSON request bodies only (a required
form/multipart body skips the operation); arrays serialize comma-joined (no
`explode`); credentials are shared per upstream (per-tenant credentials need
secrets-broker integration); OpenAPI 3.x only (no Swagger 2.0 conversion); no
OAuth2 client-credentials flow.

A runnable example lives in `examples/rest-demo/` — a tiny orders API that
serves its own OpenAPI spec, with a README walking through federating it.

### Failure model

Startup never blocks on upstreams: `runtimed` comes up immediately and each
upstream is dialed asynchronously, with a per-upstream reconnect loop (capped
backoff) owning its lifecycle. A call against a down upstream returns an MCP
`isError` result rather than breaking the gateway session. `GET
/gateway/status` shows the live per-upstream state.

### Limitations

- **Static config only** — upstreams come from `runtime.yaml` at startup; no
  dynamic registration.
- **Tools only** — no resources or prompts federation yet.
- **REST upstreams: JSON bodies, shared credentials** — see
  [REST / OpenAPI upstreams](#rest--openapi-upstreams) for the per-milestone
  limitations (JSON-only request bodies, comma-joined arrays, per-upstream
  shared credentials, OpenAPI 3.x only, no OAuth2 flow).
- **Operator-managed agent keys** — `agent_keys` maps tenants to service keys
  by hand; no automatic key minting for `gateway: true` agents.
- **`${VAR}` expansion required for values containing `$`** — there is no
  escape for a literal `$` in `headers`/`env`/`agent_keys`/`openapi`/`base_url`
  values.

---

## Code-interpreter sandbox

The platform ships an isolated **code interpreter** any agent can use: a
`sandboxd` binary that manages one locked-down Docker container per sandbox
session and exposes it as MCP tools. It is delivered **through the gateway** —
declare it as an ordinary upstream and every gateway-enabled agent (Go or
foreign-SDK shim, `gateway: true` or `search`) sees the tools as
`mcp__gateway__sandbox__<tool>`, with **zero agent-side changes**.

```yaml
gateway:
  servers:
    - name: sandbox
      command: bin/sandboxd
      forward_tenant: true       # the gateway injects the caller's tenant (see below)
      tenants: [acme]            # optional visibility filter, as for any upstream
```

Build the bundled container image once (`python:3.12-slim` + numpy / pandas /
matplotlib / requests, non-root user):

```bash
make sandbox-image     # builds runtime-sandbox:latest (override: RUNTIME_SANDBOX_IMAGE)
```

### The tools

| Tool | What it does |
|---|---|
| `create_sandbox` | Start an isolated session → `{sandbox_id, expires_at}` |
| `execute_code` | Run Python (`python3 -c`) in `/workspace` → stdout/stderr/exit code |
| `run_command` | Run a shell command (`sh -c`) — same limits |
| `write_file` / `read_file` | Move text in and out of `/workspace` (reads capped at 256 KiB) |
| `list_sandboxes` / `close_sandbox` | Lifecycle — list is tenant-scoped; close is idempotent |

Sessions are **stateful**: files in `/workspace` persist across calls within a
sandbox (write a CSV, then run pandas over it, then read the result), but
**Python variables do not** — each execution is a fresh interpreter process.
The tool descriptions tell the model this, so it writes intermediate results
to files.

### Isolation posture

Every sandbox container runs with: **no network** (`network=none`, always — a
`requests.get` inside the sandbox fails), read-only root filesystem, tmpfs
`/workspace` (64 MiB default) and `/tmp`, all capabilities dropped,
`no-new-privileges`, a non-root user, and CPU / memory / pid limits.
Executions are wrapped in a wall-clock timeout (default 30s, max 120s) that
kills the runaway process — never the session. On Linux hosts,
`RUNTIME_SANDBOX_RUNTIME=runsc` runs sandboxes under gVisor with no other
changes.

Lifecycle is bounded: an idle reaper closes sandboxes unused past
`RUNTIME_SANDBOX_IDLE_TTL` (default 10m) or older than
`RUNTIME_SANDBOX_MAX_LIFETIME` (default 1h), each tenant is capped at
`RUNTIME_SANDBOX_MAX_PER_TENANT` (default 5) concurrent sandboxes, and on
startup `sandboxd` removes any containers left over from a previous run
(label `runtime.sandbox=1`). Run exactly one `sandboxd` per host (or per
`DOCKER_HOST`) — startup reaping removes *all* labeled containers.

### Tenancy (`forward_tenant`)

MCP carries no caller identity, so the gateway forwards it: with
`forward_tenant: true` on a **stdio** upstream (config-validated — it is
rejected on `url:` upstreams), the gateway strips any caller-supplied
`__rt_tenant` argument and injects the authenticated principal's tenant into
every forwarded call. An agent can never choose its own tenant. sandboxd then
scopes everything by that tenant: another tenant's `sandbox_id` returns the
same "no such sandbox" as a nonexistent one (existence hidden, like the
cross-tenant `404` rule elsewhere), and `list_sandboxes` shows only the
caller's.

sandboxd **fails closed** when the tenant key is absent — every tool returns
an error telling the operator to set `forward_tenant: true` — so forgetting
the flag cannot silently collapse all tenants into one namespace. For
single-tenant direct use (no gateway), set `RUNTIME_SANDBOX_ALLOW_DIRECT=1`.

### Failure model

Mirrors the gateway's degrade-don't-fail: if the Docker daemon is unreachable,
sandboxd still serves MCP and `create_sandbox` returns a "backend unavailable"
tool error (recovering as soon as the daemon appears); if a container dies
mid-session, calls on that sandbox error and the agent creates a new one; if
sandboxd itself crashes, the gateway's reconnect loop restarts it and startup
reaping clears orphans.

### Configuration reference

| Variable | Default | Meaning |
|---|---|---|
| `RUNTIME_SANDBOX_IMAGE` | `runtime-sandbox:latest` | container image |
| `RUNTIME_SANDBOX_MAX_PER_TENANT` | `5` | concurrent sandboxes per tenant |
| `RUNTIME_SANDBOX_IDLE_TTL` | `10m` | close after this long unused |
| `RUNTIME_SANDBOX_MAX_LIFETIME` | `1h` | hard close after create |
| `RUNTIME_SANDBOX_WORKSPACE_MB` | `64` | tmpfs `/workspace` size |
| `RUNTIME_SANDBOX_MEM_MB` | `512` | memory limit |
| `RUNTIME_SANDBOX_CPUS` | `1.0` | CPU limit |
| `RUNTIME_SANDBOX_RUNTIME` | (engine default) | e.g. `runsc` for gVisor |
| `RUNTIME_SANDBOX_ALLOW_DIRECT` | unset | `1` ⇒ serve without gateway tenant (single-tenant) |

### Limitations

- **Python variables don't persist across calls** — files do; a kernel-mode
  backend (variables persist) is the planned upgrade, same tool surface.
- **No network egress, ever** — `pip install` at runtime doesn't work; bake
  packages into the image instead. An egress-policy milestone may relax this.
- **Docker required** — the engine socket (or `DOCKER_HOST`) must be reachable
  from `sandboxd`; gVisor is optional hardening on Linux.
- **One sandboxd per host** — startup reaping is host-global by label.

---

## Browser sandbox

Alongside the code interpreter, the platform ships an isolated **browser** any
agent can drive: a `browserd` binary that manages one locked-down Chromium
container per browser session and exposes it as MCP tools. Like sandboxd it is
delivered **through the gateway** — declare it as an ordinary `forward_tenant`
upstream and every gateway-enabled agent (`gateway: true` or `search`) sees the
tools as `mcp__gateway__browser__<tool>`, with **zero agent-side changes**.

```yaml
gateway:
  servers:
    - name: browser
      command: bin/browserd
      forward_tenant: true              # gateway injects the caller's tenant
      env:
        RUNTIME_BROWSER_EGRESS_MODE: allow-list
        RUNTIME_BROWSER_EGRESS_ALLOW: "*.example.com,docs.python.org"
```

Build the bundled Chromium image once:

```bash
make browser-image     # builds runtime-browser:latest (override: RUNTIME_BROWSER_IMAGE)
```

### The tools

`create_browser`, `navigate`, `click`, `type`, `get_text`, `extract`,
`screenshot` (returns image content, passed through the gateway's existing
image-content support), `evaluate`, `list_browsers`, `close_browser`. Sessions
are stateful — a `browser_id` from `create_browser` carries page state across
calls until closed or reaped.

### Network egress policy

Unlike the code interpreter (which has *no* network at all), the browser needs
to reach the web — so Chrome's entire network stack is forced through a
browserd-run HTTP/HTTPS proxy via `--proxy-server` (the agent drives Chrome only
over CDP, so the proxy adjudicates all of the agent's reachable traffic), which
allows or denies by hostname:

| Variable | Default | Meaning |
|---|---|---|
| `RUNTIME_BROWSER_EGRESS_MODE` | `deny-all` | `deny-all` \| `allow-list` \| `allow-all-public` |
| `RUNTIME_BROWSER_EGRESS_ALLOW` | (empty) | comma-separated hostname globs for `allow-list` |

The default is **deny-all** (fail closed). Because the proxy sees subresources,
fetch, redirects, and CONNECT — not just the top-level URL — it filters where
DNS/iptables rules cannot.

### Security posture

The proxy enforces the hostname allow/deny decision over all of Chrome's traffic
(the agent drives Chrome only via CDP), and **internal/private addresses are
always blocked** in every mode (with DNS-rebind defense: hostnames are resolved
and the resolved IP re-checked). The container sits on a docker bridge so it can
reach the proxy; a network-level egress boundary (internal network / iptables) so
that even a non-proxy-respecting in-container process is contained is follow-on
hardening. The container itself is locked down like the code-interpreter
sandbox — read-only rootfs, all capabilities dropped, `no-new-privileges`,
non-root user, CPU/memory/pid limits, optional gVisor via
`RUNTIME_BROWSER_RUNTIME=runsc`. Tenancy and lifecycle mirror sandboxd:
`forward_tenant` spoof-proofing, existence-hiding cross-tenant lookup, idle-TTL
+ max-lifetime reaper, per-tenant cap, and reap-on-start by label
`runtime.browser=1` (one browserd per host).

### Testing

```bash
go test ./internal/browser/...              # hermetic unit tests (fake backend)
go test -tags live ./internal/browser/...   # real-Chrome test: needs `make browser-image` + Docker
go test -tags integration ./test/ -run TestGatewayBrowserE2E   # through-serve e2e: needs Postgres
```

---

## Observability

The platform exposes **one Prometheus endpoint for the whole fleet**: scrape
`runtimed`'s `/metrics` and you get the control plane's own series plus every
supervised agent's series, merged into a single valid exposition.

```bash
curl -s localhost:8080/metrics | grep -E '^(runtime_|agent_)' | head
# runtime_agent_up{agent="support"} 1
# runtime_http_requests_total{method="POST",route="/agents/{id}/...",status="200"} 4
# agent_turns_total{agent="support",outcome="completed"} 7
# agent_tokens_total{agent="support",direction="input"} 18342
```

### Metrics inventory

**Control-plane metrics** (owned by `runtimed`; the `runtime_*` namespace is
reserved for it):

| Metric | Labels | Meaning |
|---|---|---|
| `runtime_http_requests_total` | `route,method,status` | Control-plane HTTP requests, by **matched route pattern** (never raw paths). Requests rejected by the identity middleware count under `route="auth_rejected"`. |
| `runtime_http_request_duration_seconds` | `route,method` | Control-plane HTTP latency histogram. |
| `runtime_agent_up` | `agent` | 1 when the agent's `/metrics` was reachable on the last fan-out scrape (a 404 — no metrics endpoint — still counts as serving). |
| `runtime_agent_restarts_total` | `agent` | Supervisor respawns per agent. |
| `runtime_proxy_errors_total` | `agent` | Reverse-proxy failures (503s served). Client-initiated cancellations are not counted. |
| `runtime_gateway_tool_calls_total` | `server,tool,outcome` | Federated gateway tool calls that reached the upstream (authz rejections are not counted). |
| `runtime_gateway_tool_call_duration_seconds` | `server` | Gateway tool-call latency histogram. |
| `runtime_gateway_upstream_up` | `server` | 1 when the gateway upstream connection is up. |
| `runtime_metrics_scrape_skips_total` | `agent,reason` | Agents skipped during the fan-out scrape. Reasons: `timeout`, `unreachable`, `parse`, `no_metrics`, `status_<code>`, `reserved_name`, `type_conflict`. |

**Agent metrics** (emitted per `agentd`, merged into `runtimed`'s exposition):

| Metric | Labels | Meaning |
|---|---|---|
| `agent_turns_total` | `agent,outcome` | Turns by outcome (`completed`/`error`/`aborted`/`continue`). |
| `agent_turn_duration_seconds` | `agent` | Turn wall time (buckets sized for LLM turns: 0.1s–120s). |
| `agent_tokens_total` | `agent,direction` | LLM tokens by direction (`input`/`output`/`cache_creation`/`cache_read`). |
| `agent_tool_calls_total` | `agent,tool` | Tool calls dispatched by the agent loop. |

### Fan-out, auth, and cardinality

`GET /metrics` is **auth-free**, like `/healthz` — it sits outside the identity
middleware so a Prometheus scraper needs no service key. That is safe because
every label value is an **operator-level identifier** (agent ids, gateway
server names, tool names, route patterns) — never tenant, session, or user
data. That is the **cardinality promise**: no per-tenant / per-session /
per-user labels exist anywhere in the exposition, and adding one is a spec
change, not a casual edit. On each scrape, `runtimed` fans out to every
supervised agent's `/metrics` concurrently (500ms cap per agent), parses each
exposition, and merges families by name with **server-enforced agent labels**:
the registered agent id is injected/overwritten on every scraped series, so a
lying agent cannot spoof another agent's series or inject `runtime_*` families
(those are dropped as `reserved_name`). An agent's `/metrics` is **optional**
— a foreign-SDK shim that doesn't serve it is skipped (reason `no_metrics`)
without being marked down.

### Request correlation (`X-Request-ID`)

Every control-plane request gets a correlation id: a valid inbound
`X-Request-ID` (chars `[A-Za-z0-9._-]`, ≤128 long) is accepted, anything else
is replaced with a generated `req-<32 hex>`. The id is echoed on the response,
forwarded through the reverse proxy to the agent, appears in the `slog` lines
on **both** sides (control-plane access log, per-turn `turn` lines, and
turn-failure warnings), and is checkpointed in the DBOS workflow input — so a
resumed session logs the same id it started with (replay-safe). To capture it
from the CLI:

```bash
runtimectl invoke -v --agent support "hello"
# request-id: req-3f9c…        (stderr)
```

### Prometheus + Grafana overlay

`deploy/docker-compose.obs.yml` adds Prometheus and Grafana (with a
provisioned **Runtime Overview** dashboard — 12 panels covering fleet health,
HTTP traffic, turns, tokens, tools, and the gateway) on top of the base
compose file:

```bash
docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.obs.yml up -d
# Grafana:    http://localhost:3000   (anonymous viewer enabled)
# Prometheus: http://localhost:9090
```

The bundled `deploy/prometheus.yml` targets `host.docker.internal:8080`, which
assumes `runtimed` runs on the host (the local-dev workflow). When using the
full-stack compose (`deploy/docker-compose.full.yml`, `runtimed` in Docker),
edit `deploy/prometheus.yml` to target the `runtimed` service name instead of
`host.docker.internal`.

### Limitations

- **No tracing yet** — request ids are the correlation seed; OTel spans/OTLP
  push are an Observability M2 candidate.
- **sandboxd internals are not directly instrumented** — sandbox activity is
  visible only as gateway tool-call series (`server="sandbox"`); per-container
  detail belongs with Sandboxes M2.
- **No per-tenant token accounting** — `agent_tokens_total` is per-agent; the
  tenant dimension is deliberately excluded (see the cardinality promise).

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
| `runtimectl admin secret rotate [--tenant <t>]` | Re-encrypt secrets under the current primary key (superuser: all tenants). |

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
| `GET /metrics` | Prometheus exposition for the whole fleet (control plane + merged agent series; always auth-exempt — see [Observability](#observability)). |
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
| `GET /metrics` | *Optional.* Prometheus text exposition for this agent. An agent without it (e.g. a foreign-SDK shim) is skipped by the fan-out scrape (reason `no_metrics`) and is **not** marked down. |
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

TWO worked examples host the SG Nutrition Investigator on different foreign
frameworks through the SAME shim — the reuse proof for the adapter seam:

- `examples/nutrition-label-openai/` — OpenAI Agents SDK (adapter: 39 code lines).
  Memory via the SDK's `SQLiteSession`; typed verdict via `output_type=`.
- `examples/nutrition-label-claude/` — Claude Agent SDK (adapter: 100 code lines).
  Memory via SDK-native `resume=` transcripts + a runtime→SDK session-id map;
  typed verdict via a `submit_verdict` in-process MCP tool (tool-as-output);
  all built-in tools disabled (`tools=[]` + deny-list + `dontAsk`).

Both boot under `runtimed` via their Makefiles:

```bash
cd examples/nutrition-label-claude   # or examples/nutrition-label-openai
cp .env.example .env          # fill in the proxy key (ANTHROPIC_* / OPENAI_*)
make run                      # builds binaries, uv sync, runs the control plane

# in another shell — the same conformance gate that validates Go agents:
make conformance              # runtimectl conformance --agent nutrition-claude
make demo-image IMAGE=milo.jpeg   # base64 a label photo → POST → stream the verdict
```

The shim provides **Level-1 durability** (sessions and their event logs persist
in `shim.db`, so after a restart sessions are listable, replayable, and
conversation memory continues) — but **not** Level-2 in-flight crash resume,
which remains the Go agents' DBOS-backed advantage. The shim also serves
`POST /sessions/{id}/messages` for follow-up turns on an existing session —
an extension the Go agent contract does not have yet (Go sessions are
single-turn workflows); reconciling it into the Go contract + conformance
suite is tracked in the ROADMAP. See
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
| `RUNTIME_AGENT_TENANT` | agentd | `default` | The agent's tenant, injected by the control plane; pins the memory store's isolation. |
| `RUNTIME_AGENT_MEMORY` | agentd | (unset) | `1` when the agent opted into memory (`memory: true`); tells agentd to wire the memory tool. |
| `RUNTIME_EMBED_MODEL` | agentd | (unset) | Embedding model for semantic recall. Unset ⇒ recall disabled (tag/id memory only). |
| `RUNTIME_EMBED_DIM` | agentd | (unset) | Embedding dimension (the `vector(N)` width). Required + positive when `RUNTIME_EMBED_MODEL` is set; invalid ⇒ fatal at startup. |
| `RUNTIME_EMBED_RECALL_K` | agentd | `5` | Max memories injected per turn. |
| `RUNTIME_EMBED_RECALL_FLOOR` | agentd | `0.25` | Minimum cosine similarity (0–1) for a memory to be injected. Tuned for OpenAI-family embeddings; see the per-model guidance under "Semantic recall". |
| `RUNTIME_INGEST_ENABLED` | agentd | (unset) | `1`/`true`/`yes`/`on` enables auto-ingestion. Requires semantic recall; ignored (warn) if embeddings are unset. |
| `RUNTIME_INGEST_MODEL` | agentd | (unset) | Chat model for fact extraction (reuses `OPENAI_*`). Required when ingestion is enabled with embeddings on; missing ⇒ fatal at startup. |
| `RUNTIME_INGEST_MIN_MESSAGES` | agentd | `2` | Growth gate: minimum thread messages before a turn is extracted. |
| `RUNTIME_INGEST_MAX_INFLIGHT` | agentd | `4` | Max concurrent extraction goroutines; turns over the cap are dropped. |
| `RUNTIME_INGEST_DEDUP_FLOOR` | agentd | `0.85` | Cosine similarity at/above which a candidate fact is treated as a duplicate and skipped. |
| `RUNTIME_INGEST_MAX_FACTS` | agentd | `10` | Hard cap on facts saved per turn. |
| `RUNTIME_LOG_FORMAT` | runtimed | `text` | `json` switches `slog` to JSON output. |
| `RUNTIME_CTL_URL` | runtimectl | `http://localhost:8080` | Control-plane base URL the CLI targets. |
| `RUNTIME_TOKEN` | runtimectl | (unset) | Bearer credential (service key, OIDC token, or bootstrap key) sent on every CLI request when set. |
| `RUNTIME_OIDC_ISSUER` | runtimed | (unset) | OIDC issuer URL; enables human login. Empty ⇒ OIDC disabled. |
| `RUNTIME_OIDC_CLIENT_ID` | runtimed | (unset) | OIDC client id (expected token audience). |
| `RUNTIME_OIDC_CLIENT_SECRET` | runtimed | (unset) | OIDC client secret for the console code exchange. |
| `RUNTIME_OIDC_REDIRECT_URL` | runtimed | `http://localhost:8080/ui/callback` | OIDC redirect URL registered with the issuer. |
| `RUNTIME_ADMIN_BOOTSTRAP` | runtimed | (unset) | One-time break-glass superuser service key (read from env, never stored). |
| `RUNTIME_SECRETS_KEYS` | runtimed | (unset) | Keyring: `id:base64key,id:base64key` (each 32 bytes). Enables multi-key rotation. Requires `RUNTIME_SECRETS_PRIMARY`. |
| `RUNTIME_SECRETS_PRIMARY` | runtimed | (unset) | Keyring id new secrets are sealed under. Required when `RUNTIME_SECRETS_KEYS` is set. |
| `RUNTIME_SECRETS_KEY` | runtimed | (unset) | Legacy/back-compat single key (base64 of 32 bytes). On its own ⇒ keyring `{v1:key}`. With `RUNTIME_SECRETS_KEYS` ⇒ names the legacy-decrypt key. Unset (and no keyring) ⇒ disabled; malformed ⇒ fatal. |
| `RUNTIME_GATEWAY_URL` | agentd | (set by runtimed) | Gateway MCP endpoint injected into `gateway:`-enabled agents (`?mode=search` appended for `gateway: search`). |
| `RUNTIME_GATEWAY_KEY` | agentd | (set by runtimed) | Bearer service key for the gateway, from `gateway.agent_keys` (identity on). |
| `RUNTIME_GATEWAY_SEARCH_FLOOR` | runtimed | `0.2` | Minimum cosine similarity for a `search_tools` match. |
| `RUNTIME_GATEWAY_SEARCH_K` | runtimed | `5` | Default `search_tools` result count (cap 20). |
| `RUNTIME_SANDBOX_*` | sandboxd | (see [sandbox](#code-interpreter-sandbox)) | Code-interpreter sandbox tuning: image, per-tenant cap, TTLs, workspace/memory/CPU limits, gVisor runtime, direct mode. |
| `RUNTIME_SHIM_DB` | (python shim) | `./shim.db` | SQLite path for the polyglot shim's Level-1 store (not used by runtimed). |

`runtimed` injects `RUNTIME_LISTEN_ADDR` and `RUNTIME_AGENT_ID` into each agent
subprocess from `runtime.yaml`; you don't set them by hand.

### Postgres schema

Applied automatically on startup (under an advisory lock so concurrent agents
don't race): `agents`, `sessions` (with `agent_id`, `status`, `turn_count`,
`workflow_id`), `session_events` (append-only), plus DBOS's own `dbos` schema.
pgvector backs [semantic recall](#semantic-recall) (the Compose image
provisions it; on your own Postgres a superuser must `CREATE EXTENSION vector`
once per database).

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
- **`TestGatewaySandboxE2E`** — boots `runtimed` with identity enforced, two
  tenants, and `sandboxd` federated as a `forward_tenant` stdio upstream
  (in-memory backend — no Docker needed): asserts the sandbox tools federate,
  a spoofed `__rt_tenant` argument is overridden by the gateway, and one
  tenant's sandbox is invisible and unusable to the other.
- **`TestGatewayRESTE2E`** — boots `runtimed` with identity enforced and an
  `openapi:` (REST) upstream scoped to one tenant: the spec is fetched over
  HTTP, generated `orders__*` tools are listed and called through
  `/gateway/mcp` (envelope JSON with the upstream status), the other tenant
  sees and can call nothing, and the merged `/metrics` + `/gateway/status`
  carry the `openapi` transport's series and state.
- **`TestObservabilityE2E`** — boots `runtimed` with identity enforced and
  asserts the metrics fan-out merges agent series into one valid exposition
  (server-enforced `agent` labels), HTTP routes are recorded as patterns (no
  raw-path cardinality), `X-Request-ID` is echoed/propagated, `/metrics` is
  reachable without credentials, and auth-rejected requests land in the
  `route="auth_rejected"` bucket.

**Live-gated tests** exercise the real Docker backend (build the image first
with `make sandbox-image`):

```bash
go test -tags live ./internal/sandbox/ -v   # real containers: file round-trip, 8 MiB write
```

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
full-stack Docker build). Since then: **polyglot agent hosting** (two milestones —
the Python contract shim hosting the OpenAI Agents SDK and the Claude Agent SDK,
each running the full nutrition investigator — see the Python shim section); the
**Identity** sub-project (three milestones — multi-tenant access control,
per-tenant secrets brokering, and secrets key rotation, below); the **Memory**
sub-project (three milestones — durable per-tenant store, semantic recall, and
auto-ingestion, with recall and ingest wired into the live turn path — see
[agent memory](#per-tenant-agent-memory)); the **Gateway** sub-project (three
milestones — MCP federation core, semantic tool search, and REST/OpenAPI→tool
adapters — see [MCP Gateway](#mcp-gateway)); the **Sandboxes** sub-project (first
milestone — the Docker-backed code interpreter, delivered through the gateway
— see [Code-interpreter sandbox](#code-interpreter-sandbox)); and the
**Observability** sub-project (first milestone — fleet-wide Prometheus
metrics, request-id correlation, and the Grafana overlay — see
[Observability](#observability)).

**Deliberately not yet implemented** (each is planned, scoped to a later
milestone or sub-project):

- **Identity — first three milestones DONE** (see [Authentication & multi-tenancy](#authentication--multi-tenancy)):
  M1 multi-tenant access control with OIDC human login, bcrypt-hashed service keys
  (constant-time verify), and per-agent admin/operator/viewer roles; M2 per-tenant
  **secrets brokering** (AES-256-GCM at rest, injected into the tenant's agents at
  spawn); M3 **secrets key rotation** (a multi-key keyring with self-describing,
  AAD-bound blobs + an explicit re-encrypt command). **Still to come:**
  fine-grained/custom RBAC, cross-tenant users + self-service, an admin console UI,
  optional local password accounts, and console CSRF (`state`/`nonce`) hardening.
- **Memory — first three milestones DONE** (see [agent memory](#per-tenant-agent-memory)):
  M1 durable per-tenant `MemoryStore`, M2 semantic recall (pgvector), and M3
  auto-ingestion (background LLM fact extraction + semantic dedup), with recall and
  ingest wired into the live turn path. **Still to come:** compaction/TTL/GC of
  dead rows, finer (per-agent/per-user) scoping, per-tenant embedding models,
  refinement/merge dedup, and session-level synthesis.
- **Observability — first milestone DONE** (see [Observability](#observability)):
  M1 Prometheus metrics (control-plane + per-agent series merged behind one
  auth-free `/metrics` via a hardened fan-out scrape), `X-Request-ID`
  correlation end-to-end (edge → proxy → agent logs → DBOS workflow input),
  and the Prometheus + Grafana compose overlay with a provisioned dashboard.
  **Still to come:** OTel tracing/OTLP push, sandboxd-internal metrics,
  per-tenant token accounting, alerting/recording rules, a console `/ui`
  metrics panel, log shipping, and DBOS-internal metrics.
- **Write actions from the console** — the console is read-only; deploy/stop/
  invoke from the UI is future work.
- **Subprocess pools / autoscaling** — one subprocess per agent today; no
  per-agent replicas or load-based scaling.
- **Dynamic deploy** — agents come from `runtime.yaml` at startup; no runtime
  `POST /agents` registration or rollback yet (tokens are config-only too).
- **Gateway — first three milestones DONE** (see [MCP Gateway](#mcp-gateway)):
  M1 MCP federation core (one endpoint federating upstream MCP servers,
  tenant-filtered via Identity service keys), M2 semantic tool search
  (`gateway: search` / `?mode=search` — one listed `search_tools` tool,
  embedding-ranked discovery, callable-but-unlisted catalog), and M3
  REST/OpenAPI→tool adapters (`openapi:` upstreams — one generated tool per
  spec operation, no MCP server required). **Still to come:**
  dynamic upstream registration, resources/prompts passthrough, OAuth2
  upstream auth, per-tenant upstream credentials, a console panel,
  auto-minted agent keys, and rate limits.
- **Polyglot hosting — first two milestones DONE** (see
  [the Python shim](#hosting-a-foreign-sdk-agent-python-shim)): the OpenAI
  Agents SDK and Claude Agent SDK adapters, each hosting the full nutrition
  investigator. **Still to come:** Level-2 in-flight crash resume, a TS shim,
  further adapters (PydanticAI is the next candidate), and reconciling the
  shim's follow-up-messages endpoint into the Go contract + conformance suite.
- **Sandboxes — first milestone DONE** (see
  [Code-interpreter sandbox](#code-interpreter-sandbox)): the isolated,
  stateful, Docker-backed code interpreter, federated behind the gateway with
  tenant-scoped ownership. **Still to come:** the browser sandbox, kernel-mode
  variable persistence (same tool surface, persistent interpreter), network
  egress policy / runtime `pip install`, per-user scoping, and a console
  panel.
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
