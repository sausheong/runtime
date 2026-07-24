# Runtime

Runtime is a self-hosted execution and operations layer for durable AI agents.
It runs local or remote agents behind one control plane and adds session
durability, routing, identity, tenant-aware tools and memory, isolated code and
browser execution, evaluations, metrics, and tracing.

Runtime is inspired by managed agent infrastructure such as AWS Bedrock
AgentCore, but it is not a drop-in implementation or a claim of feature parity.
It is infrastructure around an agent. Your application still owns the agent's
instructions, models, tools, safety policy, and end-user experience.

## Project status

The latest formal repository tag is `v0.2.0`. The current `master` tree contains
substantial unreleased work beyond that tag, including the turnkey deployment
and post-v1 capability milestones recorded in [ROADMAP.md](ROADMAP.md). Treat
the current tree as pre-release software until a versioned release, migration
notes, and release artefacts are published.

The repository currently has no licence file. Source is visible, but reuse and
redistribution terms are not granted until the project owner selects and adds a
licence.

## Start here

- [Quickstart](quickstart.md) brings up the complete single-host stack.
- [What Runtime is](runtime.md) explains the architecture and capability
  boundaries.
- [Operator guide](operator-guide.md) covers identity, ports, persistence,
  security, and observability.
- [Tenant guide](tenant-guide.md) covers self-service onboarding.
- [Deploying SDK agents](deploying-sdk-agents.md) covers Go, Python, Claude SDK,
  and generic remote agents.
- [Helm chart guide](deploy/charts/runtime/README.md) covers Kubernetes.
- [Roadmap](ROADMAP.md) is the concise forward-looking backlog.

## Architecture

```text
clients and operators
        |
        v
  runtimed control plane
  identity | routing | registry | console | metrics
        |
        +-------------------+
        |                   |
        v                   v
 local agentd pools     remote contract agents
        |                   |
        +---------+---------+
                  |
                  v
     Postgres / pgvector / DBOS
 sessions | events | memory | identity | evaluations
                  |
                  v
     gateway, code sandbox, browser sandbox
```

The main components are:

- `runtimed`, the control plane and local-process supervisor.
- `agentd`, the bundled native agent host.
- `runtimectl`, the operator CLI.
- `agentruntime`, the Go SDK that turns a harness agent into a durable HTTP/SSE
  service.
- contract shims and examples for non-Go agents.

## What is implemented

### Durable sessions

Native Go sessions run as DBOS workflows. Completed turns are checkpointed in
Postgres, and an agent restart resumes from the first incomplete turn. Session
events are persisted before live delivery and use deterministic idempotency
keys, so recovery does not create duplicate replay events. SSE reconnects can
resume from a sequence number.

The durability boundary is a completed turn. A side-effecting tool may run
again if the process dies after the side effect but before the turn checkpoint.
Such tools still need idempotency keys or equivalent at-least-once handling.

Python contract shims persist sessions and replayable events but do not resume
an in-flight SDK invocation.

### Agent hosting

File-configured local agents can run as fixed replica pools or use
active-session autoscaling. Sessions remain pinned to their owner replica.
Scale-down drains a replica rather than moving active sessions. Remote agent
pools can be attached and health-checked, while their external process lifecycle
remains the remote operator's responsibility.

Runtime-configured remote agents can be registered, enabled, restarted at the
proxy layer, and removed without restarting `runtimed`. Local file-configured
agents remain startup-only.

### Identity and tenancy

Runtime supports OIDC browser login, platform service keys, a one-time bootstrap
superuser, and tenant-scoped `viewer`, `operator`, and `admin` roles. Session
routes verify both the requested agent and the session owner at the control
plane and agent boundary. Dynamic agent mutations verify the resource tenant
before changing state.

Open mode exists for local development. It is not suitable for an exposed
deployment.

### Secrets

Tenant credentials can be encrypted with AES-256-GCM under an operator keyring.
Values are write-only through the API and are resolved at agent spawn or
upstream dial time.

Local child processes no longer inherit the control plane's complete
environment. Runtime passes a small platform allowlist plus the resolved
agent-specific environment. Extra non-reserved variables can be named in
`RUNTIME_AGENT_ENV_PASSTHROUGH`.

Use a separate `RUNTIME_AGENT_PG_DSN` role with only the tables an agent needs.
If it is omitted, local agents use `RUNTIME_PG_DSN`; Runtime warns about this in
identity-enabled deployments.

### Tool gateway

