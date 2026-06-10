# Gateway M1 — MCP Federation Core

**Date:** 2026-06-10
**Status:** Approved design, pre-implementation
**Sub-project:** B1 Gateway (sub-project 2 of 6 from the runtime-spine decomposition)
**Builds on:** Runtime spine M1–M3, Identity M1 (edge auth), harness `tools/mcp`

## 1. Context & purpose

The Gateway turns the runtime into a tool hub: one central MCP endpoint that
federates many upstream tool servers, so agents (runtime-hosted or external)
configure **one** URL + key and get a curated, tenant-scoped tool catalog.
This is the AgentCore-Gateway-equivalent piece of the platform.

M1 delivers the **federation core only**: a Streamable HTTP MCP endpoint on the
existing control plane that connects to statically configured upstream MCP
servers, re-exposes their tools namespaced, enforces tenant visibility via
Identity, degrades gracefully on upstream failure, and is consumed by runtime
agents through an opt-in flag. REST→tool adapters, semantic tool search, and
dynamic registration are later Gateway milestones.

## 2. Decisions (settled during brainstorming)

| Decision | Choice | Rationale |
|---|---|---|
| M1 scope | Federation core only | Small proven milestone cadence; REST adapters / semantic search are M2/M3 |
| Process shape | Inside `runtimed` (`internal/gateway` + control-plane routes) | Single-binary story preserved (like Identity/Memory); reuses identity middleware; can split out later |
| Upstream registration | Static `runtime.yaml` (`gateway.servers:`) | Matches agent registration; dynamic registration deferred like A3 |
| Auth & tenancy | Service keys (Bearer), tenant-filtered tool visibility | Reuses Identity M1 verbatim; per-upstream `tenants:` allowlist |
| Agent consumption | `gateway: true` opt-in per agent; platform injects env | Zero agent-code changes, same pattern as `memory: true` |
| Failure model | Degrade + reconnect (never fail startup, never crash on a dead upstream) | Platform convention (Memory M2/M3 degrade-don't-fail) |
| Transports | Both stdio (`command:`) and Streamable HTTP (`url:`) upstreams | harness `mcp.Connect` already does both — nearly free |
| Internal currency | harness `tool.Tool` (Approach 1) | Reuse `mcp.Connect` + adapter; REST adapters later are just more `tool.Tool`s |

## 3. Architecture

```
                          ┌────────────────────── runtimed ──────────────────────┐
 caller (svk-… Bearer) ──▶ IdentityMiddleware ─▶ POST /gateway/mcp               │
                          │                       │ StreamableHTTPHandler         │
                          │                       ▼ getServer(r) → per-tenant     │
                          │                  ┌─ mcp.Server (tenant view) ─┐       │
                          │                  │ fs__read_file              │       │
                          │                  │ search__query              │       │
                          │                  └────────────┬───────────────┘       │
                          │                               ▼                       │
                          │                    internal/gateway.Manager           │
                          │            ┌──────────┬──────────────┐                │
                          │            ▼          ▼              ▼                │
                          │      upstream "fs"  upstream "search"  …              │
                          │      (stdio child)  (Streamable HTTP)                 │
                          └───────────────────────────────────────────────────────┘
```

Components, each independently testable:

- **`internal/gateway.Manager`** — owns the set of configured upstreams and
  their lifecycle. For each upstream it holds: config, connection state
  (`up`/`down`), the connected harness `mcp.Client` (when up), its adapted
  `tool.Tool` slice, last error, and retry bookkeeping. Exposes a read
  snapshot for status and a `ToolsFor(tenant)` view. One supervision
  goroutine per upstream: connect → on failure mark down, retry with capped
  exponential backoff → on (re)connect re-list tools and bump a generation
  counter.
- **Per-tenant MCP servers** — the SDK's `NewStreamableHTTPHandler` takes a
  `getServer(*http.Request) *mcp.Server` hook. The gateway keeps a lazily
  built cache `tenant → *mcp.Server` (plus one "all upstreams" server for
  open mode / superuser). A server is (re)built from `Manager.ToolsFor(tenant)`
  when first requested or when the manager's generation counter has moved
  (an upstream came up/down). Each gateway tool handler delegates to the
  underlying `tool.Tool.Execute` and maps the result to an MCP
  `CallToolResult` (`ToolResult.Error` ⇒ `isError: true` text content).
- **Tool naming** — harness's adapter names upstream tools
  `mcp__<server>__<tool>`. The gateway re-exposes them as
  `<server>__<tool>` (it strips the adapter's `mcp__` prefix), because the
  consuming harness client adds its own `mcp__gateway__` prefix; agents end
  up with `mcp__gateway__fs__read_file`, not a double-prefixed name.
  Upstream `name`s must be unique in config (validated at load).
- **Control-plane wiring** — `NewAPI` (or the router around it) mounts
  `/gateway/mcp` (the StreamableHTTP handler) and `GET /gateway/status`.
  Both sit behind the existing `IdentityMiddleware`. The middleware's
  `actionForRequest` already treats POST as `invoke` and GET as `read`;
  gateway routes carry no `{agent id}`, so the middleware authenticates but
  does not agent-authorize — tenancy is enforced *inside* the gateway by
  tool-visibility filtering (see §5).

## 4. Configuration

`runtime.yaml` grows a top-level `gateway:` section; `AgentConfig` grows a
`gateway` bool:

```yaml
gateway:
  servers:
    - name: fs                         # required, unique; namespaces tools
      command: npx                     # stdio transport…
      args: ["-y", "@modelcontextprotocol/server-filesystem", "/data"]
      env: {SOME_VAR: value}           # optional, merged into child env
    - name: search
      url: https://mcp.example.com/mcp # …or Streamable HTTP transport
      headers: {Authorization: "Bearer ${SEARCH_TOKEN}"}   # ${VAR} expanded from operator env at load
      tenants: [acme, globex]          # optional; absent/empty ⇒ visible to ALL tenants

agents:
  - id: support
    tenant: acme
    gateway: true                      # opt-in, like memory: true
```

Validation at config load (fail-fast, like agent validation today):
- `name` required and unique across `gateway.servers`.
- Exactly one of `command` / `url` per server (mirrors `mcp.ServerConfig`).
- `${VAR}` expansion from the operator environment in the values of
  `headers`, `env`, and `gateway.agent_keys`, so secrets stay out of the
  YAML file. Unset var ⇒ load error (fail-fast, not empty string). Literal
  values without `${…}` pass through unchanged.
- An empty/absent `gateway:` section ⇒ the gateway routes return 404 and
  nothing else changes (full backward compatibility).

Go shape (in `internal/config`):

```go
type GatewayConfig struct {
    Servers   []GatewayServer   `yaml:"servers"`
    AgentKeys map[string]string `yaml:"agent_keys"` // tenant → svk-… (values ${VAR}-expandable); see §6
    SelfURL   string            `yaml:"self_url"`   // optional base URL agents use to reach the gateway; see §6
}
type GatewayServer struct {
    Name    string            `yaml:"name"`
    Command string            `yaml:"command"`
    Args    []string          `yaml:"args"`
    Env     map[string]string `yaml:"env"`
    URL     string            `yaml:"url"`
    Headers map[string]string `yaml:"headers"`
    Tenants []string          `yaml:"tenants"` // nil/empty ⇒ all tenants
}
```

`GatewayServer` maps 1:1 onto harness `mcp.ServerConfig` (plus `Tenants`).

## 5. Auth & tenancy

- `/gateway/mcp` and `/gateway/status` are **not** exempt paths — the
  identity middleware authenticates every request. Machines use service keys
  (`svk-…`) as `Authorization: Bearer`; that path already exists in
  Identity M1's `Authenticator`.
- The authenticated `Principal` is read from the request context inside the
  gateway handler. Visibility rule, applied in `ToolsFor(tenant)`:
  - Upstream with `tenants:` unset/empty → visible to every tenant.
  - Upstream with `tenants: [a, b]` → visible only to principals of tenant
    `a` or `b` (and superusers, who see everything).
- A tool that is not visible to the caller's tenant is absent from
  `tools/list` and `tools/call` returns the standard MCP "tool not found"
  error — invisible, not forbidden, consistent with the platform's
  404-hides-existence stance.
- **Role gate:** `tools/call` (and the MCP handshake generally) arrives as
  POST ⇒ `ActionInvoke`; viewers can still *connect* (the middleware only
  agent-authorizes `/agents/{id}` paths), so the gateway itself requires
  role ≥ operator for `tools/call` and allows any authenticated principal
  to `tools/list`. Enforced in the tool handler via the Principal.
- **Open mode** (no identity configured, matching the spine): all upstreams
  visible, no key needed. Same backward-compatible posture as the rest of
  the platform.
- `GET /gateway/status` requires role ≥ operator (it leaks operational
  detail); superusers see all upstreams, tenant principals see only
  upstreams visible to their tenant.

## 6. Agent wiring (`gateway: true`)

Spawn path (all existing seams, no new mechanisms):

1. `AgentConfig.Gateway bool` flows `config → Registry → AgentProcess`
   (exactly like `Memory`).
2. `AgentProcess.buildEnv` appends, when set:
   - `RUNTIME_GATEWAY_URL=<base>/gateway/mcp`, where `<base>` is a new
     optional `gateway.self_url` config value (e.g. `http://127.0.0.1:8080`);
     when unset it is derived from the control-plane listen address with a
     wildcard/empty host rewritten to `127.0.0.1` (agents are local
     subprocesses, so loopback is always correct in M1)
   - `RUNTIME_GATEWAY_KEY=<key>` — resolved per tenant from operator config:
     a new optional `gateway.agent_keys: {tenant: svk-…}` map (values
     `${VAR}`-expandable). In open mode the key may be absent and the
     variable is omitted.
   - Fail-closed: identity configured + `gateway: true` + no key for the
     agent's tenant ⇒ spawn error (same posture as the secrets broker).
3. `cmd/agentd` reads the two vars; when `RUNTIME_GATEWAY_URL` is set, the
   agentkind layer appends to the spec before `BuildRuntime`:

   ```go
   spec.MCPServers = append(spec.MCPServers, mcp.ServerConfig{
       Name: "gateway",
       URL:  gatewayURL,
       Headers: map[string]string{"Authorization": "Bearer " + gatewayKey}, // when key set
   })
   ```

   Wired in `agentkind` as `wireGateway(cfg *agentruntime.Config, d Deps)`
   alongside `wireMemory`, with `Deps` growing `GatewayURL`/`GatewayKey`
   fields (read from env in `cmd/agentd/main.go`, mirroring `Memory`).
4. Foreign (shim) agents receive the same env vars and may use them or not —
   no contract change.

Harness's `BuildRuntime` then connects to the gateway like any MCP server;
the agent sees tools named `mcp__gateway__<server>__<tool>`.

## 7. Failure model

- **Startup:** `runtimed` starts regardless of upstream health. Each upstream
  connects asynchronously; a failure logs (structured slog, per-upstream
  fields) and marks it `down`.
- **Reconnect:** per-upstream loop with capped exponential backoff
  (e.g. 1s → 2s → … → 60s cap, with jitter). On success: re-list tools,
  swap the tool slice, bump generation (per-tenant servers rebuild lazily).
- **Mid-flight death:** a `tools/call` whose upstream session errors returns
  an MCP error result (`isError: true` with the error text) — never a
  transport-level failure to the caller. The manager marks the upstream
  down and the reconnect loop takes over.
- **Stdio children:** an upstream child process dying is just a failed call /
  failed session → same down/reconnect path. Children are killed on
  `runtimed` shutdown (bounded, alongside the agent supervisor shutdown).
- **No restart storms:** reconnect state is per-upstream and independent;
  one flaky upstream cannot affect others or the platform.

## 8. Observability (M1-level)

- Structured slog throughout: connect/disconnect/retry per upstream with
  `server`, `transport`, `err`, `attempt` fields (matches M3 logging style).
- `GET /gateway/status` returns JSON: per upstream `{name, transport, state,
  tool_count, last_error, connected_at}`. This is the operator's view until
  the console panel (later milestone) and full Observability (B5).

## 9. Testing & done criteria

Conventions: unit tests hermetic; integration tests `//go:build integration`
against local Postgres.app (`postgres://runtime:runtime@localhost:5432/runtime`),
self-cleaning; the `go` CLI is ground truth.

**Unit (in-memory MCP upstreams via the SDK's `InMemoryTransport`, as harness
already does):**
- Config: validation (unique names, command/url exclusivity, `${VAR}`
  expansion incl. unset-var failure), back-compat empty section.
- Naming: upstream tool re-exposed as `<server>__<tool>`; no double prefix.
- Tenancy: `tenants:`-restricted upstream invisible to other tenants
  (absent from list; call ⇒ tool-not-found), visible to allowed tenant and
  superuser; open-mode shows all.
- Role gate: viewer can list, cannot call; operator can call.
- Failure: down upstream ⇒ startup proceeds, tools absent, status shows
  `down` + last_error; call on a dying upstream ⇒ `isError` result;
  reconnect ⇒ tools reappear (generation bump rebuilds tenant servers).
- Agent wiring: `buildEnv` emits/omits the two vars correctly; fail-closed
  on missing tenant key when identity is on; `agentkind` appends the
  gateway `MCPServers` entry iff `RUNTIME_GATEWAY_URL` is set.

**Integration (through-serve):**
- Full `runtimed` + a fake Streamable HTTP upstream + a `gateway: true`
  test agent: one end-to-end turn in which the agent calls a gateway tool
  through the injected env, and the tool result round-trips. (The analog of
  `test/kg_runturn_e2e_test.go` for the gateway path.)

**Live proof (manual, recorded in the milestone notes):**
- A real agent calls a real public MCP server (e.g. the reference
  filesystem or fetch server, stdio) through the gateway end-to-end.

**Done =** all of the above green + README/ROADMAP updated.

## 10. Explicitly out of scope (later Gateway milestones)

- REST/OpenAPI → tool adapters ("turn any API into a tool").
- Semantic tool search / discovery (will reuse the Memory M2 embedding
  plumbing).
- Dynamic upstream registration (admin API, Postgres-backed) and per-tenant
  self-service upstreams.
- MCP resources/prompts passthrough — **tools only** in M1.
- Console UI panel for gateway status.
- Automatic minting of per-tenant agent service keys (Identity follow-on);
  M1 uses operator-configured keys.
- Rate limiting / quotas per tenant.

## 11. Risks & mitigations

- **SDK server feature drift** (tool annotations, content types not
  representable through `tool.Tool`): accepted for M1 — `tool.Tool` carries
  name/description/schema/result text + images, which covers the tools-only
  scope. Raw-passthrough (Approach 2) remains possible later behind the same
  endpoint if fidelity gaps appear.
- **Per-tenant server cache growth:** bounded by tenant count (small);
  rebuilt-on-generation keeps it correct without invalidation complexity.
- **Stdio child lifecycle inside runtimed:** children are owned by the
  manager and torn down on shutdown; a runaway child is no worse than any
  supervised agent subprocess.
