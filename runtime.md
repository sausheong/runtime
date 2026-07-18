# Runtime: Self-Hosted Infrastructure for Durable AI Agents

## 1. Executive Summary and Architectural Model

### What is Runtime?

Runtime is an open-source, on-prem platform for hosting and operating LLM agents. It provides the infrastructure around an agent—the durable execution loop, process supervision, identity, memory, tool access, isolation, observability, and operator surfaces—so application teams can concentrate on the agent's instructions, models, and domain tools.

It is designed as a self-hosted counterpart to services such as AWS Bedrock AgentCore. Runtime keeps the control plane, agent processes, session history, credentials, and operational telemetry under your control. Its smallest useful deployment is a single Runtime process plus Postgres; its turnkey deployment adds identity, memory, gateway services, sandboxes, metrics, and tracing with Docker Compose.

Runtime is not an LLM and it does not prescribe one agent framework. It is the execution and operations layer between users, agent code, model providers, tools, and infrastructure.

```text
Users, applications, and operators
                 │
                 ▼
┌──────────────────────────────────────────────────────────┐
│                    Runtime control plane                 │
│  Routing · Identity · Registry · Console · Observability │
└───────────┬───────────────┬───────────────┬──────────────┘
            │               │               │
            ▼               ▼               ▼
      ┌──────────┐    ┌──────────┐    ┌──────────┐
      │ Agent A  │    │ Agent B  │    │ Agent C  │
      │ replica  │    │ replicas │    │ remote   │
      └────┬─────┘    └────┬─────┘    └────┬─────┘
           │               │               │
           ├───────────────┴───────────────┤
           ▼                               ▼
   Postgres / pgvector             Gateway and sandboxes
   sessions · checkpoints          MCP · REST · browser
   events · memory · identity      Python · shell · tools
```

### The core value proposition

Runtime turns an agent program into an operable service:

- **Durable by default.** Completed turns are checkpointed in Postgres. A restarted Go agent resumes from its last completed turn instead of starting the session again.
- **Bounded execution.** Operators can cap the duration of one turn or a whole session, the number of turns, and cumulative input/output tokens.
- **Many agents, one control plane.** Agents are registered once and exposed through a consistent path-routed API, CLI, and console.
- **Framework and model flexibility.** Native Go agents use the `agentruntime` library; Python agents built with the OpenAI Agents SDK or Claude Agent SDK can use the contract shim; any other process can integrate by implementing the HTTP/SSE contract.
- **Tenant isolation at the edge.** Human and machine identities are mapped to tenants and roles before traffic reaches agent code.
- **Centralized tools.** MCP servers and REST/OpenAPI services can be exposed through one searchable, tenant-aware gateway.
- **Controlled execution.** Stateful code and browser tools run in isolated, per-session containers rather than in the agent process.
- **Operations included.** Health checks, restart supervision, metrics, request correlation, traces, a CLI, and a web console are part of the platform.
- **Self-hosted.** Runtime can run on a laptop, a single production host, Kubernetes, or a distributed set of agent hosts without requiring a managed cloud agent service.

### What Runtime does—and does not do

Runtime owns the lifecycle around agents. Your agent still owns its domain behavior.

| Runtime provides | Your agent provides |
|---|---|
| Session creation, status, event replay, and streaming | System prompt and application behavior |
| Durable turn checkpoints for native Go agents | Choice of model and provider |
| Process supervision, routing, pools, and autoscaling | Domain-specific tools and business logic |
| Authentication, tenant boundaries, roles, and secrets | Decisions about when and how tools are used |
| Shared memory, gateway, code, and browser facilities | Application-specific safety and evaluation criteria |
| Metrics, tracing, logging, CLI, console, and conformance | Product UI or end-user experience, if required |

## 2. Platform Architecture and Major Capabilities

### 2.1 Durable Agent Runtime

The durable runtime is the platform's execution spine. A session is represented by a DBOS workflow, and each agent turn is executed as a checkpointed DBOS step.