The MCP gateway federates local stdio MCP servers, remote Streamable HTTP MCP
servers, and REST APIs described by OpenAPI. Tools are namespaced, filtered by
tenant, and policy-checked. Search mode can expose one discovery tool instead of
placing a large tool catalogue in every model prompt.

Tenant-registered agents and HTTP/OpenAPI upstreams must use public HTTP(S)
targets. Validation rejects local, private, link-local, metadata, reserved, and
userinfo URLs. DNS is resolved and checked again immediately before each
connection to resist DNS rebinding. File-configured upstreams are
operator-trusted and may intentionally use private addresses.

### Memory

Tenant-scoped memory supports explicit save/update/remove operations, pgvector
semantic recall, and best-effort extraction of durable facts from completed
conversations. It does not yet provide per-user or per-agent isolation,
retention policies, or session-level synthesis.

### Sandboxes

The code interpreter uses per-session Docker containers with resource limits, a
read-only root filesystem, no network, and an isolated workspace.

The browser service uses per-session Chromium containers and a policy-enforcing
egress proxy. In the turnkey Compose deployment, browser containers join an
internal Docker network shared only with `runtimed`; their unauthenticated CDP
port is not published on the host. Direct host installs without that network
publish CDP only on loopback. Chromium's sandbox is enabled by default.

Both services need the Docker socket on a single-host deployment. Docker socket
access is effectively host-root authority, so the node and `runtimed` remain
trusted infrastructure. gVisor can strengthen the container boundary where it
is available.

### Evaluations

Runtime stores golden datasets, runs rule or judge scorers, captures online
transcripts, classifies failures, and exposes aggregate results. Incomplete
evaluation runs are recovered at control-plane startup and completed case
indexes are not rerun. Credential-shaped transcript fields and common bearer
token patterns are redacted before persistence.

Completed/error evaluation data and transcripts are retained for 30 days by
default. Set `RUNTIME_EVAL_RETENTION` to another Go duration, or `0` to disable
automatic deletion.

Evaluation recovery and gateway quotas currently assume one active control
plane. Quota buckets are process-local, though persisted quota configuration is
shared. Multi-control-plane deployments need a distributed claim mechanism and
shared rate-limit state before these guarantees extend horizontally.

### Operations

- `/healthz` is process liveness.
- `/readyz` verifies Postgres with a bounded timeout.
- Prometheus fleet metrics use a separate management listener,
  `RUNTIME_METRICS_ADDR` (`127.0.0.1:9091` by default), rather than the public
  API listener.
- HTTP servers bound request headers and idle connections. SSE remains
  streaming-friendly.
- OpenTelemetry traces, structured request IDs, Prometheus, Grafana,
  Alertmanager, and Jaeger are included in deployment assets.
- The turnkey stack binds management UIs to loopback and requires a generated
  Grafana administrator password.

## Quick local development

Prerequisites are Go 1.25.1 or later and Postgres. Dependencies are pinned in
`go.mod`.

```bash
make pg-up
make build
RUNTIME_AGENTD_BIN=./bin/agentd ./bin/runtimed
```

The example [runtime.yaml](runtime.yaml) defines two deterministic agents.

```bash
./bin/runtimectl agents
./bin/runtimectl invoke --agent support "hello"
./bin/runtimectl sessions --agent support
```

For the complete stack:

```bash
make compose-init
make compose-build
make compose-up
```

`compose-init` creates `deploy/compose/.env` with a bootstrap key, secrets
keyring, and Grafana password. See [quickstart.md](quickstart.md) before exposing
any port.

## Minimal configuration

Runtime rejects unknown YAML fields, additional YAML documents, unsafe
identifiers, invalid durations, colliding ports, and incompatible local/remote
settings.

```yaml
agents:
  - id: support
    name: Support Agent
    model: test/scripted
    listen_addr: 127.0.0.1:8101
    replicas: 2
    memory: true
    gateway: search
    limits:
      max_turns: 20
      max_tokens: 100000
      turn_timeout: 2m
      session_timeout: 30m

  - id: remote-research
    name: Remote Research Agent
    model: provider/model
    url: https://agent.example.com
    auth_token: ${REMOTE_AGENT_TOKEN}
```

Set `RUNTIME_CONFIG` to select another file. See [runtime.yaml](runtime.yaml) and
[runtime.md](runtime.md) for the full schema.

Important process-level settings include:

