# Operator guide — Runtime turnkey self-host

For the person who runs the platform. Assumes you brought it up via the
[Quickstart](quickstart.md).

## Log in to the console

`make compose-init` generated `deploy/compose/.env` with a one-time superuser
bootstrap key. From the repo root (`runtime/`):

```bash
grep RUNTIME_ADMIN_BOOTSTRAP deploy/compose/.env
```

Open http://localhost:8080/ui and log in with that key. Treat it as root — it is
the break-glass superuser credential. Use it to create the first tenant and
tenant-admin (see the [Tenant guide](tenant-guide.md)).

## Ports

| Service | Host port |
|---|---|
| Control plane / console | 8080 |
| Prometheus | 9090 |
| Grafana | 3000 |
| Jaeger UI | 16686 |
| OTLP HTTP (collector) | 4318 |

Postgres is **not** published to the host (reachable only inside the compose
network).

## Persistence & reset

- Data lives in the `pgdata` named volume.
- `docker compose down` **preserves** data; `docker compose up` resumes it.
- `docker compose down -v` **wipes** the volume (and re-runs the pgvector
  extension init on the next `up`) — a clean reset. (`make compose-reset` does
  the same from the repo root.)

## Security posture (single-node trust)

- runtimed mounts the host Docker socket to launch sandbox/browser containers.
  That is **root-equivalent on the host** — run this stack only on a trusted
  single node, not on untrusted/shared infrastructure.
- Secrets (bootstrap key, AES key, tenant credentials) are never written to logs.
- The bundled stack runs with identity ON; the console and APIs require auth.

## Bound agent execution

Bound what any single session may consume. All limits are optional: an
absent field falls back to the platform default (if any), while an explicit
`0` always means unlimited, overriding any platform default. Set platform
defaults on `runtimed` to prevent a runaway native agent
from holding a turn indefinitely or consuming an unbounded session budget:

```bash
RUNTIME_LIMIT_TURN_TIMEOUT=2m
RUNTIME_LIMIT_SESSION_TIMEOUT=30m
RUNTIME_LIMIT_MAX_TURNS=50
RUNTIME_LIMIT_MAX_TOKENS=200000
```

Override any field for one agent with its `limits:` block in `runtime.yaml`:

```yaml
agents:
  - id: support
    listen_addr: ":9101"
    limits:
      turn_timeout: 120s    # one model+tool turn
      session_timeout: 30m  # whole session, wall clock from start
      max_turns: 50         # loop iterations
      max_tokens: 200000    # cumulative input+output tokens
```

An agent opts out of a platform default with an explicit `0` (`turn_timeout:
0s`). A breached session terminates with status `limit_exceeded` and a final
`error` event naming the limit, e.g.
`limit exceeded: max_tokens (150231/100000)`. Breaches are counted in
`agent_session_limit_hits_total{agent,limit}`. `limits:` is valid on remote
(`url:`) agents — enforcement runs inside the agent process. Session downtime
during a crash/restart counts against `session_timeout`.

Limits are process-lifetime constants for durable Go agents. Drain in-flight
sessions before changing them and restarting an agent; adding or removing a
session timeout changes the durable DBOS step sequence and is not replay-safe
— an in-flight session recovered under changed limits may terminate with
status `error` (fail-closed). On Kubernetes/remote scheduled agents, an
operator-set `RUNTIME_AGENT_LIMITS` in the pod environment takes precedence
when the control plane sends an empty value (the registration handshake skips
empty entries).
The Python contract shim does not yet enforce these native limits, so bound
foreign SDK agents in their framework or process supervisor.

## Policy engine (Cedar)

Deterministic per-call authorization for gateway tools — the LLM is never the
authorization arbiter. Enable with either:

    RUNTIME_POLICY_ENABLED=1              # tenant-layer policies only
    RUNTIME_POLICY_FILE=platform.cedar    # + operator guardrails (implies enabled)

Two layers, and a `forbid` in either denies the call:

- **Platform layer** — the `.cedar` file above. Loaded at boot, immutable at
  runtime, applied to every tenant AND to superuser/open-mode calls. A parse
  error in this file is a **boot failure** (fail-closed: a broken guardrail
  file must never mean "no guardrails").
- **Tenant layer** — per-tenant policies managed by tenant admins (console →
  Gateway policies, `POST /admin/policies`, `runtimectl admin policy`).

With the engine on and no policies, every call is allowed (**permit by
default**); policies subtract. A denied call returns the MCP tool error
`forbidden by policy: <id>` (e.g. `platform/0` or `tenant/<name>`). Decisions
are counted in `runtime_gateway_policy_decisions_total{tenant,decision}` and
denials are audit-logged — never with argument values or policy text. An
evaluation error also denies (fail-closed).

Policies see `principal` (`Runtime::Key`, attrs `tenant`/`subject`/`role`/
`superuser`), `resource` (`Gateway::Tool`, attrs `server`/`tool`), and
`context.input` — the tool-call arguments as the agent sent them (before any
platform tenant injection). Example platform guardrail — block shell-
destructive sandbox code for everyone, in every tenant:

    forbid (principal, action == Gateway::Action::"call_tool", resource)
    when { resource.server == "sandbox" &&
           context.input has code &&
           context.input.code like "*rm -rf*" };

A tenant admin cannot weaken a platform forbid: Cedar's forbid-overrides rule
means a tenant `permit` can never re-enable a platform-forbidden call.

## Observability

- **Grafana** http://localhost:3000 (anonymous viewer) — the runtime dashboard.
- **Prometheus** http://localhost:9090 — `runtimed` is a scrape target at
  `/metrics`.
- **Jaeger** http://localhost:16686 — distributed traces for control-plane
  requests.
- **Lifecycle limits** — query `agent_session_limit_hits_total` in Prometheus
  to find sessions terminated by timeout, turn, or token guardrails.
- **Policy decisions** — query `runtime_gateway_policy_decisions_total` to see
  gateway allow/deny/error counts per tenant; denials are also audit-logged.

For the complete configuration and metrics reference, see [README.md](README.md).
