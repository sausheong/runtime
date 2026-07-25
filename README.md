# Runtime

Runtime is a self-hosted execution and operations layer for durable AI agents.
It runs local or remote agents behind one control plane and adds durable
sessions, routing, identity, tenant-aware tools and memory, isolated code and
browser execution, evaluations, metrics, and tracing.

Runtime is infrastructure around an agent. Your application still owns the
agent's instructions, models, tools, safety policy, and end-user experience. It
is inspired by managed agent infrastructure such as AWS Bedrock AgentCore, but
it is not a drop-in implementation or a claim of feature parity.

## Project status

The latest formal repository tag is `v0.2.0`. The current `master` tree contains
substantial unreleased work beyond that tag. Treat it as pre-release software
until a versioned release, migration notes, and release artefacts are published.

The repository has no licence file. Source is visible, but reuse and
redistribution terms are not granted until the project owner selects and adds a
licence.

## Documentation

| If you want to… | Read |
|---|---|
| Understand the architecture and capability boundaries | [Runtime overview](runtime.md) |
| Bring up the complete single-host stack | [Quickstart](quickstart.md) |
| Operate identity, persistence, security, and observability | [Operator guide](operator-guide.md) |
| Onboard a tenant and exercise the platform | [Tenant guide](tenant-guide.md) |
| Configure agents, pools, autoscaling, limits, pricing, and environment | [Configuration reference](configuration.md) |
| Configure authentication, secrets, OIDC, and durable memory | [Identity, secrets, and memory](identity-and-memory.md) |
| Federate MCP/REST tools and operate code/browser sandboxes | [Gateway and sandboxes](gateway-and-sandboxes.md) |
| Use metrics, logs, request IDs, tracing, dashboards, and alerts | [Observability](observability.md) |
| Create golden sets, run evaluations, and configure online scoring | [Evaluations guide](evals.md) |
| Use the CLI/API, implement the agent contract, or develop native agents | [Interfaces and development](interfaces-and-development.md) |
| Build or attach Go, Python, Claude SDK, or generic agents | [Deploying SDK agents](deploying-sdk-agents.md) |
| Add TLS and identity to a cloud or on-prem host | [Secured deployment](deploy/secured/README.md) |
| Deploy on Kubernetes | [Helm chart guide](deploy/charts/runtime/README.md) |
| Report security issues or contribute | [Security policy](SECURITY.md) and [contribution guide](CONTRIBUTING.md) |
| Upgrade, roll back, or publish a release | [Release guide](RELEASING.md) and [changelog](CHANGELOG.md) |
| See remaining work and release blockers | [Roadmap](ROADMAP.md) |
| Find material moved out of the former long README | [Documentation map](documentation-map.md) |

Worked agents live under [`examples/`](examples/).

The root README is deliberately an overview. Detailed material from the former
long-form README has been retained in the topic guides above; the
[documentation map](documentation-map.md) records the destination of every
former section.

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
- `agentd`, the bundled native Go agent host.
- `runtimectl`, the operator CLI.
- `agentruntime`, the Go library that turns a harness agent into a durable
  HTTP/SSE service.
- Contract shims and examples for non-Go agents.

## Capabilities and boundaries

| Area | What is implemented | Important boundary |
|---|---|---|
| Durable sessions | DBOS-backed completed-turn recovery and replayable SSE events for native Go agents | A tool side effect may repeat if the process dies before its turn checkpoint |
| Agent hosting | Local processes, replica pools, active-session autoscaling, and remote agents | Sessions remain pinned to their owner replica |
| Identity | OIDC, service keys, tenant roles, and encrypted tenant secrets | Open mode is only for local development |
| Gateway | Tenant-aware MCP and REST/OpenAPI federation, policy, quotas, and search | Dynamically registered targets must use public HTTP(S) endpoints |
| Memory | Tenant/actor-scoped durable memory, semantic recall, best-effort extraction, dead-row GC, and opt-in per-kind retention | Memory is shared across an agent tenant unless actor scoping is used |
| Sandboxes | Per-session code and browser containers with resource and network controls | The Docker socket makes the host a trusted boundary |
| Evaluations | Golden sets, rule/judge scoring, leased recovery, bounded online sampling, retention, and transcript filtering | Transcript redaction is best effort and evaluation is not a deployment gate |
| Operations | Health/readiness, private fleet metrics, tracing, dashboards, alerts, CLI, and console | Multi-control-plane quota state is not distributed |

See the [Runtime overview](runtime.md) for the detailed model and the
[roadmap](ROADMAP.md) for known gaps.

## Turnkey quickstart

The complete evaluation stack requires Docker with Compose v2:

```bash
make compose-init
make compose-build
make compose-up
```

`compose-init` generates `deploy/compose/.env` with a bootstrap key, secrets
keyring, and Grafana administrator password. The stack exposes the console on
`http://localhost:8080/ui`; management interfaces bind to host loopback. Read
the [quickstart](quickstart.md) and [operator guide](operator-guide.md) before
exposing it.

## Local development

Local development requires Go 1.25.1 or later, Postgres, and Docker if you use
the supplied database target:

```bash
make pg-up
make build
make run
```

The example [`runtime.yaml`](runtime.yaml) defines two deterministic agents:

```bash
./bin/runtimectl agents
./bin/runtimectl invoke --agent support "hello"
./bin/runtimectl sessions --agent support
./bin/runtimectl conformance --agent support
```

`runtimectl` uses `RUNTIME_CTL_URL` for the control-plane address and
`RUNTIME_TOKEN` for an optional bearer credential.

## Configuration

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
```

Set `RUNTIME_CONFIG` to select another file. [`runtime.yaml`](runtime.yaml)
contains worked examples. The [configuration
reference](configuration.md) documents local and remote pools, autoscaling,
limits, pricing, gateway blocks, validation, injected child variables, and the
full environment model.

Important process-level settings are:

| Variable | Purpose |
|---|---|
| `RUNTIME_PG_DSN` | Control-plane Postgres credential |
| `RUNTIME_AGENT_PG_DSN` | Restricted, single-tenant credential injected into local agents |
| `RUNTIME_CTL_ADDR` | Public API listener, default `:8080` |
| `RUNTIME_METRICS_ADDR` | Management metrics listener, default `127.0.0.1:9091` |
| `RUNTIME_AGENTD_BIN` | Local agent host binary |
| `RUNTIME_AGENT_ENV_PASSTHROUGH` | Comma-separated extra safe child variables |
| `RUNTIME_ADMIN_BOOTSTRAP` | Initial superuser secret |
| `RUNTIME_ADMIN_BREAK_GLASS` | Temporarily re-enable bootstrap after identity is configured; default off |
| `RUNTIME_SECRETS_KEYS` | Secrets keyring |
| `RUNTIME_SECRETS_PRIMARY` | Primary keyring entry |
| `RUNTIME_PUBLIC_URL` | HTTPS public URL used for secure-cookie inference |
| `RUNTIME_COOKIE_SECURE` | Explicit secure-cookie override |
| `RUNTIME_EVAL_RETENTION` | Evaluation/transcript retention, default `720h` |
| `RUNTIME_SESSION_RETENTION` | Terminal session/event retention, default `720h` |
| `RUNTIME_SESSION_RETENTION_DRY_RUN` | Count eligible sessions without deleting |
| `RUNTIME_MEMORY_RETENTION_FACT` | Fact-memory retention duration; disabled when unset |
| `RUNTIME_MEMORY_RETENTION_SUMMARY` | Summary-memory retention duration; disabled when unset |
| `RUNTIME_MEMORY_RETENTION_EPISODE` | Episodic-memory retention duration; disabled when unset |
| `RUNTIME_MEMORY_RETENTION_DRY_RUN` | Count eligible live-memory rows without deleting or incrementing deletion counters |
| `RUNTIME_MAX_REQUESTS` / `RUNTIME_MAX_STREAMS` | Control-plane concurrency limits |

## API and agent integration

The control-plane API exposes process health, readiness, agent discovery, and
agent-specific session routes under `/agents/{id}/...`. Fleet metrics are
intentionally served only from the private management listener.

```bash
curl -sS -X POST http://localhost:8080/agents/support/sessions \
  -H 'Content-Type: application/json' \
  -d '{"message":"hello"}'

curl -N \
  'http://localhost:8080/agents/support/sessions/ses-.../stream?since=0'
```

When identity is enabled, send `Authorization: Bearer <service-key>`.
Administrative APIs and the gateway are described in the [operator
guide](operator-guide.md), [tenant guide](tenant-guide.md), and [interfaces
reference](interfaces-and-development.md).

Native Go agents link `agentruntime`; Python SDK agents can use the supplied
authenticated contract shim; any language may implement the same HTTP/SSE contract. The
[agent deployment guide](deploying-sdk-agents.md) contains complete examples,
the endpoint contract, and framework-specific durability differences. Run the
conformance command above before registering a custom implementation.

## Security and reliability

- The authenticated control-plane API is the tenant security boundary.
- A local `agentd` is a trusted platform process, not a hostile-code sandbox.
  Identity-enabled deployments require a distinct `RUNTIME_AGENT_PG_DSN`; use
  a remote container/VM boundary for less-trusted code.
- Tenant-provided HTTP targets are SSRF-filtered. Operator file configuration
  is trusted and may intentionally use private infrastructure.
- Code and browser containers reduce application-level risk, but a service
  with the Docker socket remains privileged on the host.
- TLS is expected at the deployment edge. Cookie security is inferred from the
  public/OIDC HTTPS URL and can be forced with `RUNTIME_COOKIE_SECURE=1`.
- Side-effecting tools must be idempotent because recovery is at-least-once
  across an incomplete turn.

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
Python-shim tests, Helm rendering, shell checks, GCP image builds, and
turnkey/distributed Compose validation. GitHub Actions are pinned to commit SHAs and Dependabot tracks Go,
Actions, Python, and container dependencies.

Tagged releases publish immutable images and charts with SBOM/signature
artefacts through the [release workflow](.github/workflows/release.yml).
