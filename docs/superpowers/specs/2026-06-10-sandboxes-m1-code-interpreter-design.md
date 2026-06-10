# Sandboxes M1 — Code Interpreter behind the Gateway

**Date:** 2026-06-10
**Status:** Approved design, pre-implementation
**Sub-project:** B4 Sandboxes, milestone 1 (of: M1 code interpreter, M2 candidates: browser sandbox / kernel-mode persistence / egress policy)
**Builds on:** Gateway M1 (federation core: stdio upstreams, supervise/reconnect, per-tenant views) and Gateway M2 (search mode — orthogonal, works unchanged); Identity M1 (tenant model, existence-hiding posture).

## 1. Context & purpose

B4 brings AgentCore-style sandboxed execution to the platform. M1 delivers the
**code interpreter**: an isolated, stateful, per-session Python + shell
environment any agent can use through the existing gateway opt-in — Go agents
and foreign-SDK shim agents alike, with **zero agent-side changes**.

The delivery mechanism is the milestone's architectural statement: the sandbox
is **just another federated MCP upstream**. A new `cmd/sandboxd` binary speaks
MCP over stdio; `runtime.yaml` declares it under `gateway.servers:`; agents see
`mcp__gateway__sandbox__<tool>`. This both ships B4 and stress-tests the
gateway with a real, stateful, multi-call upstream (everything federated so
far has been stateless per call).

## 2. Decisions (settled during brainstorming)

