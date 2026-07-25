# Configuration reference

Runtime has two configuration layers:

- `runtime.yaml` describes agents, gateway upstreams, quotas, and model prices.
- Environment variables describe the deployment, credentials, feature tuning,
  and defaults inherited by agents.

The loader rejects unknown YAML fields, multiple YAML documents, duplicate or
unsafe identifiers, invalid durations, colliding addresses, and incompatible
local/remote settings. Configuration errors fail startup instead of being
silently ignored.

## Agent entries

```yaml
agents:
  - id: support
    name: Support Agent
    model: openai/gpt-5.4
    listen_addr: 127.0.0.1:8101
    kind: testagent
    tenant: default
    replicas: 2
    memory: true
    gateway: search
    limits:
      turn_timeout: 2m
      session_timeout: 30m
      max_turns: 20
      max_tokens: 100000
```

`id`, `name`, and `model` are required. Exactly one of `listen_addr` or `url`
is required. Identifiers are 1–64 characters and may contain letters, digits,
`.`, `_`, and `-`; the first character must be alphanumeric.

Local-agent fields:

| Field | Meaning |
|---|---|
| `listen_addr` | Base host and port; pool replica `i` uses base port + `i` |
| `kind` | Bundled Go agent builder; omitted means `testagent` |
| `command` | Argument array for a custom or foreign-SDK process instead of `agentd` |
| `workdir` | Working directory for `command` |
| `memory` | Attach tenant-scoped durable memory |
| `gateway` | `false`, `true`/`full`, or `search` |
| `replicas` | Static pool size; omitted or zero means one |
| `autoscale` | Dynamic local pool policy; replaces `replicas` |

Remote-agent fields:

```yaml
agents:
  - id: remote-support
    name: Remote Support
    model: vendor/model
    url: https://agent.internal:8443
    auth_token: ${REMOTE_AGENT_TOKEN}
    tenant: acme
```

A remote agent is health-checked and proxied but not spawned. It must not set
spawn-only fields such as `command`, `workdir`, `kind`, `memory`, `gateway`, or
`autoscale`. `auth_token` is expanded from the operator environment and used
on the control-plane-to-agent hop.

A fixed remote pool uses an ordinal URL:

```yaml
  - id: remote-pool
    name: Remote Pool
    model: vendor/model
    url: http://agent-{i}.runtime.internal:8080
    replicas: 3
```

The `{i}` placeholder is required when a remote has more than one replica and
forbidden for a single remote.

## Pools and session affinity

New sessions are assigned to a reachable, non-draining replica. Once created,
a session remains bound to its owner. Session status, stream, event, and
message requests return `503` while that owner is unavailable; they are not
sent to a replica that cannot safely resume the work.

Local replica identity is stable across restart. Replica `i` receives:

- `RUNTIME_AGENT_REPLICA=i`;
- a derived listen port;
- a stable DBOS executor identity.

This is what lets a restarted native agent recover its own completed-turn
checkpoints.

## Autoscaling

Autoscaling is available for local agents:

```yaml
agents:
  - id: elastic
    name: Elastic Agent
    model: test/scripted
    listen_addr: 127.0.0.1:8200
    autoscale:
      min: 1
      max: 8
      target_sessions_per_replica: 10
```

The desired pool size is the active-session count divided by the target,
rounded up and clamped to `[min,max]`. Scale-down first marks a replica as
draining, stops assigning new sessions to it, and waits for its active
sessions to finish. Cooldowns and polling can be tuned with:

| Variable | Meaning |
|---|---|
| `RUNTIME_AUTOSCALE_POLL_SECONDS` | Policy-loop interval override |
| `RUNTIME_AUTOSCALE_UP_COOLDOWN_SECONDS` | Minimum time between scale-up actions |
| `RUNTIME_AUTOSCALE_DOWN_COOLDOWN_SECONDS` | Minimum time between scale-down actions |

Setting both `replicas` and `autoscale` is accepted with a warning;
`autoscale.min` determines the initial size.

## Lifecycle limits

Limits can be set per agent or supplied as platform defaults:

| YAML field | Environment default | Zero means |
|---|---|---|
| `turn_timeout` | `RUNTIME_LIMIT_TURN_TIMEOUT` | Unlimited |
| `session_timeout` | `RUNTIME_LIMIT_SESSION_TIMEOUT` | Unlimited |
| `max_turns` | `RUNTIME_LIMIT_MAX_TURNS` | Unlimited |
| `max_tokens` | `RUNTIME_LIMIT_MAX_TOKENS` | Unlimited |

An explicit YAML value wins, including zero. Durations use Go syntax such as
`30s`, `2m`, or `1h`. A turn timeout cannot exceed a non-zero session timeout.
Native agents terminate a breach as `limit_exceeded`, emit a terminal error
event naming the limit, and increment
`agent_session_limit_hits_total{agent,limit}`. The session deadline is
wall-clock based, so downtime still consumes it.

## Model pricing

Pricing is optional and does not affect token metering:

```yaml
pricing:
  currency: USD
  models:
    openai/gpt-5.4:
      input: 2.50
      output: 10.00
      cache_read: 0.25
      cache_write: 2.50
```

Values are per million tokens; `currency` documents the unit and should be
`USD` for the exported `agent_cost_usd_total` metric. `cache_write` defaults to
the input price and `cache_read` defaults to zero. A priced model contributes to
`agent_cost_usd_total`; an unpriced turn increments
`agent_cost_unpriced_total` so missing prices remain visible.

