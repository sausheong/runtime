# Gateway and sandboxes

The gateway gives agents one tenant-aware MCP endpoint for operator-configured
stdio servers, HTTP MCP servers, and REST/OpenAPI services. The code and browser
sandboxes are MCP servers normally reached through that gateway.

## Gateway configuration

```yaml
gateway:
  self_url: http://127.0.0.1:8080
  agent_keys:
    acme: ${ACME_AGENT_SERVICE_KEY}
  servers:
    - name: filesystem
      command: ./bin/filesystem-mcp
      args: ["--root", "/srv/docs"]
      env:
        LOG_LEVEL: warn
      tenants: [acme]
      forward_tenant: true

    - name: remote
      url: https://mcp.example.com/mcp
      headers:
        Authorization: Bearer ${REMOTE_MCP_TOKEN}

    - name: orders
      openapi: ./orders.openapi.yaml
      base_url: https://orders.example.com
      operations:
        - "GET /orders/*"
```

Each server has exactly one transport:

- `command` launches a trusted local stdio MCP child;
- `url` connects to Streamable HTTP MCP;
- `openapi` converts selected OpenAPI 3.x operations into tools.

Tools are namespaced as `<server>__<tool>`. Server names cannot contain `__`.
An empty `tenants` list makes an operator-configured server visible to all
tenants. File configuration is operator-trusted and may intentionally target
private infrastructure.

Environment placeholders in headers, stdio environment, agent keys, and remote
agent tokens are expanded at load time. Prefer tenant secrets for dynamic
upstreams and credentials; avoid putting plaintext credentials in YAML.

## Authentication and tenancy

Clients call `/gateway/mcp` through the same identity boundary as the rest of
the control plane. Runtime filters the visible catalogue and verifies tenant
access again when a tool is called. A client cannot call a hidden tool merely
by guessing its name.

For a managed native agent:

```yaml
agents:
  - id: support
    name: Support
    model: vendor/model
    listen_addr: 127.0.0.1:8101
    registration_generation: 89ef9a06-e752-49e2-a8bc-a9b14983dc6f
    gateway: true
```

The control plane injects `RUNTIME_GATEWAY_URL` and the owning tenant's service
key. `gateway: true` or `gateway: full` exposes the normal federated tool list.
`gateway: search` exposes catalogue search first, reducing large tool lists in
the model context.

Search tuning:

| Variable | Default | Meaning |
|---|---:|---|
| `RUNTIME_GATEWAY_SEARCH_K` | `5` | Maximum catalogue matches |
| `RUNTIME_GATEWAY_SEARCH_FLOOR` | `0.2` | Minimum relevance score |

Search mode depends on the configured embedding service. It changes discovery,
not authorisation: the same tenant, policy, quota, and credential checks run on
the eventual call.

## REST and OpenAPI tools

For an OpenAPI server, Runtime derives JSON schemas from operations. Use
`operations` to allowlist operation IDs or `METHOD /path/*` patterns.
`base_url` overrides the specification's first usable server URL.

Tool input represents path, query, header, and body parameters. Runtime builds
the HTTP request, applies operator or tenant credentials, and returns a bounded
response envelope rather than giving the model a general-purpose HTTP client.
Tenant-registered OpenAPI and HTTP MCP targets must resolve to public
addresses; redirects and address resolution are checked to reduce SSRF risk.

Operator-configured targets are a separate trust class and may use private
addresses. Treat edits to `runtime.yaml` as privileged infrastructure changes.

## Dynamic upstreams and agents

Tenant administrators can persist upstreams without restarting the control
plane:

```bash
runtimectl admin upstream add \
  --name orders \
  --openapi https://api.example.com/openapi.json \
  --base-url https://api.example.com \
  --cred-secret orders-oauth \
  --cred-header Authorization

runtimectl admin upstream ls
runtimectl admin upstream rm <upstream-id>
```

Dynamic remote agents use the same public-target restrictions:

