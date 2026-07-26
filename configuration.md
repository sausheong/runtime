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
    registration_generation: 89ef9a06-e752-49e2-a8bc-a9b14983dc6f
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

`registration_generation` is also required for every agent. Use an opaque
persisted value such as a UUID. Keep it unchanged for ordinary process,
control-plane, and host restarts. Rotate it when deleting/recreating an agent,
replacing its endpoint or database trust domain, or intentionally retiring its
sessions and registration credentials. Runtime never derives this lifecycle
identity from a reusable tenant/agent ID.

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
    registration_generation: ${REMOTE_AGENT_GENERATION}
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
    registration_generation: ${REMOTE_POOL_GENERATION}
```

The `{i}` placeholder is required when a remote has more than one replica and
forbidden for a single remote.

## Pools and session affinity

New sessions are assigned to a reachable, non-draining replica. Once created,
a session remains bound to its owner. Session status, stream, event, and
message requests return `503` while that owner is unavailable; they are not
sent to a replica that cannot safely resume the work.

The durable binding contains tenant, agent ID, generation, and replica.
Generation-less legacy bindings and unknown sessions fail closed with `404`,
including single-replica remote and command agents. The control plane pins the
exact selected lifecycle snapshot while forwarding, so replacement,
disablement, and autoscale reaping cannot invalidate an in-flight target.

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
    registration_generation: 89ef9a06-e752-49e2-a8bc-a9b14983dc6f
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
| `RUNTIME_AGENT_PG_DSN` | Restricted DSN injected into locally managed agents; required with identity and bound to one local-agent trust domain. Attach-only remote agents do not receive it or participate in role provisioning |
| `RUNTIME_PROVISION_REMOTE_AGENT_ROLE` | Set to `1` only when one configured remote agent receives `RUNTIME_AGENT_PG_DSN` through registration or an independently managed pod; the Helm `perAgentPods` mode sets this automatically |
| `RUNTIME_IDENTITY_SIGNING_PRIVATE_KEY` | URL-safe base64 Ed25519 seed/private key used only by the control plane when subject forwarding is enabled |
| `RUNTIME_IDENTITY_SIGNING_PUBLIC_KEY` | Matching URL-safe base64 Ed25519 public key injected into agents for verification |
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
| `RUNTIME_SESSION_RETENTION` | Terminal session/event retention, default `720h`; `0` disables |
| `RUNTIME_SESSION_RETENTION_BATCH` | Maximum sessions removed per statement, default `500`; each sweep runs at most 20 batches within 30 seconds |
| `RUNTIME_SESSION_RETENTION_DRY_RUN` | Count and log eligible sessions without deletion |
| `RUNTIME_MEMORY_GC_ENABLED` | Enable dead-memory GC for memory-enabled local agents; default on |
| `RUNTIME_MEMORY_GC_INTERVAL` | Memory GC and live-retention sweep interval, default `1h` |
| `RUNTIME_MEMORY_GC_GRACE` | Age required before dead memory is deleted, default `24h` |
| `RUNTIME_MEMORY_GC_BATCH` | Maximum memory rows removed per statement, default `1000` |
| `RUNTIME_MEMORY_RETENTION_FACT` | Optional live fact-memory retention duration; unset disables |
| `RUNTIME_MEMORY_RETENTION_SUMMARY` | Optional live summary-memory retention duration; unset disables |
| `RUNTIME_MEMORY_RETENTION_EPISODE` | Optional live episodic-memory retention duration; unset disables |
| `RUNTIME_MEMORY_RETENTION_DRY_RUN` | Count and log eligible live-memory rows without deletion |
| `RUNTIME_MAX_REQUESTS` | Control-plane concurrent request cap, default `512` |
| `RUNTIME_MAX_STREAMS` | Control-plane concurrent SSE cap, default `128` |

Identity, memory, gateway, sandbox, evaluation, and telemetry variables are
documented in their corresponding topic guides. Variables beginning
`RUNTIME_AGENT_`, `RUNTIME_GATEWAY_`, `RUNTIME_LISTEN_ADDR`, and registration
variables may be injected into child agents; operators should not put them in
`RUNTIME_AGENT_ENV_PASSTHROUGH`.

The memory maintenance and agent HTTP-limit variables above are explicitly
included in the safe environment inherited by locally managed agents. Remote
agents must receive them through their own deployment environment. The supplied
Compose profiles expose these as `${VARIABLE:-default}` settings. The Helm chart
maps them through `runtime.*` and `agent.*` values, including live-memory
retention and dry-run, so operators do not need to edit manifests.

When signed subject forwarding is enabled, an agent keeps at most 131,072
recent request nonces in two rotating validity buckets. Replay lookup is
constant-time. Duplicate nonces and new signed requests received while the
cache is at capacity fail closed with `401`; health and readiness probes remain
exempt.

## Runtime-injected agent environment

Managed agents receive a minimal environment plus explicit safe passthrough:

| Variable | Meaning |
|---|---|
| `RUNTIME_AGENT_ID` | Configured agent ID |
| `RUNTIME_AGENT_KIND` | Bundled Go builder |
| `RUNTIME_AGENT_TENANT` | Owning tenant |
| `RUNTIME_AGENT_GENERATION` | Immutable configured agent lifecycle generation |
| `RUNTIME_AGENT_REPLICA` | Stable zero-based replica ordinal |
| `RUNTIME_DBOS_SCHEMA` | Tenant/agent-isolated durable workflow schema |
| `RUNTIME_LISTEN_ADDR` | Concrete bind address |
| `RUNTIME_AGENT_PG_DSN`/`RUNTIME_PG_DSN` | Agent database credential |
| `RUNTIME_AGENT_MEMORY` | Memory opt-in |
| `RUNTIME_AGENT_LIMITS` | Resolved JSON limits |
| `RUNTIME_AGENT_PRICING` | Resolved JSON model price |
| `RUNTIME_AGENT_AUTH_TOKEN` | Bearer required on protected agent routes |
| `RUNTIME_GATEWAY_URL`/`RUNTIME_GATEWAY_KEY` | Platform gateway connection |
| `RUNTIME_EVAL_POLICY` | Validated native online-evaluation policy |
| `RUNTIME_EVAL_SCORE_WORKERS` | Online-scoring worker count, default `2` |
| `RUNTIME_EVAL_SCORE_QUEUE` | Online-scoring queue capacity, default `64` |
| `RUNTIME_EVAL_SCORE_TIMEOUT` | Per-session scoring deadline, default `2m` |
| `RUNTIME_AGENT_MAX_REQUESTS` | Agent HTTP concurrency cap, default `256` |
| `RUNTIME_AGENT_MAX_STREAMS` | Agent SSE concurrency cap, default `64` |
| `RUNTIME_TRANSCRIPT_CAPTURE` | Transcript capture; default on, set `0` to disable |

Tenant secrets are added by name. Store provider keys in the secrets broker or
pass only explicitly approved non-reserved variables. Do not give child agents
the full control-plane environment.

## Postgres schema

Runtime applies ordered, transactional component migrations under a database
advisory lock and records them in `runtime_schema_migrations`. Restricted
agents validate the complete core ledger and its checksums plus required
tables, foreign keys, tenant-integrity triggers, RLS state, and policies
without applying control-plane
DDL. The schema covers identity, service and registration keys, encrypted
secrets, dynamic agents and upstreams, policies and quotas, control-plane
session ownership, durable events, memory, DBOS workflow state, transcripts,
golden sets, and evaluation results.

Registration-token schema upgrades fail closed: legacy tokens that recorded
only an agent ID are not inferred from the agent's current tenant. Mint
replacement tokens after upgrading. New tokens bind tenant plus a persisted
agent-instance generation; deleting and recreating a managed agent invalidates
the former generation even when its ID is reused.

Back up the complete database, not selected tables. Schema changes and binary
rollbacks should be treated as an operator-controlled deployment event. See the
[operator guide](operator-guide.md) and [release guide](RELEASING.md) for
backup, restore, upgrade, and rollback checks.