```text
Create session
      │
      ▼
Run model + tool turn ──► persist completed step ──► emit durable events
      │                                                     │
      │ continue                                            ▼
      └──────────────────────────────────────────────► next turn

On crash: restart process ──► load workflow ──► replay checkpoints
                                               └─► resume first incomplete turn
```

This provides several practical guarantees:

- Completed turns are not re-run after an ordinary crash and restart of the same agent binary.
- Persisted SSE events can be replayed when a client reconnects with `?since=<sequence>`.
- A session exposes a real lifecycle and turn count rather than being only an open HTTP stream.
- Each replica has a stable executor identity, so only the replica that owns a session recovers it.
- Operator limits are evaluated from checkpointed state, so token and turn budgets survive ordinary recovery.

Native agents support four lifecycle guardrails: `turn_timeout`,
`session_timeout`, `max_turns`, and `max_tokens`. A breach ends the session with
the terminal `limit_exceeded` status, publishes a final `error` event naming the
limit, and increments `agent_session_limit_hits_total{agent,limit}`. The
session-time budget is wall-clock based, so downtime during a crash and recovery
still counts.

The durability boundary is a **completed turn**, not an individual external side effect. If a tool performs a side effect and the process dies before that turn is checkpointed, Runtime may call the tool again. Side-effecting tools should therefore use idempotency keys or otherwise tolerate at-least-once execution.

Native Go agents receive full DBOS-backed turn recovery. Python contract-shim agents persist sessions and event replay in SQLite, but do not currently resume an in-flight SDK invocation after a process crash.
The Python shim also does not currently enforce Runtime's native lifecycle
limits; SDK agents should configure equivalent limits in their framework or
process supervisor.

### 2.2 Multi-Agent Hosting, Pools, and Autoscaling

`runtimed` loads an agent registry and either starts local agent processes or attaches to remote ones. Every agent is addressed through `/agents/{id}/...`, giving clients one stable control-plane URL regardless of where an agent runs.

Local agents are isolated as operating-system subprocesses. A failed agent is restarted with capped backoff, while other agents remain available. Agents may run as fixed replica pools or use load-based autoscaling between configured minimum and maximum replica counts.

For replica pools:

- New sessions are distributed across replicas.
- A session remains pinned to its owner replica for its lifetime.
- A restarted replica recovers the work associated with its stable executor identity.
- If the owner is unavailable, session-scoped requests return `503` until it returns; Runtime does not route the session to a replica that cannot safely resume it.
- Scale-down drains a replica before stopping it, so active sessions are not moved unsafely.

Runtime also supports remote agents. The control plane health-checks and proxies to them but leaves their OS or container lifecycle to the remote host, Docker, or Kubernetes. Remote agents can be registered, enabled, disabled, reattached, and removed dynamically without restarting the control plane.

### 2.3 Identity, Tenancy, and Secret Brokering

Identity is enforced at the control-plane edge so hosted agents do not each need to implement authentication and tenant filtering.

Runtime supports:

- OIDC authorization-code login for people using the console.
- Platform-issued service keys for applications, automation, and agents.
- Three tenant-scoped roles: `viewer`, `operator`, and `admin`.
- Tenant-filtered agent, session, gateway, and console views.
- A one-time bootstrap superuser for initial platform setup.
- Open mode for low-friction local development when no identity mechanism is configured.

Cross-tenant resources are hidden with `404`, rather than revealing their existence with `403`. Within a tenant, role violations return `403`; missing or invalid credentials return `401`.

Provider and upstream credentials can be stored per tenant. Runtime encrypts them with AES-256-GCM, stores only ciphertext, never returns secret values through the API, and injects resolved values into an agent's environment when it starts. A multi-key keyring supports online rotation and explicit re-encryption of existing records.

This lets each tenant bring its own model or API credentials without changing the agent's normal `os.Getenv`-style configuration.

### 2.4 Durable Memory and Semantic Recall

An agent can opt into a tenant-scoped memory store backed by Postgres. Memory is shared by the tenant's enabled agents and isolated from every other tenant.

The memory stack has three layers:

