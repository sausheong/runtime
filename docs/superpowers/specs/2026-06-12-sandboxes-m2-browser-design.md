# Sandboxes M2 — Browser Sandbox

**Date:** 2026-06-12
**Status:** Approved design, pre-implementation
**Sub-project:** B4 Sandboxes, milestone 2 (after M1 code interpreter)
**Builds on:** Sandboxes M1 (Manager contract, locked-down container posture, `forward_tenant`/`__rt_tenant` tenancy, `cmd/*d` stdio-MCP skeleton), the harness browser tool (`harness/tools/browser` chromedp logic, stealth script, `web.ValidateURLNotInternal`), the gateway (federation, search, `runtime_gateway_*` metrics, image-content passthrough), Observability M1 (`internal/obs` for a new egress metric).

## 1. Context & purpose

The M1 code interpreter runs the *absence* of network as its security boundary
(`network=none`, read-only rootfs). A browser inverts that: network is
mandatory and Chrome is a large attack surface. M2 ships a managed
headless-browser sandbox that runtime agents drive (navigate, click, type,
extract, screenshot) under an **enforced egress policy** and **per-tenant
scoping**, federated through the gateway exactly like M1 — agents opt in with
the existing `gateway: true`/`search` and see `mcp__gateway__browser__<tool>`
with zero agent-side changes.

The ROADMAP names two headline M2 features: the **browser sandbox** and
**network egress policy**. Because a browser must have network, egress policy
stops being optional and becomes the milestone's center of gravity — it is the
feature, not a side concern.

## 2. Decisions (settled during brainstorming)