## Gateway and quotas

```yaml
gateway:
  self_url: http://127.0.0.1:8080
  agent_keys:
    acme: ${ACME_AGENT_KEY}
  servers:
    - name: docs
      command: ./bin/docs-mcp
      args: ["--root", "./docs"]
      tenants: [acme]
      forward_tenant: true
    - name: weather
      url: https://mcp.example.com/mcp
      headers:
        Authorization: Bearer ${WEATHER_TOKEN}
    - name: orders
      openapi: ./orders.openapi.yaml
      base_url: https://orders.example.com
      operations: ["GET /orders/*"]

quotas:
  - tenant: acme
    upstream: weather
    rate_per_min: 60
```

Each server uses exactly one transport: `command`, `url`, or `openapi`.
Gateway server names must be unique and cannot contain `__`, which is reserved
for the `<server>__<tool>` namespace. See [Gateway and
sandboxes](gateway-and-sandboxes.md) for trust and credential behaviour.

## Control-plane environment

| Variable | Default or purpose |
|---|---|
| `RUNTIME_CONFIG` | `runtime.yaml` |
| `RUNTIME_PG_DSN` | Control-plane Postgres DSN |
| `RUNTIME_AGENT_PG_DSN` | Restricted DSN injected into managed local agents; defaults to the control-plane DSN |
| `RUNTIME_CTL_ADDR` | Public API listener, default `:8080` |
| `RUNTIME_METRICS_ADDR` | Private management listener, default `127.0.0.1:9091` |
| `RUNTIME_AGENTD_BIN` | Managed Go agent host, default `./agentd` |
| `RUNTIME_AGENT_ENV_PASSTHROUGH` | Comma-separated additional child variables |
| `RUNTIME_LOG_FORMAT` | Set to `json` for structured logs |
| `RUNTIME_PUBLIC_URL` | External URL used for secure-cookie inference |
| `RUNTIME_COOKIE_SECURE` | Explicit secure-cookie override |
| `RUNTIME_OIDC_ISSUER` | OIDC issuer URL |
| `RUNTIME_OIDC_CLIENT_ID` | OIDC client ID |
| `RUNTIME_OIDC_CLIENT_SECRET` | OIDC client secret |
| `RUNTIME_OIDC_REDIRECT_URL` | Explicit OIDC callback URL |
| `RUNTIME_ADMIN_BOOTSTRAP` | Initial superuser credential |
| `RUNTIME_ADMIN_BREAK_GLASS` | Re-enable bootstrap after durable credentials exist |
| `RUNTIME_SECRETS_KEYS` | AES keyring |
| `RUNTIME_SECRETS_PRIMARY` | Primary keyring ID |
| `RUNTIME_SECRETS_KEY` | Legacy single-key configuration |
| `RUNTIME_POLICY_ENABLED` | Enable Cedar enforcement |
| `RUNTIME_POLICY_FILE` | Platform Cedar policy file |
| `RUNTIME_GATEWAY_QUOTA_DEFAULT` | Default calls per minute when no narrower quota exists |
| `RUNTIME_EVAL_RETENTION` | Evaluation retention, default `720h` |

Identity, memory, gateway, sandbox, evaluation, and telemetry variables are
documented in their corresponding topic guides. Variables beginning
`RUNTIME_AGENT_`, `RUNTIME_GATEWAY_`, `RUNTIME_LISTEN_ADDR`, and registration
variables may be injected into child agents; operators should not put them in
`RUNTIME_AGENT_ENV_PASSTHROUGH`.

## Runtime-injected agent environment

Managed agents receive a minimal environment plus explicit safe passthrough:

| Variable | Meaning |
|---|---|
| `RUNTIME_AGENT_ID` | Configured agent ID |
| `RUNTIME_AGENT_KIND` | Bundled Go builder |
| `RUNTIME_AGENT_TENANT` | Owning tenant |
| `RUNTIME_AGENT_REPLICA` | Stable zero-based replica ordinal |
| `RUNTIME_LISTEN_ADDR` | Concrete bind address |
| `RUNTIME_AGENT_PG_DSN`/`RUNTIME_PG_DSN` | Agent database credential |
| `RUNTIME_AGENT_MEMORY` | Memory opt-in |
| `RUNTIME_AGENT_LIMITS` | Resolved JSON limits |
| `RUNTIME_AGENT_PRICING` | Resolved JSON model price |
| `RUNTIME_AGENT_AUTH_TOKEN` | Bearer required on protected agent routes |
| `RUNTIME_GATEWAY_URL`/`RUNTIME_GATEWAY_KEY` | Platform gateway connection |
| `RUNTIME_EVAL_POLICY` | Validated native online-evaluation policy |

Tenant secrets are added by name. Store provider keys in the secrets broker or
pass only explicitly approved non-reserved variables. Do not give child agents
the full control-plane environment.

## Postgres schema

Runtime creates and migrates its tables at startup under a database advisory
lock. The schema covers identity, service and registration keys, encrypted
secrets, dynamic agents and upstreams, policies and quotas, control-plane
session ownership, durable events, memory, DBOS workflow state, transcripts,
golden sets, and evaluation results.

Back up the complete database, not selected tables. Schema changes and binary
rollbacks should be treated as an operator-controlled deployment event. See the
[operator guide](operator-guide.md) for backup, restore, and migration checks.