| Decision | Choice | Rationale |
|---|---|---|
| M1 scope | Code interpreter only | Higher-value AgentCore-parity piece; browser is a separate subsystem (own spec later) |
| Isolation | Docker container per sandbox session | Works on macOS dev AND Linux prod; gVisor becomes a config knob (`runsc` runtime) on Linux, not a separate build. Locked-down: no network, read-only rootfs, tmpfs workspace, caps dropped, non-root, CPU/mem/pids limits |
| Session model | Stateful, explicit tools (`create_sandbox` → id → calls → `close_sandbox`) | AgentCore semantics: files persist across calls; idle TTL + max lifetime bound runaway sessions |
| Execute backend | `docker exec` per call into a sleeping container (Approach A) | Files persist, interpreter variables do NOT (fresh process per call — documented). Robust: a hung exec is killed without nuking the session. Kernel-mode (variables persist) is the designated M2 upgrade — same tool surface, swapped backend |
| Packaging | New `cmd/sandboxd` Go binary, stdio upstream | Gateway M1 already supervises stdio upstreams with capped-backoff reconnect — zero new plumbing; also runnable standalone for tests |
| Tenancy | Tenant-scoped ownership + per-tenant caps | Sandbox created by tenant A is not-found to tenant B (existence hidden, Identity M1 posture); gateway forwards caller tenant (the milestone's only gateway change) |
| Runtime & network | Bundled Python image, `network=none` ALWAYS | `python:3.12-slim` + numpy/pandas/matplotlib/requests preinstalled. No egress in M1; pip-install-at-runtime deferred to an egress-policy milestone |

## 3. Architecture

```
agent (Go or shim) ── mcp__gateway__sandbox__execute_code ──▶ runtimed /gateway/mcp
                                                                    │ (identity middleware: principal)
                                                              gateway Handler.toolHandler
                                                                    │ inject __rt_tenant (forward_tenant: true)
                                                              harness tools/mcp client (stdio)
                                                                    ▼
                                                              cmd/sandboxd (MCP server)
                                                                    │ strip __rt_tenant, authorize, dispatch
                                                              internal/sandbox.Manager
                                                                    │ Docker API (engine socket / DOCKER_HOST)
                                                                    ▼
                                                    one container per sandbox session
                                                    (sleep infinity; tmpfs /workspace)
```

Config:

```yaml
gateway:
  servers:
    - name: sandbox
      command: bin/sandboxd          # stdio, supervised by Gateway M1 Manager
      forward_tenant: true           # NEW field — see §5
      tenants: [acme, globex]        # existing visibility filter, unchanged
```

No runtimed code paths change besides config parsing of the new field and the
gateway's forwarding behavior. `gateway: true` / `gateway: search` agents pick
the tools up automatically (search mode indexes the sandbox tool descriptions
like any others).

## 4. sandboxd

### 4.1 Package layout

```
cmd/sandboxd/main.go      # MCP server over stdio (same SDK the gateway serves with);
                          # env config; wires Manager; reap-on-start
internal/sandbox/
  manager.go              # Manager: sessions map, per-tenant caps, reaper, TTLs
  docker.go               # engine seam: the Docker API subset we use, as an
                          # interface (containerBackend) + real implementation
  tools.go                # the 6 MCP tool definitions + handlers (strip
                          # __rt_tenant, validate, dispatch to Manager)
  paths.go                # /workspace path confinement (clean, reject escape)
```

The Docker dependency is the official `github.com/docker/docker/client` Go SDK,
isolated behind the `containerBackend` interface so unit tests run hermetic.

### 4.2 Tool surface (7 tools)

All tools take/return JSON. `sandbox_id` is an unguessable random id
(`sbx-<128-bit hex>`). Agent-visible names: `mcp__gateway__sandbox__<name>`.

| Tool | Input | Output | Notes |
|---|---|---|---|
| `create_sandbox` | — | `{sandbox_id, expires_at}` | Starts the container (§4.3). Cap exceeded ⇒ isError naming the cap. `expires_at` = now + max lifetime |
| `execute_code` | `{sandbox_id, code, timeout_s?}` | `{stdout, stderr, exit_code, duration_ms}` | `docker exec` of `python3 -c <code>`, workdir `/workspace`. Default timeout 30s, max 120s. Timeout kills the exec process only; sandbox stays usable |
| `run_command` | `{sandbox_id, command, timeout_s?}` | same | `sh -c <command>` — same exec path, same limits |
| `write_file` | `{sandbox_id, path, content}` | `{path, bytes}` | Text content; path confined to `/workspace` (§4.5); parent dirs created |
| `read_file` | `{sandbox_id, path}` | `{content, bytes, truncated}` | Confined; capped at 256 KiB (truncated flag set beyond) |
| `list_sandboxes` | — | `{sandboxes: [{sandbox_id, created_at, expires_at, last_used_at}]}` | Caller-tenant's sandboxes only |
| `close_sandbox` | `{sandbox_id}` | `{closed: true}` | Removes the container; idempotent (already-gone ⇒ closed: true) |

Semantics note surfaced in every relevant tool description so the LLM knows:
**files in `/workspace` persist across calls; Python variables do not** (each
`execute_code` is a fresh interpreter process). This is the documented M1
trade-off; kernel-mode persistence is the designated M2 upgrade with the same
tool surface.

### 4.3 Container posture (per sandbox)

- Image: bundled `runtime-sandbox:latest` (§4.4); override `RUNTIME_SANDBOX_IMAGE`.
- Entrypoint: `sleep infinity` (the session anchor; execs ride alongside).
- `NetworkMode: none` — always; no M1 opt-out.
- Read-only rootfs; tmpfs mounted at `/workspace` (default 64 MiB,
  `RUNTIME_SANDBOX_WORKSPACE_MB`), tmpfs at `/tmp` (16 MiB).
- Non-root user (the image's `sandbox` user), `CapDrop: ALL`,
  `no-new-privileges`.
- Limits: CPU 1.0 (`RUNTIME_SANDBOX_CPUS`), memory 512 MiB
  (`RUNTIME_SANDBOX_MEM_MB`), pids 128.
- Labels: `runtime.sandbox=1`, `runtime.sandbox.tenant=<tenant>` — discovery
  for reap-on-start and debugging.
- Optional `RUNTIME_SANDBOX_RUNTIME=runsc` passes a Docker runtime name
  through (the gVisor knob on Linux hosts; unset ⇒ default runtime).

### 4.4 Bundled image

`deploy/sandbox.Dockerfile`: `python:3.12-slim`, a non-root `sandbox` user,
and preinstalled `numpy pandas matplotlib requests` (requests included so
network-isolation failures are *meaningful* — the lib exists, egress doesn't).
Makefile target `make sandbox-image` builds/tags `runtime-sandbox:latest`;
the main `make build` does NOT require it (sandboxd degrades per §6 if the
image is missing — create_sandbox isError names the missing image).

### 4.5 Path confinement

`write_file`/`read_file` paths resolve under `/workspace` only:
relative paths joined to `/workspace`, absolute paths must have `/workspace/`
prefix after `path.Clean`; anything escaping (`..`, absolute elsewhere,
symlink tricks are moot — I/O goes through `docker exec cat`/shell-free tar
copy, not host mounts) ⇒ isError "path outside /workspace". File I/O uses the
Docker copy API (tar), not shell interpolation — no quoting bugs.

## 5. The gateway change: `forward_tenant`

The only change outside the new package. MCP carries no principal, and the
harness MCP client forwards only `Arguments` — so the tenant rides a
**reserved argument key**, `__rt_tenant`:

- Config: `GatewayServer.ForwardTenant bool` (`forward_tenant:` in YAML).
- In `Handler.toolHandler`, when the upstream serving the tool has
  `ForwardTenant`, the gateway **first strips any caller-supplied
  `__rt_tenant`** from the arguments (spoof-proof: an agent can never set it),
  then injects the authenticated principal's tenant. Open mode / superuser ⇒
  injects `""`.
- sandboxd reads and removes `__rt_tenant` before schema validation of the
  remaining input; `""` maps to tenant `default` (mirrors Identity M1's
  absent-tenant rule).
- sandboxd trusts the value because it is a stdio child of runtimed reachable
  ONLY through the gateway — there is no other client. The spec for any future
  HTTP-transport sandbox deployment must revisit this (noted in §9).
- Upstreams without `forward_tenant` see byte-identical behavior to today.

Tenant scoping rules in sandboxd:

- Every sandbox records its creator tenant. Any call with a `sandbox_id` owned
  by another tenant returns the **same isError as a nonexistent id**
  ("no such sandbox") — existence hidden, matching the platform's cross-tenant
  404 posture.
- Per-tenant concurrent-sandbox cap: `RUNTIME_SANDBOX_MAX_PER_TENANT`
  (default 5). At cap, `create_sandbox` isError tells the agent to close one.

## 6. Lifecycle & failure posture

**Reaper** (goroutine in Manager): closes sandboxes idle past
`RUNTIME_SANDBOX_IDLE_TTL` (default 10m; every successful call touches
`last_used_at`) or older than `RUNTIME_SANDBOX_MAX_LIFETIME` (default 1h).
Closed = container removed, session forgotten. A call racing the reaper gets
"no such sandbox".

**Reap-on-start:** sandboxd startup lists containers labeled
`runtime.sandbox=1` and removes them — leftovers from a crashed previous run
never accumulate.

**Degrade-don't-fail** (mirrors gateway posture):

| Failure | Behavior |
|---|---|
| Docker daemon unreachable at startup | sandboxd still serves MCP; `create_sandbox` ⇒ isError "sandbox backend unavailable"; retried per call (daemon appearing later just works) |
| Image missing | `create_sandbox` ⇒ isError naming the image and the `make sandbox-image` remedy |
| Container dies mid-session | calls on that id ⇒ isError; session marked closed; agent creates a new one |
| Exec timeout | exec process killed; sandbox stays usable; result reports the timeout |
| sandboxd crashes | Gateway M1 supervise/reconnect restarts it; reap-on-start clears orphans; in-flight calls fail with MCP isError at the gateway (existing behavior) |

**Config validation:** `forward_tenant: true` on a `url:` (HTTP) upstream is a
config load error in M1 (the trust argument in §5 only holds for stdio
children). A `command:` upstream with `forward_tenant` is valid regardless of
which binary it runs (the mechanism is generic; sandboxd is just its first
consumer).

## 7. Testing & done criteria

**Hermetic unit tests (no Docker, no network):**

- `internal/sandbox`: fake `containerBackend` — create/execute/files/close
  round-trip; per-tenant cap; cross-tenant id ⇒ same error as missing id;
  `__rt_tenant` stripped from caller args and honored from gateway; ""
  ⇒ default tenant; path confinement table (.. / absolute / clean escapes);
  idle + max-lifetime reaper with a fake clock; reap-on-start removes labeled
  leftovers; timeout kills exec not session; read_file truncation flag.
- `internal/gateway`: `forward_tenant` injection (tenant set, open-mode ""),
  caller-supplied `__rt_tenant` stripped before inject; non-forwarding
  upstreams' arguments byte-identical; config error for `forward_tenant` + `url:`.
- `internal/config`: YAML parse of the new field; validation rule above.

**Through-serve e2e** (`test/gateway_sandbox_e2e_test.go`): full chain with the
fake backend — runtimed serving, identity on, agent-visible tool name
`mcp__gateway__sandbox__execute_code` called via the gateway endpoint; proves
tenant injection end-to-end (two principals, sandbox invisible across tenants).

**Live proof (manual, recorded in the ROADMAP entry):**

1. Real Docker: create → `write_file` a CSV → `execute_code` pandas analysis
   reading it → `read_file` the produced result. Files persisted across calls.
2. Network is dead: `requests.get(...)` inside the sandbox fails.
3. Two tenants: tenant B `list_sandboxes` doesn't show A's; B calling A's id
   gets "no such sandbox".
4. Idle reaper observed closing a sandbox (short TTL override).
5. End-to-end agent turn: a gateway-enabled agent under runtimed uses the
   sandbox tools to compute something and answers with the result.

## 8. Out of scope (M2+ candidates, recorded)

- Browser sandbox (B4's second half — own spec).
- Kernel-mode variable persistence (same tool surface, swapped execute backend).
- Network egress policy / pip-install-at-runtime.
- gVisor beyond the `RUNTIME_SANDBOX_RUNTIME=runsc` passthrough knob.
- Per-user (sub-tenant) scoping; resource billing/quotas beyond the
  concurrency cap.
- sandboxd over Streamable HTTP (multi-runtimed sharing) — requires a real
  auth story for `__rt_tenant` (see §5 trust note).
- Console panel for live sandboxes.

## 9. Risks & mitigations

- **Docker API surface drift** — the official Go SDK is versioned; pin it and
  keep the `containerBackend` seam minimal (create/start/exec/copy/remove/list).
- **`__rt_tenant` collision with a real argument** — the `__rt_` prefix is
  reserved; sandboxd's schemas never declare it; the gateway strips it
  unconditionally for forwarding upstreams. Risk of a non-sandbox forwarding
  upstream legitimately wanting an `__rt_tenant` arg: accepted, documented.
- **Trust scope of `__rt_tenant`** — valid ONLY because stdio children are
  unreachable except via the gateway; enforced by the §6 config rule
  (forward_tenant ⇒ stdio only). Revisit before any HTTP sandbox deployment.
- **macOS dev vs Linux prod differences** (tmpfs, runsc absent on macOS) —
  runsc knob is optional/unset by default; everything else is portable Docker
  API. Live proof runs on macOS Docker Desktop; Compose path covers Linux.
- **Image size / build time** — slim base + 4 libs ≈ 400–600 MB; built only
  by the explicit `make sandbox-image` target, never as a side effect.