| Decision | Choice | Rationale |
|---|---|---|
| Isolation boundary | Chrome in a locked-down Docker container, driven by `chromedp` over remote CDP (`NewRemoteAllocator`) | A container is the only enforcement point that sees *all* of Chrome's traffic and contains a renderer compromise. Reuses M1's `docker.go` lifecycle, reaping, tenancy. Driving with `chromedp` (not hand-rolled CDP) reuses the harness's navigation/wait/screenshot/extract logic, which is allocator-agnostic |
| Egress enforcement | Forced HTTP/HTTPS proxy (Chrome's network stack routed via `--proxy-server` to a `browserd`-run proxy that allows/denies by hostname; the agent drives Chrome only over CDP, so the proxy adjudicates all reachable traffic) | A proxy sees subresources, `fetch`, redirects, WebSocket upgrades — not just the top-level URL. Filters by hostname (what operators write rules about), not IP. DNS/iptables filtering races TTLs, breaks on shared-IP CDNs, and is bypassed by Chrome DoH / literal-IP connections. A network-level egress boundary is follow-on (§10) |
| Egress policy scope | Three modes in M2: `deny-all` (default), `allow-list` (hostname globs), `allow-all-public` (block internal). Richer DSL (per-tenant, ports, methods) deferred | Modes cover the real use cases with a crisp, testable security contract; the mode enum grows later without redesign. Matches how M1 deferred per-tenant credentials and Gateway M3 deferred OAuth2 |
| Tool surface | Explicit session lifecycle (M1-style) + a high-level `extract` action | `create_browser → verbs → close_browser` mirrors `create_sandbox`; the explicit `browser_id` maps cleanly onto M1's capped/reaped/tenant-scoped Manager (an implicit session label does not). `extract` (clean text/markdown) optimizes the dominant "read the page" path |
| Daemon | New `cmd/browserd` (package `internal/browser`), a sibling to `cmd/sandboxd` — not an extension | M1's no-network/read-only posture is the inverse of a browser's; one binary serving both would need a backend abstraction with two contradictory postures. A clean sibling keeps each daemon's security contract legible |
| Code from harness | Port (not import) the ~6 chromedp action functions; import the stealth script and SSRF guard directly | `harness/tools/browser` is built around its own `tool.Tool` types and subprocess launcher/session map, which would fight the Manager. The action functions are thin `chromedp.Run` sequences — porting keeps `internal/browser` self-contained and Manager-owned |
| Live proof | Real federated browse + egress enforcement demonstrated + screenshot through gateway + end-to-end agent turn | The egress block must be shown working against real Chrome — that is the milestone's marquee claim |

## 3. Architecture

```
agent ─▶ gateway (forward_tenant) ─▶ browserd (MCP stdio)
                                        │
                          ┌─────────────┴───────────────┐
                          │ browser.Manager             │  M1 Manager contract:
                          │  per-tenant cap, slot resv,  │  cap, reaper, maskIfGone,
                          │  idle/max-life reaper,        │  lookup-by-(tenant,id)
                          │  existence-hiding lookup      │
                          └─────────────┬───────────────┘
                                        │ dockerBackend.Create → chrome container
                                        │ chromedp.NewRemoteAllocator(container CDP)
                          ┌─────────────▼───────────────┐
                          │ chrome container             │  network: NO direct route
                          │  --headless=new              │  HTTP(S)_PROXY + --proxy-server
                          │  --remote-debugging-port      │  → egress proxy
                          │  read-only rootfs, capdrop,   │
                          │  non-root, cpu/mem/pids caps  │
                          └─────────────┬───────────────┘
                                        │ all traffic
                          ┌─────────────▼───────────────┐
                          │ egress proxy (in browserd)   │  hostname allow/deny per mode
                          │  deny-all|allow-list|         │  + unconditional internal block
                          │  allow-all-public             │  + DNS-rebind defense
                          └──────────────────────────────┘
```

**Reuse ledger** (this milestone is mostly assembly):

- *From M1 (`internal/sandbox`):* the Manager contract (per-tenant cap, slot
  reservation under lock, idle + max-lifetime reaper, reap-on-start by container
  label, `maskIfGone` existence-hiding, `lookup`-by-(tenant,id)); the
  `forward_tenant`/`__rt_tenant` tenancy mechanism; the `cmd/*d` env-config +
  stdio-MCP skeleton; the locked-down container posture (read-only rootfs,
  CapDrop ALL, no-new-privileges, non-root, cpu/mem/pids, optional gVisor).
- *From the harness (`tools/browser`, `tools/web`):* the chromedp
  navigation/wait/screenshot/extract action logic (allocator-agnostic); the
  anti-bot stealth script; `web.ValidateURLNotInternal` as the proxy's
  internal-address guard.
- *From the gateway:* federation, search indexing, `runtime_gateway_*` metrics,
  image-content passthrough (screenshots already survive federation —
  `internal/gateway/server.go` appends `ImageContent`).

**Genuinely new code:** (1) the Chrome container image + `chromedp.NewRemoteAllocator`
wiring across the container boundary (the one real engineering risk — Chrome
rejects CDP connections whose `Host` header is not `localhost`/an IP, so connect
via the container IP and handle debug-port readiness); (2) the egress proxy;
(3) the browser-verb tool definitions; (4) the `extract` HTML→markdown logic.

## 4. Components & file structure

**`internal/browser/` (new package)**

| File | Responsibility |
|---|---|
| `manager.go` | `Manager` over a `Backend`: per-tenant cap with slot reservation under lock, idle/max-lifetime reaper, reap-on-start, `maskIfGone` existence-hiding, `lookup`-by-(tenant,id). `Session` adds `CDPEndpoint`, the per-session chromedp allocator/ctx + cancels, and a `sync.Mutex` (one tab, serialized actions). Direct descendant of M1 `manager.go` |
| `backend.go` | `Backend` interface (`Create`→container+CDP endpoint, `Connect`→remote allocator ctx, `Remove`, `ListLeftovers`, `Ping`) + `fakeBackend` (in-memory, no Chrome) for hermetic tests and `RUNTIME_BROWSER_FAKE` |
| `docker.go` | `dockerBackend`: locked-down Chrome container (`HTTP(S)_PROXY` + `--proxy-server` pointed at the egress proxy via `host.docker.internal`, read-only rootfs + tmpfs profile dir, CapDrop ALL, non-root, cpu/mem/pids, optional `runsc`); waits for the CDP port; returns endpoint. Label `runtime.browser=1` |
| `actions.go` | The chromedp action logic — navigate (networkIdle dance + low-level fallback), click/type/get_text/evaluate/screenshot/extract, per-selector wait budgets, stealth script. Ported from harness `browser.go`, adapted to drive a Manager-held remote-allocator ctx |
| `egress.go` | The egress proxy: `Policy` (mode + hostname globs), `http.Server` handling forward + `CONNECT`, hostname allow/deny decision, internal-address block via `web.ValidateURLNotInternal` + resolved-IP recheck. Listens on the bridge address the containers reach |
| `extract.go` | HTML→clean-text/markdown (strip script/style/nav, collapse whitespace). Small, self-contained, separately testable |
| `tools.go` | `NewServer(m, policy, allowDirect)`: the MCP tool definitions, `popTenant` reuse, error shaping |
| `tenant.go`, `paths.go` | `popTenant`/`__rt_tenant` (from M1) and selector/URL validation helpers |

**`cmd/browserd/main.go`** — env-config + stdio-MCP skeleton (sibling to
`sandboxd`): build backend, Manager, **start the egress proxy**, reap-on-start,
reaper goroutine, serve MCP over stdio.

**`deploy/browser.Dockerfile`** — headless Chromium + non-root user (uid matches
the const), launched with `--headless=new --remote-debugging-port
--remote-debugging-address` and no-sandbox flags. `make browser-image`.

**Tests:** `*_test.go` per file (hermetic, fake backend);
`internal/browser/docker_live_test.go` (`//go:build live`, real Chrome
container); `test/gateway_browser_e2e_test.go` (`integration` tag, through-serve
with identity + two tenants).

## 5. Egress proxy

The contract: **Chrome's entire network stack is routed through this proxy via
`--proxy-server`, which allows or denies by hostname. The agent can only drive
Chrome over CDP, so the proxy adjudicates all of the agent's reachable traffic.**

**Network wiring.** The container runs on a Docker bridge network (it must be
able to reach the proxy). `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` env vars plus a
Chrome `--proxy-server=` flag point at the proxy's host address, reached from the
container via `host.docker.internal` (mapped to the host gateway with an
`ExtraHosts: host.docker.internal:host-gateway` entry). The flag and env are
baked at container create into config the agent never touches, so the agent
cannot tell Chrome to bypass the proxy. Enforcement is therefore via Chrome's
proxy configuration, which is complete for the agent's CDP-only capability; a true
network-level egress boundary (so even a non-proxy-respecting in-container process
is contained) is out of scope for M2 — see §10.

**What the proxy sees and decides.** Two request shapes:

- **Plain HTTP** — full request line; decide on `Host`, then forward.
- **HTTPS `CONNECT host:443`** — destination host is in the CONNECT target (and
  SNI). Decide on that host; if allowed, open a blind TCP tunnel (encrypted body
  stays opaque — correct, not a gap); if denied, `403`.

This sees subresources, `fetch()`, redirects, and WebSocket upgrades — every
connection Chrome opens, not just the top-level `navigate` URL.

**The three modes** (`Policy.Decide(host) → allow|deny`):

- **`deny-all`** (default) — deny everything. A browser that reaches nothing is
  the safe default; mirrors M1's posture.
- **`allow-list: [globs]`** — allow iff host matches a configured glob
  (`*.wikipedia.org`, `api.github.com`); deny otherwise. Glob match is on
  hostname labels (`*` spans one or more labels, case-insensitive), never
  substring.
- **`allow-all-public`** — allow everything *except* internal/private addresses
  (RFC1918, loopback, link-local, ULA, `.internal`/`.local`), via
  `web.ValidateURLNotInternal` generalized to a host check.

**Security invariants:**

1. **Internal-address block is unconditional** — even `allow-list` and
   `allow-all-public` reject private/loopback/link-local hosts *and*
   allowlisted names that resolve to them (DNS-rebinding defense: resolve, then
   check the resolved IP before connecting/tunneling). No mode can turn this off.
2. **Deny is the default decision** — unknown mode, parse failure, malformed
   host → deny. Fail closed.
3. **The agent never sees the policy and can't reach the proxy's control path** —
   the proxy only proxies; it has no config endpoint.
4. **Every decision is logged** (`slog`: tenant, host, decision, mode) and
   **metered** — a new `runtime_browser_egress_total{decision}` counter via
   `internal/obs`, so denied-egress floods are observable.

**Honest limitations (recorded for later milestones):**

- HTTPS bodies are opaque — host-level filtering only, no path/method rules
  (those need MITM, out of scope).
- One policy per `browserd` instance in M2 (not per-tenant). Per-tenant policies
  are the designated follow-on.
- A renderer-exploit chain that breaks container networking is out of scope —
  defense in depth (capdrop, read-only, non-root, optional gVisor) is the
  mitigation; the proxy is the egress control, not an anti-exploit boundary.

## 6. Tool surface & data flow

Ten tools, M1-consistent. Tenancy rides `__rt_tenant` (gateway-injected,
`popTenant`-stripped) on every call — never in any schema. `browser_id` is the
session handle, exactly as `sandbox_id` is in M1.

**Lifecycle:**

- `create_browser` → `{browser_id, expires_at}`. Reserves a per-tenant slot
  under lock, creates the Chrome container, connects the remote allocator. No
  args.
- `list_browsers` → `{browsers: [{browser_id, created_at, last_used_at,
  expires_at, current_url}]}`.
- `close_browser {browser_id}` → idempotent, existence-hiding (unknown/foreign
  id → success).

**Actions** (all take `browser_id`; all serialize on the session mutex — one
tab, no interleaved `chromedp.Run`):

- `navigate {browser_id, url, wait_for?, wait_ms?}` → `{url, title}`. URL must be
  `http(s)://`; the **proxy** enforces egress — a denied host returns a
  navigation failure (the honest result), the tool does not pre-filter beyond
  scheme.
- `click {browser_id, selector, wait_for?}`, `type {browser_id, selector, text}`
  → action confirmation.
- `get_text {browser_id, selector?}` → raw innerHTML (selector defaults to
  body), truncated at the read cap.
- `extract {browser_id, selector?}` → clean text/markdown (script/style/nav
  stripped, whitespace collapsed) — the dominant "read the page" path.
- `screenshot {browser_id}` → image content through the gateway + a short text
  note.
- `evaluate {browser_id, script}` → JSON-marshaled JS result.

**Descriptions carry the rules of the road** (like M1's `persistNote`): a
`sessionNote` (the `browser_id` persists page/cookie/scroll state across calls)
and a `selectorNote` (CSS-only — no Playwright `:has-text()`/`text=`/`>>`;
reused from the harness). navigate/click advertise `wait_for`/`wait_ms` for SPAs.

**Typical agent turn** (`create → navigate → extract → close`):

```
create_browser → Manager.Create (cap + slot reservation) → chrome container,
                 wait CDP, NewRemoteAllocator → {browser_id, expires_at}
navigate       → lookup(tenant,id); session.mu.Lock → actions.navigate:
                 Chrome fetches via proxy; proxy.Decide(host) per mode;
                 subresources adjudicated individually → {url, title}
extract        → clean markdown of the rendered page (truncated)
close_browser  → Manager.Close → backend.Remove (or idle reaper at 10m)
```

**Error semantics** (M1's `maskIfGone` discipline): a vanished session mid-call
→ `errNoSandbox` (uniform not-found, existence hidden); a backend/Chrome
internal error → logged with the id, genericized to the model; a user-actionable
failure (bad selector, egress-blocked navigation, JS exception) → passed through
verbatim so the agent can adapt. Same split M1 draws between `ErrNoSuchFile` and
engine internals.

## 7. Lifecycle, failure posture & resource bounds

**Session lifecycle** — identical contract to M1:

- **Per-tenant cap** (default 5, `RUNTIME_BROWSER_MAX_PER_TENANT`): slot reserved
  under the Manager lock *before* the slow container create, so concurrent
  `create_browser` calls at cap−1 cannot all slip through. Lost-reservation path
  (tenant closed / reaper fired mid-create) removes the orphan container.
- **Idle TTL** (default 10m) + **max lifetime** (default 1h): reaper goroutine
  closes sessions past either bound. Browsers are heavy (~hundreds of MB), so
  these caps matter more than for Python containers.
- **Reap-on-start**: removes all `runtime.browser=1` containers on boot. Same
  single-daemon-per-host caveat as M1, documented in `main.go`.

**Failure posture (degrade-don't-fail):**

| Failure | Behavior |
|---|---|
| Docker daemon down at `create_browser` | Create fails; raw backend error surfaces (operator-relevant, no per-session state to hide) |
| CDP port never opens | `create_browser` fails after a bounded wait; orphan container removed |
| Chrome crashes mid-session | Next action's `chromedp.Run` errors → genericized; session marked dead, agent told to `create_browser` again. Backend `Ping` lets the reaper drop dead sessions proactively |
| Egress denies a host | Navigation/subresource fails as a normal page error — not a tool crash. Logged + metered as a deny |
| Action context canceled (turn ends) | `chromedp.Run` aborts via ctx; session survives for reuse |
| Session vanished mid-call (reaper raced) | `errNoSandbox` — uniform not-found, existence hidden |
| Screenshot/extract exceeds caps | Truncated (text) / JPEG quality-bounded (image); never an error |

**Resource bounds** — env-tunable with defaults: `RUNTIME_BROWSER_MAX_PER_TENANT=5`,
`RUNTIME_BROWSER_IDLE_TTL=10m`, `RUNTIME_BROWSER_MAX_LIFETIME=1h`,
`RUNTIME_BROWSER_MEM_MB` (higher default than M1 — Chrome needs ~512MB–1GB),
`RUNTIME_BROWSER_CPUS`, `RUNTIME_BROWSER_PROFILE_MB` (tmpfs profile dir),
`RUNTIME_BROWSER_RUNTIME` (`runsc` opt-in), `RUNTIME_BROWSER_IMAGE`, the three
egress vars (`RUNTIME_BROWSER_EGRESS_MODE`, `RUNTIME_BROWSER_EGRESS_ALLOW`,
`RUNTIME_BROWSER_PROXY_ADDR`), and `RUNTIME_BROWSER_ALLOW_DIRECT` /
`RUNTIME_BROWSER_FAKE` from M1.

**Security posture, consolidated** (the invariants a reviewer checks): Chrome's
network stack is routed through the proxy via `--proxy-server` and the agent can
only drive Chrome over CDP, so the proxy is the enforcement point for all of the
agent's reachable traffic (the container is on a bridge network — a network-level
egress boundary is follow-on, §10); internal-address block is unconditional
across all modes with DNS-rebind defense (resolve-then-check);
fail-closed default-deny; read-only rootfs + CapDrop ALL + no-new-privileges +
non-root + pids/mem/cpu caps + optional gVisor; tenancy is spoof-proof via
gateway `forward_tenant`; cross-tenant existence hidden; the agent never sees or
reaches the policy.

## 8. Testing & done criteria

**Hermetic unit tests** (fake backend, no Chrome, no Docker — default
`go test ./...` stays fast and CI-safe):

- `manager_test.go` — cap + slot reservation under concurrent create; idle +
  max-lifetime reaping; `lookup` foreign-tenant → `errNoSandbox`; `maskIfGone`
  genericizes engine errors but passes user-actionable ones; lost-reservation
  cleanup; existence-hiding on close/list.
- `egress_test.go` — the heart of the milestone. Table-driven `Policy.Decide`:
  `deny-all` denies all; `allow-list` glob matching (label-wise,
  case-insensitive, `*.x.org` matches `a.x.org` not `xx.org`, no substring
  leak); `allow-all-public` allows public, denies RFC1918/loopback/link-local/
  ULA/`.internal`; **unconditional internal-block across all modes**;
  **DNS-rebind defense** (allowlisted name resolving to a private IP → denied,
  fake resolver injected); fail-closed on unknown mode / malformed host; CONNECT
  vs plain-HTTP host extraction; end-to-end proxy test with two `httptest`
  servers (one allowed, one denied) proving forward + CONNECT decisions and the
  `403`.
- `extract_test.go` — script/style/nav stripping, whitespace collapse,
  truncation, malformed HTML does not panic.
- `tools_test.go` — `popTenant` strip; missing-required-arg errors; absent
  `__rt_tenant` fails closed unless `allowDirect`; schemas never mention
  tenancy; `screenshot` image-content shape.

**Live-gated tests** (`//go:build live`, real Chrome container):

- `docker_live_test.go` — `create_browser` → real container + CDP connect;
  `navigate`+`extract` against a local `httptest` server added to the
  allow-list; **egress actually blocks** a non-allowlisted host (the security
  proof); `screenshot` returns a real JPEG; reaper removes a real container;
  timeout/crash handling.

**Through-serve e2e** (`test/gateway_browser_e2e_test.go`, `integration` tag):
runtimed + identity on + `browserd` as a `forward_tenant` upstream + fake
backend; an external MCP client calls `mcp__gateway__browser__*`; **tenant
filtering** hides one tenant's browsers from another; spoofed `__rt_tenant`
overridden; `runtime_gateway_tool_calls_total` and `runtime_browser_egress_total`
series appear.

**Live proof (recorded in the ROADMAP entry), four parts:**

1. **Real federated browse** — `browserd` behind the gateway, `allow-list` mode;
   an external MCP client drives `create_browser → navigate(a real public page
   on the allow-list) → extract`, getting clean content back.
2. **Egress enforcement, demonstrated** — same session, `navigate` to a host
   *not* on the allow-list → blocked; flip to `allow-all-public` → the public
   host loads but an internal/RFC1918 address is still refused. The headline
   feature, shown working against real Chrome.
3. **Screenshot through the gateway** — `screenshot` returns an image that
   survives federation to the agent.
4. **End-to-end agent turn** — a `gateway: true` agent (real LLM via the proxy)
   answers a question requiring a browse of an allowed page, and `gateway:
   search` discovers a browser tool by natural-language query.

**Done =** all suites green + live proof recorded + ROADMAP/README updated +
spec/plan committed + merged to master, following the established flow
(subagent-driven development with two-stage review per task → final holistic
review → live proof → `merge --no-ff`).

## 9. Out of scope (recorded for later milestones)

- Network-level egress boundary (internal network / iptables) so even a non-proxy-respecting in-container process is contained — M2 enforces egress via Chrome's proxy configuration.
- Per-tenant egress policies (M2 is one policy per `browserd` instance).
- Path/method egress rules (needs HTTPS MITM).
- Richer egress DSL (ports, protocols, per-tenant rule sets).
- File upload into / download out of the browser.
- PDF generation from pages.
- Multi-tab / multi-window per session (one tab per session in M2).
- Persistent cookie jars across sessions (state lives only within a session).
- A console/admin panel for browser sessions (shared B4 backlog item).

## 10. Risks & mitigations

- **Remote CDP across the container boundary** (the main engineering risk) —
  Chrome rejects CDP connections whose `Host` header is not `localhost`/an IP,
  and the debug port has a readiness race. Mitigation: connect via the container
  IP, poll the debug endpoint for readiness with a bounded wait. Well-trodden
  (browserless, rod). Tactical fallback if the remote driver proves too fiddly:
  keep the container, hand-roll thin nav logic (Option 1 from brainstorming) —
  same architecture, less reuse, not a redesign.
- **Egress policy bypass** — Chrome's network stack is routed through the proxy
  via `--proxy-server` and the agent can only drive Chrome over CDP, so the proxy
  adjudicates all of the agent's reachable traffic; internal-block is
  unconditional with resolve-then-check DNS-rebind defense; fail-closed default.
  All unit-tested. The container is on a bridge network, so a hypothetical
  non-proxy-respecting process (e.g. a renderer-sandbox escape, which the agent
  cannot launch) would have a direct route; defense-in-depth (read-only rootfs,
  CapDrop ALL, no-new-privileges, non-root, pids/mem caps, optional gVisor)
  mitigates that class, and a network-level egress boundary is recorded as
  follow-on hardening (§9).
- **Chrome resource weight** — higher mem default, pids/cpu caps, idle +
  max-lifetime reaping, reap-on-start crash recovery.
- **Real-world page messiness** (anti-bot, SPAs) — the harness stealth script,
  networkIdle wait with low-level fallback, `wait_for`/`wait_ms` knobs; the live
  proof includes a real public page.
- **chromedp/Chrome dependency weight** — chromedp is already a harness
  dependency; the Chrome image is bundled and overridable via
  `RUNTIME_BROWSER_IMAGE`.
