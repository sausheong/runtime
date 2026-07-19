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

## Cost metering

Attach dollar prices to models and the platform meters per-turn LLM cost. Add a
top-level `pricing:` block to `runtime.yaml`, keyed by the exact
`provider/model` string the agent uses:

```yaml
pricing:
  currency: USD
  models:
    anthropic/claude-opus-4-8: { input: 15.00, output: 75.00, cache_write: 18.75, cache_read: 1.50 }
    openai/gpt-4o:             { input: 2.50,  output: 10.00 }
```

- Prices are **$ per million tokens**. Matching is **exact** on the full
  `provider/model` key — no prefix or wildcard fallback.
- `cache_write` defaults to `input` and `cache_read` defaults to `0` when omitted.
- The block is **optional**. An absent or empty `pricing:` means every model is
  unpriced — tokens still flow, only dollar cost is not computed.
- A **malformed** block (a negative, NaN, or Inf price) is a **boot failure**
  (fail-closed: a broken price table must never silently meter wrong costs).
- A model with no price entry never stalls the agent: its turns still run and
  still emit tokens, plus `agent_cost_unpriced_total` and one boot log line
  naming the unpriced model.

Three agent metrics carry the accounting (all `agent_*`, merged into runtimed's
exposition — see [README.md](README.md) for the full inventory):

- `agent_tokens_total{agent,tenant,model,direction}` — tokens by direction
  (`input`/`output`/`cache_creation`/`cache_read`).
- `agent_cost_usd_total{agent,tenant,model}` — dollar cost per turn (tokens ×
  per-model price, **cache included**), emitted only for priced models.
- `agent_cost_unpriced_total{agent,tenant,model}` — +1 per turn when the model
  has no price entry (a cost blind spot; alertable via `UnpricedModelUsage`).

Per session, cumulative `tokens_total` and `cost_usd` are persisted and shown in
the session API (`GET /agents/<id>/sessions` and `.../sessions/<id>`) and the
console session view.

Two caveats to keep in mind:

- **Metering-grade, not billing-grade.** Cost is a `float64` sum for
  observability and rough budgeting — do not reconcile invoices against it.
- **Cost includes cache tokens; the `max_tokens` budget does not.** The P1.2
  lifecycle `max_tokens` guardrail counts only input + output, so a session's
  metered cost can reflect cache traffic that never counted toward its token
  budget.

## Alerting

The compose stack ships an **Alertmanager** service on `:9093`. Prometheus loads
every rule file under `deploy/compose/rules/*.rules.yml` and forwards firing
alerts to Alertmanager.

The bundled `rules/runtime.rules.yml` has seven starter rules:

| Alert | Fires when | Severity |
|---|---|---|
| `AgentDown` | an agent replica's `/metrics` is unreachable for 2m | critical |
| `GatewayUpstreamDown` | a gateway upstream is unreachable for 2m | warning |
| `HighTurnErrorRate` | an agent's turn error ratio exceeds 20% over 10m | warning |
| `SessionLimitSpike` | lifecycle limits trip >5 times in 15m | warning |
| `PolicyDenySpike` | a tenant sees >10 gateway policy denials in 15m | warning |
| `UnpricedModelUsage` | a model runs with no price entry (cost blind spot) | warning |
| `CostBurnHigh` | *(commented)* a tenant's projected spend rate is high | warning |

`CostBurnHigh` is shipped **commented** because its dollars-per-hour threshold
is deployment-specific — uncomment it in the rules file and tune the threshold
for your fleet before enabling.

By default alerts route to a **null receiver** (`deploy/compose/alertmanager.yml`),
so the stack is self-contained and provable without an external notifier. To
wire a real one: uncomment the Slack or PagerDuty block in `alertmanager.yml`,
fill in its `api_url`/`routing_key`, and point `route.receiver` at that
receiver's name. Restart Alertmanager to apply.

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
- **Cost** — query `agent_cost_usd_total` and `agent_tokens_total` (both labelled
  `tenant`/`model`) for spend and token burn; `agent_cost_unpriced_total` flags
  models running without a price. See [Cost metering](#cost-metering).

For the complete configuration and metrics reference, see [README.md](README.md).