1. **Explicit durable memory** through save, update, remove, list, and get tools.
2. **Semantic recall** using embeddings and pgvector to retrieve relevant prior facts before each turn.
3. **Automatic ingestion** that extracts durable facts from completed conversations in the background, deduplicates them semantically, and stores them for later recall.

Recall and ingestion are best-effort. An embedding or extraction failure does not fail the user's turn. Operators can tune result counts, similarity floors, ingestion concurrency, and duplicate thresholds. The embedding model and vector dimension must agree, and pgvector must be installed in the target database.

Runtime currently scopes memory per tenant. Per-user and per-agent memory boundaries, TTL/compaction, and session-level synthesis remain future work.

### 2.5 MCP and REST Tool Gateway

Runtime exposes a central Streamable HTTP MCP endpoint at `/gateway/mcp`. It can federate:

- Local stdio MCP servers started by Runtime.
- Remote Streamable HTTP MCP servers.
- Ordinary REST APIs described by OpenAPI 3.x documents.

Tools are namespaced by upstream to prevent collisions. Visibility is tenant-filtered, and every tool call is authorized against the current principal. When an upstream is not visible, its tools are omitted from discovery and behave as not found if called.

For large tool catalogs, an agent can use **search mode**. Instead of placing every tool schema in the model context, Runtime lists a single `search_tools` capability. The agent describes what it needs, receives embedding-ranked matches with schemas, and calls the selected tool by name. This reduces prompt size while keeping the tenant's complete catalog callable.

Tenant administrators can register HTTP and OpenAPI upstreams at runtime through the console, admin API, or CLI. An upstream may refer to a secret by name; Runtime resolves that secret and injects it into the configured header only when dialing the upstream.

### 2.6 Isolated Code and Browser Sandboxes

The gateway can expose two stateful, per-session execution environments:

- **Code interpreter:** Python and shell execution inside a locked-down Docker container with a read-only root filesystem, resource limits, no network access, and an isolated workspace.
- **Browser:** Chromium controlled through a sandbox service, with outbound traffic constrained by hostname allow/deny policy.

Both environments are tenant-scoped and session-owned. State can survive across calls within a session without exposing the agent host filesystem or placing execution libraries inside the agent process.

The single-host implementation uses the Docker socket to create these containers. Access to the Docker socket is root-equivalent on the host, so the turnkey stack is intended for a trusted node. Stronger isolation can be added with gVisor where available; untrusted multi-user deployments should assess the host boundary carefully.

### 2.7 Observability and Operations

Runtime exposes one Prometheus endpoint for the entire fleet. The control plane merges its own metrics with metrics scraped from agents and enforces the registered agent label so an agent cannot impersonate another series.

The observability stack includes:

- Fleet, HTTP, agent, turn, token, tool, gateway, restart, and proxy metrics.
- A provisioned Grafana dashboard.
- `X-Request-ID` validation, generation, forwarding, response echo, and structured log correlation.
- OpenTelemetry traces exported over OTLP/HTTP.
- Correlated control-plane request traces and durable workflow traces, joined by request ID.
- Jaeger and an OpenTelemetry Collector in the bundled deployment.

Metric labels deliberately exclude session, user, and tenant identifiers to avoid unbounded cardinality and accidental disclosure. Message text and tool arguments are not attached to traces.

### 2.8 Contract-First, Polyglot Agent Hosting

Runtime integrates agents through a small HTTP and SSE contract:

| Endpoint | Purpose |
|---|---|
| `GET /healthz` | Liveness and readiness |
| `GET /meta` | Agent identity and contract version |
| `POST /sessions` | Create a session and begin work |
| `GET /sessions` | List an agent's sessions |
| `GET /sessions/{id}` | Read session status |
| `GET /sessions/{id}/stream?since=N` | Replay and stream sequenced events |
| `GET /metrics` | Optional agent metrics |

The reusable conformance suite executes the contract against a live agent. It is available both as a CLI command and as a Go test helper, making compatibility an executable CI gate rather than a documentation claim.

There are three integration paths:

- **Native Go:** link `agentruntime`, provide a harness `AgentSpec`, provider, and tool registry, and let the library serve the contract and durable loop.
- **Python SDKs:** use the framework-agnostic `runtime_contract` FastAPI package and implement a small adapter around the OpenAI Agents SDK, Claude Agent SDK, or another Python framework.
- **Any language:** implement the contract directly and pass the same conformance suite.

## 3. What Runtime Can Do for You

### 3.1 Turn a Local Agent into a Managed Service

An agent author can move from a local process to a managed endpoint without building a bespoke control plane. Runtime supplies session APIs, SSE streaming, replay, health checks, routing, supervision, and deployment conventions.

For a native Go agent, the author-facing configuration is intentionally small:

```go
err := agentruntime.Serve(ctx, agentruntime.Config{
    Spec: harnessruntime.AgentSpec{
        Name:         "support",
        Model:        "anthropic/claude-sonnet-4-6",
        SystemPrompt: "Help customers solve product issues.",
        MaxTurns:     20,
    },
    Provider: provider,
    Tools:    tools,
})
```

The operator supplies Postgres, identity, bind addresses, secrets, and gateway settings. The agent supplies its model behavior and tools.

### 3.2 Operate Several Teams or Use Cases on One Platform

A single Runtime installation can host support, research, data, operations, and specialist agents while preserving separate tenants and credentials. Teams share infrastructure and gateway integrations without sharing access to each other's agents or memory.

Typical uses include:

- Internal copilots that must remain on company infrastructure.
- Durable research or automation agents whose sessions outlive client connections.
- Tool-using agents that need centrally governed MCP and REST access.
- Multi-tenant agent platforms where each customer supplies credentials.
- Data or coding assistants that need isolated Python, shell, or browser execution.
- Mixed-framework fleets being migrated toward a common operational contract.

### 3.3 Survive Disconnects and Ordinary Process Failures

Clients do not need to hold one fragile connection for the lifetime of an agent run. They can create a session, stream events, disconnect, inspect status later, and reconnect from the last event sequence. Native agents also recover completed turns after process restart.

### 3.4 Centralize Security and Tool Governance

Runtime makes identity, secret handling, tenant visibility, and tool authorization platform responsibilities. This prevents every agent team from building a different authentication layer or embedding credentials in YAML and source code.

### 3.5 Start Small and Change the Deployment Topology Later

The same contract supports local subprocesses, custom commands, remote containers, and Kubernetes agent pods. A team can begin with one host and later separate agent workloads from the control plane without changing client-facing routes.

## 4. Developer and Operator Workflow

### 4.1 Bring Up the Turnkey Stack

Prerequisites are Docker with Compose v2 and a clone of this repository.

```bash
make compose-init
make compose-build
cd deploy/compose
docker compose up
```

The bundled stack starts Postgres with pgvector, the Runtime control plane, an air-gap-friendly embedder, the sandbox and browser infrastructure, Prometheus, Grafana, the OpenTelemetry Collector, and Jaeger.

Primary surfaces:

| Surface | Default address |
|---|---|
| Control plane and console | `http://localhost:8080` |
| Grafana | `http://localhost:3000` |
| Prometheus | `http://localhost:9090` |
| Jaeger | `http://localhost:16686` |

### 4.2 Register Agents Declaratively

Local file-configured agents live in `runtime.yaml`:

```yaml
agents:
  - id: support
    name: Support Agent
    model: anthropic/claude-sonnet-4-6
    listen_addr: 127.0.0.1:8101
    tenant: acme
    memory: true
    gateway: search
    autoscale:
      min: 1
      max: 4
      target_sessions_per_replica: 5
    limits:
      turn_timeout: 2m
      session_timeout: 30m
      max_turns: 50
      max_tokens: 200000
```

Runtime validates required fields, unique IDs and addresses, derived replica ports, mutually exclusive local and remote settings, and feature prerequisites before starting the fleet.

File-configured local agents are loaded at startup. Remote agents may also be managed dynamically through the admin surfaces.

### 4.3 Invoke and Inspect Agents