| Variable | Purpose |
|---|---|
| `RUNTIME_PG_DSN` | Control-plane Postgres credential |
| `RUNTIME_AGENT_PG_DSN` | Restricted credential injected into local agents |
| `RUNTIME_CTL_ADDR` | Public API listener, default `:8080` |
| `RUNTIME_METRICS_ADDR` | Management metrics listener, default `127.0.0.1:9091` |
| `RUNTIME_AGENTD_BIN` | Local agent host binary |
| `RUNTIME_AGENT_ENV_PASSTHROUGH` | Comma-separated extra safe child variables |
| `RUNTIME_ADMIN_BOOTSTRAP` | Initial superuser secret |
| `RUNTIME_SECRETS_KEYS` | Secrets keyring |
| `RUNTIME_SECRETS_PRIMARY` | Primary keyring entry |
| `RUNTIME_PUBLIC_URL` | HTTPS public URL used for secure-cookie inference |
| `RUNTIME_COOKIE_SECURE` | Explicit secure-cookie override |
| `RUNTIME_EVAL_RETENTION` | Evaluation/transcript retention, default `720h` |

## HTTP contract

The control plane exposes:

```text
GET  /healthz
GET  /readyz
GET  /agents
POST /agents/{agent}/sessions
GET  /agents/{agent}/sessions
GET  /agents/{agent}/sessions/{session}
GET  /agents/{agent}/sessions/{session}/events
GET  /agents/{agent}/sessions/{session}/stream
```

Fleet metrics are intentionally not mounted on this listener. Prometheus
scrapes `http://<RUNTIME_METRICS_ADDR>/metrics` on its private management path.

Create and stream a session:

```bash
curl -sS -X POST http://localhost:8080/agents/support/sessions \
  -H 'Content-Type: application/json' \
  -d '{"message":"hello"}'

curl -N 'http://localhost:8080/agents/support/sessions/ses-.../stream?since=0'
```

When identity is enabled, send `Authorization: Bearer <service-key>`.

## Writing an agent

A native Go agent supplies a harness `AgentSpec`, provider, tools, and
optionally memory/evaluation integrations to `agentruntime.Serve`:

```go
if err := agentruntime.Serve(ctx, agentruntime.Config{
    AgentID: os.Getenv("RUNTIME_AGENT_ID"),
    Spec:    spec,
    Provider: provider,
    Tools:   tools,
}); err != nil {
    log.Fatal(err)
}
```

Every custom or shim agent must implement the documented HTTP/SSE contract.
Use the conformance suite before registration:

```bash
go run ./cmd/runtimectl conformance --url http://127.0.0.1:8101
```

See [deploying-sdk-agents.md](deploying-sdk-agents.md) for complete examples and
framework-specific durability differences.

## Security model

Runtime provides tenant checks and narrows process/container/network exposure,
but its boundaries are not interchangeable:

- The authenticated control-plane API is the tenant security boundary.
- A local `agentd` is a trusted platform process, isolated for failure
  containment rather than hostile-code containment. It shares the host user and
  can receive a database credential. Use a restricted DB role or a remote
  container/VM boundary for less-trusted agent code.
- Tenant-provided public URLs are SSRF-filtered. Operator file configuration is
  trusted and can opt into private infrastructure.
- Code/browser containers reduce application-level risk, but a daemon with the
  Docker socket remains privileged on the host.
- TLS is expected at the deployment edge. Cookie security is inferred from the
  public/OIDC HTTPS URL and can be forced with `RUNTIME_COOKIE_SECURE=1`.
- `RUNTIME_ADMIN_BOOTSTRAP`, database credentials, keyring values, and provider
  credentials must never be placed in tenant-controlled configuration.

The turnkey deployment is a single-node operational profile, not a hostile
multi-tenant cloud isolation boundary.

## Development and verification

```bash
make fmt-check
make vet
make test
make pg-up
make test-integration
make helm-lint
```

CI also runs race detection on concurrency-heavy packages, `govulncheck`,
Python-shim tests, Helm rendering, shell checks, and container/Compose
validation. GitHub Actions are pinned to commit SHAs and Dependabot tracks Go,
Actions, Python, and container dependencies.

## Known architectural limits

- One active control plane is required for exact evaluation recovery and global
  quotas.
- Native Go agents have turn-level crash resume; contract shims do not resume
  an in-flight SDK call.
- Local agents are trusted subprocesses, not hostile-code sandboxes.
- Remote process start/stop is owned by its orchestrator.
- File-configured local agents require restart for registry changes.
- Memory is tenant-scoped, not per-user or per-agent.
- Code and browser isolation relies on a trusted Docker host.
- Kubernetes support is a Helm deployment, not an operator with CRDs.

These are architectural constraints, not hidden release promises. Planned work
lives in [ROADMAP.md](ROADMAP.md); completed implementation history remains
available in Git history.
