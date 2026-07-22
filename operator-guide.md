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

## Sandbox & browser isolation

Agents that run code (the `sandbox` tool) or drive a headless browser (the
`browser` tool) execute untrusted work in sibling containers. The platform
offers three layers of isolation; they compose, and the first is on by default.

### 1. Session-scoped containers (the shipped default)

`RUNTIME_SANDBOX_SCOPE=session` and `RUNTIME_BROWSER_SCOPE=session` — the value
baked into the compose stack and the Helm chart — key each session's
sandbox/browser containers to the owning session. A session's containers are
**invisible to other sessions of the same tenant** and are **torn down at
session end**. This closes cross-session handle reuse and lingering state: one
session can never see, reuse, or leak into another's container.

Set the scope to `tenant` to share containers across a tenant's sessions
(longer-lived containers, warm reuse, but no per-session isolation):

```bash
RUNTIME_SANDBOX_SCOPE=tenant
RUNTIME_BROWSER_SCOPE=tenant
```

The scope env vars are set on `runtimed` and **inherited** by the sandboxd /
browserd stdio children it spawns — no per-server config is needed.

### 2. gVisor (`runsc`) for defense-in-depth

For a stronger boundary against container escape from executed code, run the
sandbox/browser containers under [gVisor](https://gvisor.dev), a userspace
kernel that intercepts syscalls. This is the **recommended runtime** where the
host supports it:

```bash
RUNTIME_SANDBOX_RUNTIME=runsc
RUNTIME_BROWSER_RUNTIME=runsc
```

It is **opt-in** because it requires `runsc` installed on the host and
registered with the Docker daemon (a `runtimes` entry in `/etc/docker/daemon.json`);
leave both unset on stock hosts. In the Helm chart set `sandbox.runtime` /
`browser.runtime` (empty by default) — the env vars are emitted only when
non-empty.

### 3. One-session-per-process for process-level separation

Tenants that demand process-level separation (not just container-level) can run
each session in its own process: set `replicas: N` on the agent. Each session
pins to one replica's DBOS executor, so sessions land on distinct processes;
combine with idle TTLs (see [Bound agent execution](#bound-agent-execution)) to
reclaim them.

### What the runtime does NOT provide: per-session microVMs

The runtime does **not** run each session in its own microVM (Firecracker-style
hardware virtualization). This is a **deliberate, documented trade**, not an
omission: a session pins to its owner replica's DBOS executor for durability —
its workflow state must recover on that executor after a crash. A per-session
microVM would move the session's execution off the executor that owns its
durable state, breaking the replay/recovery model that makes sessions durable.
Container-level isolation (layers 1–2) plus process-level separation (layer 3)
are the isolation guarantees the durability model can honestly support.

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

## Gateway quotas

Per-`(tenant, upstream)` request rate limiting for gateway tool calls — a
requests-per-minute token bucket that protects an upstream from a runaway or
noisy tenant. A quota is a triple `(tenant, upstream, rate_per_min)`; either key
may be the wildcard `*`.

**Most-specific-wins resolution.** For a call `(T, U)` the platform looks up the
effective limit in precedence order `(T,U) → (T,*) → (*,U) → (*,*)` and the
first match wins. The call then consumes **that one bucket** — never several. No
match at all means the call is unlimited (**permit by default**, like the policy
engine).

Configure quotas three ways, which compose:

- A top-level `quotas:` block in `runtime.yaml` — a list of
  `{tenant, upstream, rate_per_min}`:

  ```yaml
  quotas:
    - { tenant: acme, upstream: orders, rate_per_min: 60 }
    - { tenant: "*",  upstream: fragile, rate_per_min: 30 }   # protect one upstream from all tenants
  ```

- `RUNTIME_GATEWAY_QUOTA_DEFAULT` — an integer requests/min applied as the
  `(*,*)` floor (the catch-all every unmatched call falls through to).
- The DB store, managed at runtime via `POST /admin/quotas`,
  `runtimectl admin quota add|ls|rm`, and the console — with **live reload**: a
  change takes effect without restarting `runtimed` (up to ~2s, bounded by the
  limiter's refresh throttle).

  ```bash
  runtimectl admin quota add --tenant acme --upstream orders --rate 60
  runtimectl admin quota ls
  runtimectl admin quota rm --tenant acme --upstream orders
  ```

**Scope.** The **superuser is quota-exempt**. **Open-mode** (identity off) calls
are **skipped** entirely — a quota keys on the tenant, and open mode has no
principal to key on, so quotas require identity on.

**Rejection.** A call over its limit comes back to the agent as the MCP tool
error `quota exceeded: <tenant>/<upstream> (retry after Ns)` — **not** an HTTP
`429`. MCP has no per-call status channel, so the agent sees a tool error it can
reason about and back off. Rejections are counted in
`runtime_gateway_quota_rejections_total{tenant,server}`.

**RBAC.** A tenant-admin manages quotas only for its **own** tenant; the `*`
tenant wildcard is **superuser-only** (a tenant-admin naming `*` is rejected
`400`).

**Fail-open — the deliberate opposite of the policy engine.** A runtime error
reading the quota store is treated as **no limit** (calls flow). A rate limiter
is an **availability** control, so a broken quota backend must never block all
gateway traffic. This is the intentional inverse of the [policy
engine](#policy-engine-cedar), which fails **closed** — a *security* control
must not fail open. The asymmetry is by design: pick fail-open for availability
controls, fail-closed for security controls. Note the one boot-time exception: a
**malformed** `quotas:` block (a non-positive `rate_per_min`) is a **boot
failure**. A malformed `RUNTIME_GATEWAY_QUOTA_DEFAULT` is *not* fatal — it is
logged and ignored, falling back to no floor. An *absent* quota config is valid
and simply means unlimited.

### Header enrichment

The gateway can inject request-time identity into an upstream's outbound headers
so a REST backend can see *who* is calling. Add a per-upstream `enrich:` map —
principal **claim → outbound header name** — and the platform stamps those
headers on each call from the calling principal:

```yaml
gateway:
  servers:
    - name: orders
      openapi: http://host.docker.internal:9000/openapi.yaml
      base_url: http://host.docker.internal:9000
      enrich:
        tenant:  X-Runtime-Tenant
        subject: X-Runtime-User
```

- **Fixed claim vocabulary.** The map keys are exactly `tenant`, `subject`, and
  `role` — no other claims exist.
- **OpenAPI-only.** `enrich` is valid **only** on `openapi:` upstreams;
  configuring it on any other upstream is a **config load error**. The reason is
  architectural: an MCP-over-HTTP (`url:`) upstream sets its headers once at
  connect into a long-lived session, so per-call header variation is impossible
  there. Only the REST/OpenAPI adapter issues a fresh HTTP request per call, so
  only it can vary headers per principal.
- **Platform claims overwrite.** An enriched header **overwrites** any
  caller- or agent-supplied header of the same name — an agent can never spoof a
  claim.
- **`X-Runtime-*` is the reserved namespace** for platform claims. Targeting a
  non-`X-Runtime-` header logs a load **WARNING** (not an error), so operators
  can still match a real backend's expected header names when they must.
- **Collisions fail fast.** An `enrich` header that collides with the upstream's
  `cred_header` or a static `headers:` entry is a **config load error** — no
  runtime ambiguity about which value wins.

## OAuth2 outbound credentials

An OpenAPI upstream can authenticate to its backend with an OAuth2
`client_credentials` access token that the platform mints, caches, and
auto-refreshes — instead of a static API key. Create the credential once
(type `oauth2_client_credentials`), then point an upstream at it with the
existing `cred_secret` / `cred_header`:

```bash
runtimectl admin secret set-oauth2 \
  --name orders_oauth --token-url https://idp.example.com/oauth/token \
  --client-id svc-orders --client-secret "$SECRET" --scope orders.read
  # optional: --audience https://api.example.com --tenant acme
```

An upstream references it exactly like a static credential — the minted token
becomes the header value `Bearer <token>` (`cred_header` defaults to
`Authorization`):

```yaml
gateway:
  servers:
    - name: orders
      openapi: http://host.docker.internal:9000/openapi.yaml
      base_url: http://host.docker.internal:9000
      cred_secret: orders_oauth   # resolves to "Bearer <minted-token>"
      cred_header: Authorization  # default; shown for clarity
```

- **OpenAPI-only.** An oauth2 credential is valid only on an `openapi:` upstream
  (a `url:`/MCP upstream sets headers once at connect and cannot refresh a token
  per call). It is **rejected when you attach it to a non-OpenAPI upstream** —
  at registration (admin API/console, when the broker is reachable) and, for a
  file-config upstream, fatally at startup — and it is **refused at dial** (fail
  closed) if it ever reaches a non-openapi path.
- **Fail closed.** If the token endpoint is unreachable or erroring, the tool
  call is rejected with `credential unavailable: <name>` and counted in
  `runtime_gateway_credential_errors_total{tenant,server}` — the request is
  never sent unauthenticated. This is the deliberate **opposite** of the
  fail-open [quota limiter](#gateway-quotas): a credential is a **security**
  control (like the [policy engine](#policy-engine-cedar)), so it must not fail
  open.
- **Live rotation.** Re-run `secret set-oauth2` to rotate the client secret or
  scopes; the change takes effect **without a restart**. Within a generation the
  access token auto-refreshes on its TTL.
- **`client_secret` is write-only** — it never appears in `secret ls`, the
  `/admin/secrets` API, the console, or any log line.

On-behalf-of (RFC 8693 user-token exchange) is **not** included in this
release; only the `client_credentials` (service-to-service) grant is supported.

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
- **Credential errors** — query `runtime_gateway_credential_errors_total`
  (labelled `tenant`/`server`) to see gateway tool calls that failed closed
  because an outbound OAuth2 credential could not be minted. See [OAuth2
  outbound credentials](#oauth2-outbound-credentials).

For the complete configuration and metrics reference, see [README.md](README.md).