```bash
runtimectl admin agent add \
  --id analyst \
  --name Analyst \
  --model vendor/model \
  --url https://agent.example.com \
  --cred-secret agent-bearer

runtimectl admin agent disable analyst
runtimectl admin agent enable analyst
runtimectl admin agent restart analyst
runtimectl admin agent rm analyst
```

Registration tokens provide a one-time handshake for independently started
agents. Mint them with `runtimectl register mint --agent <id>`, set
`RUNTIME_REGISTRATION_URL` and `RUNTIME_REGISTRATION_TOKEN` on the agent, and
revoke unused tokens.

## Policies, quotas, and credentials

Cedar policy enforcement is enabled by `RUNTIME_POLICY_ENABLED=1` or by setting
`RUNTIME_POLICY_FILE`. The file is the platform policy layer. Tenant policy
sets are managed separately:

```bash
runtimectl admin policy add --name tool-policy --file policy.cedar
runtimectl admin policy ls
runtimectl admin policy rm tool-policy
```

Policy evaluation fails closed on errors. Keep policies in version control,
test both allow and deny cases, and monitor
`runtime_gateway_policy_decisions_total`.

Rate quotas can be file-configured or managed through the CLI:

```bash
runtimectl admin quota add --tenant acme --upstream orders --rate 60
runtimectl admin quota ls
runtimectl admin quota rm --tenant acme --upstream orders
```

`RUNTIME_GATEWAY_QUOTA_DEFAULT` supplies a platform default. Quota keys may use
`*` in file configuration. Quota state is process-local, so multiple control
planes do not yet share one exact rate window.

Outbound credentials can be static tenant secrets, OAuth2
client-credentials, or OAuth token-exchange/OBO definitions. The gateway
resolves or mints them at call time and fails closed on error. See [Identity,
secrets, and memory](identity-and-memory.md).

## Gateway failure model

- An unavailable upstream does not stop unrelated agents or servers.
- Tool list and call failures are returned as MCP errors and reflected in
  gateway metrics.
- Stdio children are supervised by the gateway manager and closed during clean
  control-plane shutdown.
- Tenant, policy, quota, and credential checks occur before dispatch.
- The gateway is a broker and enforcement point, not a transactional boundary.
  An upstream side effect may have happened even if its response is lost.

## Code sandbox

`sandboxd` exposes eight tools, namespaced by the gateway as `sandbox__*`:

`create_sandbox`, `execute_code`, `run_command`, `write_file`, `read_file`,
`list_sandboxes`, `close_sandbox`, and `close_session`.

Each sandbox is a container with a temporary `/workspace`. Python and shell
executions are separate processes; files persist within the sandbox, but Python
variables do not. Code execution timeouts default to 30 seconds and are capped
at 120 seconds. File reads are capped at 256 KiB. Paths are normalised and must
stay inside `/workspace`.

Default configuration:

| Variable | Default | Meaning |
|---|---:|---|
| `RUNTIME_SANDBOX_IMAGE` | Deployment image | Sandbox container image |
| `RUNTIME_SANDBOX_WORKSPACE_MB` | `64` | Workspace tmpfs size |
| `RUNTIME_SANDBOX_MEM_MB` | `512` | Memory limit |
| `RUNTIME_SANDBOX_CPUS` | `1.0` | CPU limit |
| `RUNTIME_SANDBOX_MAX_PER_TENANT` | `5` | Concurrent tenant limit |
| `RUNTIME_SANDBOX_IDLE_TTL` | `10m` | Idle expiry |
| `RUNTIME_SANDBOX_MAX_LIFETIME` | `1h` | Absolute expiry |
| `RUNTIME_SANDBOX_SCOPE` | Tenant | Set to `session` for session ownership |
| `RUNTIME_SANDBOX_RUNTIME` | Docker default | Optional OCI runtime such as gVisor |