`runtimectl` targets `RUNTIME_CTL_URL` and sends the bearer credential in `RUNTIME_TOKEN` when configured.

```bash
runtimectl agents
runtimectl invoke --agent support "Summarize today's open incidents"
runtimectl sessions --agent support
runtimectl logs --agent support ses-123
runtimectl conformance --agent support
```

The web console at `/ui` provides a tenant-filtered fleet overview, agent session lists, live SSE session views, managed remote-agent controls, and tenant onboarding for keys and gateway upstreams. It is intentionally an operator surface rather than a general end-user chat application.

### 4.4 Onboard a Tenant

The bootstrap flow is:

```bash
export RUNTIME_TOKEN="$RUNTIME_ADMIN_BOOTSTRAP"

runtimectl admin tenant create acme --name "Acme"
runtimectl admin user add admin@acme.example --tenant acme --role admin
runtimectl admin key create --tenant acme --role operator --label agent-key
```

A tenant administrator can then add users and keys, store provider credentials, and register gateway upstreams for that tenant. Service-key secrets are displayed only once and stored as bcrypt hashes.

### 4.5 Add a Foreign-SDK Agent

The Python shim reduces the integration to an adapter and entrypoint:

```python
from runtime_contract import serve

class MyAdapter:
    async def run(self, session_id, message, images, history):
        async for item in run_my_sdk(message):
            yield item

serve(MyAdapter)
```

Point an agent entry at the process:

```yaml
agents:
  - id: python-agent
    name: Python Agent
    model: openai/gpt-5.4
    listen_addr: 127.0.0.1:8302
    workdir: ./examples/my-agent
    command: ["uv", "run", "python", "serve.py"]
```

Run `runtimectl conformance --agent python-agent` before treating the adapter as production-ready.

## 5. Deployment Models

### 5.1 Single Host

The simplest production topology runs `runtimed` under systemd, Docker, or another process manager and points it at a reliable Postgres instance. Runtime supervises the local agents beneath that one control-plane process.

Postgres is the durability and availability floor. If it is unavailable, new sessions and durable turns cannot proceed. Use an HA Postgres service when the platform itself must be highly available.

### 5.2 Turnkey Docker Compose

`deploy/compose/` is the complete single-host deployment and the recommended evaluation path. It includes all six implemented platform pillars and the supporting observability services. The host Docker socket is mounted for sandbox creation, so use this topology only on a trusted node.

### 5.3 Kubernetes and Helm

The Helm chart supports a secure-by-default control-plane deployment and optional per-agent pod scheduling. Agent StatefulSets attach to Runtime as remote pools while retaining the same external API.

The chart runs containers as non-root, drops capabilities, uses a read-only root filesystem, and supports bundled or external Postgres. Consult `deploy/charts/runtime/README.md` for the current topology and values reference.

### 5.4 Distributed Remote Agents

An agent may run on another VM or container host and expose the Runtime contract over HTTP or HTTPS. The control plane attaches through `url:` configuration or dynamic registration. Optional bearer authentication protects the agent endpoint.

Runtime can route, health-check, enable, disable, and reattach these agents. Starting and stopping their actual remote processes remains the responsibility of the host orchestrator.

## 6. Security and Reliability Model

### 6.1 Security Checklist

- Enable OIDC or service-key identity outside local development.
- Terminate TLS before the control plane and remote agent endpoints.
- Remove the bootstrap credential after creating the first tenant administrator.
- Store tenant credentials through the encrypted broker and maintain recoverable keyring backups.
- Keep agent and upstream tenant assignments explicit.
- Restrict access to `/metrics` at the network layer if operator-level identifiers are sensitive; the endpoint is intentionally unauthenticated for Prometheus.
- Treat access to the Docker socket as root-equivalent and isolate the sandbox host accordingly.
- Review code and browser egress policies for the deployment's threat model.
- Make side-effecting tools idempotent.
- Use independent database backups; Runtime durability is not a substitute for Postgres backup and recovery.

### 6.2 Reliability Characteristics

