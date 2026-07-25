# Interfaces and development

This guide collects the command-line, HTTP, agent-contract, native SDK,
durability, testing, and repository-layout material that was formerly embedded
in the root README.

## `runtimectl`

The CLI reads:

```bash
export RUNTIME_CTL_URL=http://localhost:8080
export RUNTIME_TOKEN=<optional-bearer-key>
```

Core agent workflow:

```bash
runtimectl agents
runtimectl invoke --agent support "hello"
runtimectl invoke -v --agent support "hello"
runtimectl sessions --agent support
runtimectl logs --agent support <session-id>
runtimectl conformance --agent support
```

When only one agent is visible, commands that need an agent can infer it.
Otherwise `--agent` is required. `invoke` creates a session and streams it;
`-v` also prints the request ID.

Administrative command families:

| Family | Commands |
|---|---|
| Tenants | `admin tenant create` |
| Users | `admin user add`, `admin user ls` |
| Service keys | `admin key create`, `admin key ls`, `admin key revoke` |
| Secrets | `admin secret set`, `set-oauth2`, `set-obo`, `ls`, `rm`, `rotate` |
| Dynamic upstreams | `admin upstream add`, `ls`, `rm` |
| Dynamic agents | `admin agent add`, `ls`, `rm`, `enable`, `disable`, `restart` |
| Cedar policies | `admin policy add`, `ls`, `rm` |
| Gateway quotas | `admin quota add`, `ls`, `rm` |
| Evaluations | `admin eval set`, `run`, `runs`, `results`, `policy`, `online-results`, `failures` |
| Registration | `register mint`, `list`, `revoke` |

Run an incomplete command to see its accepted flags. Prefer
`--value-stdin`/`--client-secret-stdin` for secret material so it does not
appear in process listings or shell history. `--tenant` is for a platform
superuser acting on another tenant; a tenant administrator normally omits it.

Evaluation examples and JSON formats are in [Evaluations](evals.md). Identity,
secret, and OBO examples are in [Identity, secrets, and
memory](identity-and-memory.md).

## Web console

The console is rooted at `/ui`. Depending on role and enabled capabilities it
provides:

- agent and session views;
- an observability view;
- tenant onboarding for upstreams and managed remote agents;
- golden-set, run, online-result, and failure views;
- OIDC login and logout.

The console is an operator surface over the same tenant and role checks as the
API. It is not a separate authority. Use service keys and the CLI/API for
automation.

## Control-plane HTTP API

Public process and discovery endpoints:

| Method and path | Purpose |
|---|---|
| `GET /healthz` | Process liveness |
| `GET /readyz` | Dependency-backed readiness |
| `GET /agents` | Tenant-filtered agent discovery |
| `GET /gateway/status` | Gateway status when configured |
| `POST /register` | One-time agent registration handshake |

Agent paths are proxied under `/agents/{id}/...`. The common operations are:

| Method and path | Purpose |
|---|---|
| `POST /agents/{id}/sessions` | Create a session |
| `GET /agents/{id}/sessions` | List sessions |
| `GET /agents/{id}/sessions/{sid}` | Read session state |
| `GET /agents/{id}/sessions/{sid}/stream?since=N` | Replay and follow SSE events |
| `GET /agents/{id}/sessions/{sid}/events` | Bounded JSON event replay |
| `POST /agents/{id}/sessions/{sid}/messages` | Send a follow-up turn |

Example:

```bash
BASE=http://localhost:8080
KEY=<operator-key>

SID=$(
  curl -sS -X POST "$BASE/agents/support/sessions" \
    -H "Authorization: Bearer $KEY" \
    -H "Content-Type: application/json" \
    -d '{"message":"hello"}' |
  jq -r .session_id
)

curl -sSN \
  -H "Authorization: Bearer $KEY" \
  "$BASE/agents/support/sessions/$SID/stream?since=0"
```

SSE records have monotonically increasing sequence numbers. Reconnect using the
last observed sequence. Do not assume the TCP connection itself is the durable
record.