The image is expected to have no network access. Resource limits, tmpfs
workspace, non-root execution, dropped capabilities, and an optional hardened
OCI runtime reduce risk. The Docker daemon remains a privileged host boundary:
the service controlling its socket is trusted, and this is not equivalent to a
hostile multi-tenant VM boundary.

Configure the gateway server with `forward_tenant: true`. `sandboxd` fails
closed when the reserved tenant context is absent. Direct single-tenant use is
an explicit escape hatch via `RUNTIME_SANDBOX_ALLOW_DIRECT=1`.

Containers are labelled and reaped on startup after an unclean exit. Tool
errors are returned as tool results so one bad execution does not terminate the
MCP transport.

## Browser sandbox

`browserd` exposes:

`create_browser`, `navigate`, `click`, `type`, `get_text`, `extract`,
`screenshot`, `evaluate`, `list_browsers`, `close_browser`, and
`close_session`.

Each browser has an isolated persistent profile for its lifetime. Browser
ownership and lifecycle follow the same tenant/session model as code
sandboxes.

| Variable | Default | Meaning |
|---|---:|---|
| `RUNTIME_BROWSER_IMAGE` | Deployment image | Chromium container |
| `RUNTIME_BROWSER_MEM_MB` | `1024` | Memory limit |
| `RUNTIME_BROWSER_CPUS` | `1.0` | CPU limit |
| `RUNTIME_BROWSER_PROFILE_MB` | `256` | Profile tmpfs size |
| `RUNTIME_BROWSER_MAX_PER_TENANT` | `5` | Concurrent tenant limit |
| `RUNTIME_BROWSER_IDLE_TTL` | `10m` | Idle expiry |
| `RUNTIME_BROWSER_MAX_LIFETIME` | `1h` | Absolute expiry |
| `RUNTIME_BROWSER_SCOPE` | Tenant | Set to `session` for session ownership |
| `RUNTIME_BROWSER_RUNTIME` | Docker default | Optional hardened OCI runtime |
| `RUNTIME_BROWSER_NETWORK` | Deployment network | Container network |
| `RUNTIME_BROWSER_EGRESS_MODE` | `deny-all` | Network policy |
| `RUNTIME_BROWSER_EGRESS_ALLOW` | Empty | Comma-separated host allowlist |
| `RUNTIME_BROWSER_PROXY_ADDR` | `0.0.0.0:0` | Egress-proxy bind address |
| `RUNTIME_BROWSER_PROXY_HOST` | Empty | Proxy host as seen from a private Docker network |
| `RUNTIME_BROWSER_CDP_DIAL_HOST` | `127.0.0.1` | Host used by `browserd` to dial a published CDP port |
| `RUNTIME_BROWSER_CDP_PUBLISH_HOST` | `127.0.0.1` | Loopback-only CDP publish override for direct-host installs |

Egress modes are `deny-all`, `allow-list`, and `allow-all-public`. The proxy
rejects loopback, link-local, and private destinations in public mode and
re-checks resolution to reduce DNS rebinding and redirect bypasses. An
allowlist should contain only the hosts the workflow genuinely needs.

The browser container is still processing hostile web content. Keep it
separate from control-plane credentials and internal networks, limit resources,
use a hardened runtime where available, and avoid mounting host data. If the
egress proxy fails, browsing fails closed.

For containerised `browserd`, prefer a private Docker network and set
`RUNTIME_BROWSER_PROXY_HOST` to the service name. In direct-host mode CDP is
published only on loopback; non-loopback `RUNTIME_BROWSER_CDP_PUBLISH_HOST`
values are ignored. `RUNTIME_BROWSER_CDP_DIAL_HOST=host.docker.internal` is
useful when `browserd` runs in a container but Chrome publishes its CDP port on
the host.

As with `sandboxd`, use `forward_tenant: true`; direct mode requires the
deliberate `RUNTIME_BROWSER_ALLOW_DIRECT=1` setting. Fake backends
(`RUNTIME_SANDBOX_FAKE=1`, `RUNTIME_BROWSER_FAKE=1`) are for tests only.