- Agent failure is contained to that agent or replica.
- Supervisors restart failed local processes with bounded backoff.
- Completed native-agent turns and emitted events survive ordinary restarts.
- Agent startup is readiness-gated and sequential to avoid unsafe first-run DBOS schema races.
- Graceful shutdown cancels supervisors and allows agents to drain through DBOS shutdown.
- Gateway and embedding failures are handled according to the feature: critical configuration fails fast, while best-effort recall and ingestion degrade without failing the user turn.

## 7. Feature Matrix and Current Scope

Runtime has met the repository's documented v1.0 acceptance bar, but the latest formal repository tag is currently `v0.2.0`. Treat the current tree as a v1.0 release candidate until a formal v1.0 release is published.

| Area | Implemented now | Notable remaining work |
|---|---|---|
| Durable runtime | Native per-turn checkpoints, event replay, supervision | Cross-binary-version recovery policy and finer side-effect semantics |
| Lifecycle safety | Native turn/session timeouts, turn cap, token budget, terminal breach status and metric | Shim-native enforcement and aggregate tenant budgets |
| Scale | Replica pools, affinity, load-based local autoscaling, drain | Richer scaling signals and force-drain deadlines |
| Identity | OIDC, service keys, fixed tenant roles, encrypted secrets, rotation | Custom RBAC, local password accounts, cross-tenant users |
| Memory | Durable store, pgvector recall, automatic ingestion | TTL/GC, per-user scopes, compaction and synthesis |
| Gateway | MCP federation, semantic search, REST/OpenAPI adapters, dynamic HTTP upstreams | Resources/prompts passthrough, OAuth upstreams, rate limits |
| Sandboxes | Stateful code and browser containers, network controls | Kernel persistence, runtime package installation, per-user scope |
| Observability | Prometheus, Grafana, request IDs, OTel, Jaeger | Token accounting by tenant, alerts, log shipping, sandbox internals |
| Polyglot | Go SDK, Python contract shim, OpenAI and Claude examples | In-flight shim recovery, TypeScript shim, more framework adapters |
| Deployment | Single host, Compose, Helm, remote agents, per-agent pods | Kubernetes operator/CRDs and mTLS |

Runtime does not currently implement every AWS AgentCore feature. In particular, it does not claim equivalents for AgentCore Policy/Cedar, Evaluations, or Payments. Its present focus is the self-hosted execution spine and six surrounding pillars: identity, memory, gateway, sandboxes, observability, and turnkey operations.

## 8. Choosing Runtime

Runtime is a strong fit when you need:

- Control over where agent code, prompts, credentials, and memory run.
- Durable, reconnectable sessions rather than request-bound chat calls.
- Several agents or teams behind one operational control plane.
- A consistent contract across Go and Python agent frameworks.
- Centralized MCP and REST tool governance.
- Tenant isolation and bring-your-own provider credentials.
- Isolated code or browser execution.
- A path from a single host to remote or Kubernetes-hosted agents.

It may not be the right fit when you need a fully managed service with no infrastructure ownership, hard multi-region control-plane availability out of the box, arbitrary custom authorization policy, or a turnkey end-user chat product. Runtime is infrastructure for agent systems; it is not the finished product interface those systems present to users.

## 9. Reference Map

| Need | Start here |
|---|---|
| Install the complete platform | [`quickstart.md`](quickstart.md) |
| Operate the single-host stack | [`operator-guide.md`](operator-guide.md) |
| Onboard tenants | [`tenant-guide.md`](tenant-guide.md) |
| Build and configure agents | [`README.md`](README.md) |
| Host OpenAI or Claude SDK agents | [`deploying-sdk-agents.md`](deploying-sdk-agents.md) |
| Deploy with Helm | [`deploy/charts/runtime/README.md`](deploy/charts/runtime/README.md) |
| Explore working agents | [`examples/`](examples/) |
| Validate an implementation | `runtimectl conformance --agent <id>` |

Runtime's design can be summarized in one sentence:

> Bring your agent, model, and tools; Runtime supplies the durable, secure, observable, self-hosted platform around them.