Administrative endpoints are under `/admin` and mirror the CLI families:
tenants, users, keys, registration tokens, secrets, dynamic upstreams, dynamic
agents, policies, quotas, and evaluations. They are role- and tenant-checked.
Use the CLI implementation as a working client and inspect the handler package
for exact request/response structures while the API remains pre-release.

Fleet Prometheus metrics are not served by the public API. They are on the
private `RUNTIME_METRICS_ADDR` listener described in
[Observability](observability.md).

## Agent contract

The baseline contract served by every attached agent is:

| Endpoint | Requirement |
|---|---|
| `GET /healthz` | Cheap liveness response |
| `GET /readyz` | Readiness including required dependencies |
| `GET /meta` | Stable agent ID, name/model metadata as supported |
| `POST /sessions` | Create an invocation and return a session ID |
| `GET /sessions` | List sessions visible to this agent |
| `GET /sessions/{id}` | Return durable lifecycle state |
| `GET /sessions/{id}/stream?since=N` | SSE replay/follow |
| `GET /sessions/{id}/events` | Bounded JSON replay |

The Python contract shim additionally supports
`POST /sessions/{id}/messages` for follow-up turns. A native Go agent currently
has no follow-up-message route. Agents may expose `GET /metrics`; native agents
do, and the Python shim does when metrics are enabled. The control-plane fan-out
treats a missing agent metrics endpoint as a scrape skip rather than a contract
failure.

Protected agent routes, including `/metrics`, should require
`RUNTIME_AGENT_AUTH_TOKEN`; health and readiness remain usable by the
platform's probes. The control plane injects the token for managed agents and
applies it to remote calls when configured.

The contract is behavioural, not just a list of routes. Session identifiers
must be stable, streams must replay persisted events from `since`, terminal
events must agree with session state, and unknown sessions must return a clear
not-found response.

Run:

```bash
runtimectl conformance --agent <id>
```

before attaching a custom implementation. The conformance suite exercises the
contract through the control plane so it also catches proxy and authentication
integration problems.

## Native Go agent SDK

`agentruntime.Serve` turns a Harness agent specification into the HTTP/SSE
contract. Agent authors provide identity/model behaviour and tools; deployment
settings come from the environment injected by Runtime.

```go
package main

import (
	"context"
	"log"

	"github.com/sausheong/harness/llm"
	hrt "github.com/sausheong/harness/runtime"
	"github.com/sausheong/harness/tool"
	"github.com/sausheong/runtime/agentruntime"
)

func main() {
	registry := tool.NewRegistry()
	// registry.Register(myTool)

	cfg := agentruntime.Config{
		Spec: hrt.AgentSpec{
			ID:           "my-agent",
			Name:         "My Agent",
			Model:        "provider/model",
			SystemPrompt: "Help the user.",
			MaxTurns:     10,
		},
		Provider: buildProvider(),
		Tools:    registry,
	}

	if err := agentruntime.Serve(context.Background(), cfg); err != nil {
		log.Fatal(err)
	}
}

func buildProvider() llm.LLMProvider {
	panic("construct the selected provider")
}
```

`Spec.ID` and `Spec.Model` are required. `Serve` reads the listen address,
Postgres DSN, replica identity, agent auth token, limits, tenant, and metrics
context from the environment. Keeping these operator concerns out of the agent
source makes the same binary deployable locally or remotely.

The bundled `agentd` resolves `kind` through `internal/agentkind`. To add a
repository-native kind, create a builder returning `agentruntime.Config`,
register it in the kind registry, add a `runtime.yaml` entry, and cover it with
unit and conformance tests. Application-specific agents can instead build
their own binary and use `command`, avoiding a permanent fork of `agentd`.

## Foreign SDKs and generic agents

The reusable Python contract shim implements persistence, SSE replay, and
framework adapters for the OpenAI Agents SDK and Claude Agent SDK. A generic
process in any language may implement the contract directly.

Foreign shims do not automatically gain every native guarantee. In particular,
the supplied Python shim persists sessions and events in SQLite but does not
resume an SDK invocation that was in flight at process death, and native
lifecycle-limit enforcement is not injected into the SDK loop. See [Deploying
SDK agents](deploying-sdk-agents.md) for the full contract, adapter examples,
token telemetry, local conformance, and GCP deployment.

## Durability model

For native Go agents, a session maps to a DBOS workflow and a model/tool turn is
a checkpointed step:

```text
request
  -> run one turn
  -> persist completed step
  -> persist and publish events
  -> continue or finalise
```

On restart, completed steps are replayed and execution resumes at the first
incomplete turn. The owner replica uses a stable executor identity. Session
events are durable and use deterministic idempotency keys, so a reconnect can
replay the stored sequence.

The boundary is a completed turn, not an individual external side effect. If a
tool changes an external system and the process dies before the turn
checkpoint, recovery may call the tool again. Side-effecting tools must accept
an idempotency key or otherwise tolerate at-least-once execution.

Changing agent code, tools, or workflow structure while incomplete workflows
exist can change replay behaviour. Treat such changes like a schema migration:
test recovery against a production-like database, drain or complete active
sessions where possible, and keep rollback artefacts.

## Deployment choices

| Profile | Use |
|---|---|
| Local binaries plus Postgres | Development and contract work |
| Turnkey Compose | Complete single-host evaluation/operation |
| Secured host bundle | TLS and identity on cloud or on-prem hosts |
| Helm | Kubernetes-managed control plane and supporting services |
| Distributed remote agents | Separate lifecycle or framework-specific hosts |

Start with [Quickstart](quickstart.md). Production backup, secret, identity,
probe, monitoring, and upgrade checks are in the [Operator
guide](operator-guide.md). TLS-specific instructions are in [Secured
deployment](deploy/secured/README.md); Kubernetes values and probes are in the
[Helm guide](deploy/charts/runtime/README.md).

Multi-agent startup is readiness-gated and deliberately sequential where DBOS
first-run schema initialisation is unsafe concurrently. A failed managed agent
degrades its own availability rather than taking down unrelated agents.

## Testing

Fast local verification:

```bash
make fmt-check
make vet
make test
```

Database-backed verification:

```bash
make pg-up
make test-integration
```

Deployment and packaging checks:

```bash
make helm-lint
docker compose -f deploy/compose/docker-compose.yml config
```

CI additionally covers race-sensitive packages, `govulncheck`, Python contract
tests, Helm rendering, shell checks, container builds, and Compose validation.
Live Docker/browser tests and provider-backed examples require their external
dependencies and are not proofed by a unit-only run.

For documentation:

```bash
go test ./internal/doccheck
```

The documentation test validates tracked Markdown local links and balanced
fences. Stage new documentation before relying on its tracked-file inventory,
or inspect new files explicitly during the same change.

## Repository layout

| Path | Purpose |
|---|---|
| `cmd/runtimed` | Control-plane executable and assembly |
| `cmd/agentd` | Bundled native agent host |
| `cmd/runtimectl` | CLI |
| `cmd/sandboxd`, `cmd/browserd` | Isolated execution MCP servers |
| `agentruntime` | Native Go serving library |
| `controlplane` | Routing, proxying, admin, registration, and evaluation APIs |
| `internal/config` | Strict YAML and limit resolution |
| `internal/identity` | Tenants, users, service keys, registration tokens |
| `internal/secrets` | Encrypted tenant secret broker |
| `internal/gateway` | MCP/OpenAPI federation |
| `internal/policy`, `internal/quota` | Gateway enforcement |
| `internal/memory` | Durable memory and extraction strategies |
| `internal/eval` | Golden-set and online scoring |
| `internal/obs` | Metrics and tracing |
| `internal/sandbox`, `internal/browser` | Isolation backends and MCP tools |
| `contrib/shims/python` | Foreign-SDK contract library |
| `examples` | Worked agents |
| `deploy` | Compose, secured host, GCP, and Helm assets |

Runtime is pre-release. Exported Go types, YAML, CLI output, and administrative
JSON should be treated as evolving until a versioned compatibility policy is
published.
