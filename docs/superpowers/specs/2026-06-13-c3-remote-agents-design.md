# C3 — Remote Agents (attach instead of spawn): Design Spec

**Date:** 2026-06-13
**Status:** Approved (brainstorm complete)
**Sub-project:** C3 (Remote agents), Milestone 1

---

## Section 1 — Goal & Scope

**Goal:** Let `runtimed` attach to an already-running, contract-conformant `agentd`
on a remote host — health-checking, proxying, and status-reporting it exactly like
a local agent — without spawning or supervising it.

### In scope (C3 M1)

- A `url:` config variant marking an agent **remote**: skip spawn, monitor-only.
- Pluggable scheme: `http://` or `https://` (the operator's TLS choice — real cert,
  service mesh, or ingress; no TLS machinery in runtime itself for M1).
- Opt-in bearer-token mutual auth: runtimed sends it on every proxied / health /
  metrics request; the remote `agentd` rejects `401` when its token is set and the
  header is absent or wrong.
- Attach + monitor lifecycle: a health-poll loop (not a `Supervisor`), status
  `reachable | unreachable`, **never** "restarting".
- Degrade-don't-fail: a remote agent that is down never blocks runtimed boot and
  never crashes it; proxying it returns `503` until it comes up.

### Out of scope (explicitly backlogged)

- **Centralized secret brokering to remote hosts.** The remote `agentd`'s
  environment (PG DSN, tenant, secrets, gateway, memory opt-in) is
  **operator-provisioned** — its own systemd unit / Kubernetes Deployment+Secret /
  `docker run -e`. Brokering stays a local-spawn-only feature. (A
  registration-handshake variant — agent boots with a token, pulls config + brokered
  secrets from the control plane — is deferred to **C3 M2**.)
- **mTLS.** The bearer token covers the routable-port threat in M1; mTLS is a later
  hardening milestone (cert provisioning/rotation, a CA story, agentd TLS-server
  changes).
- **Per-agent-pod scheduling / operator / CRDs.** C3 makes remote attach *possible*;
  orchestrating it is the next C2 milestone built on this.

### Framing

A Kubernetes-scheduled agent is just a remote agent whose lifecycle the orchestrator
owns. M1 delivers the "remote agent" half: the control plane attaches to a process it
did not start and does not restart.

### Why this is a small change

The data plane is **already location-agnostic**. The control plane reverse-proxies
plain HTTP to an agent address (`controlplane/proxy.go` `reverseProxy`), routes
`/agents/{id}/...` to it (`controlplane/api.go`), health-checks via `GET /healthz`,
and scrapes `/metrics` — all against any reachable address. The **only** local-bound
piece is the spawn/supervise step (`AgentProcess.SpawnFunc()` execs the binary and
`Supervisor` babysits the PID). C3 adds an attach-and-monitor path beside it.

---

## Section 2 — Config Schema & Validation

### New fields on `AgentConfig` (`internal/config/config.go`)

```go
URL       string `yaml:"url"`        // remote agent: full base, e.g. "https://agent-1.svc:8443". Mutually exclusive with listen_addr.
AuthToken string `yaml:"auth_token"` // optional bearer for the remote hop; ${VAR}-expanded. Only valid with url.
```

**Remote ⇔ `URL != ""`.** A local agent sets `listen_addr` (unchanged behavior); a
remote agent sets `url`.

### Validation rules (added to `Config.Validate`)

- **Exactly one** of `listen_addr` / `url` per agent. Neither or both → error naming
  the agent index/id.
- `url` must parse via `net/url`, have scheme `http` or `https`, and a non-empty
  host. Anything else → error.
- **Local-only fields are invalid on a remote agent:** `command`, `workdir`, `kind`,
  `memory: true`, and any `gateway:` mode (`full`/`search`) → error. These are all
  spawn-time env injections runtimed cannot deliver to a process it did not start;
  failing loudly beats silently ignoring them.
- `auth_token` is only valid with `url` → error otherwise.
- **Uniqueness:** the existing `listen_addr` duplicate check becomes a unified
  dial-identity check — no two agents may share the same `listen_addr` *or* the same
  `url`.
- `auth_token` gets the same `${VAR}` env-expansion the gateway fields already use
  (`expandEnvScalar`), so tokens stay out of the YAML. An unset/empty referenced var
  is a hard error (matches existing fail-closed expansion semantics).

`tenant` still applies to a remote agent — identity/authorization use it for access
control on the proxy side. It is the *spawn-time* injections that do not apply.

### Example

```yaml
agents:
  - id: local-1
    name: Local Agent
    model: test/scripted
    listen_addr: 127.0.0.1:8101          # local: spawned + supervised as today
  - id: remote-1
    name: Remote Agent
    model: test/scripted
    tenant: acme
    url: https://agent-1.internal:8443   # remote: attached + monitored
    auth_token: ${REMOTE_1_TOKEN}        # optional bearer for the hop
```

---

## Section 3 — Dial Identity & the Four Dial Sites

### `AgentProcess` (`controlplane/proxy.go`) gains

```go
Remote    bool   // true ⇒ attach-only (no spawn, no Supervisor)
BaseURL   string // full dial base "scheme://host:port"; for local agents synthesized as "http://"+Addr
AuthToken string // optional bearer for the remote hop ("" ⇒ no auth header)
```

`Addr` stays — local agents still need the bare host:port for `RUNTIME_LISTEN_ADDR`
at spawn. The registry (`NewRegistry`) sets per agent:

- **local:** `Addr = listen_addr`, `BaseURL = "http://" + listen_addr`,
  `Remote = false`
- **remote:** `BaseURL = url`, `Addr = ""`, `Remote = true`,
  `AuthToken = auth_token`

### Central dial-base helper

```go
func (a AgentProcess) baseURL() string {
    if a.BaseURL != "" {
        return a.BaseURL
    }
    return "http://" + a.Addr // back-compat fallback
}
```

### Auth round-tripper

```go
type authTransport struct {
    token string
    base  http.RoundTripper // nil ⇒ http.DefaultTransport
}

func (t authTransport) RoundTrip(r *http.Request) (*http.Response, error) {
    base := t.base
    if base == nil {
        base = http.DefaultTransport
    }
    if t.token != "" {
        r = r.Clone(r.Context())
        r.Header.Set("Authorization", "Bearer "+t.token)
    }
    return base.RoundTrip(r)
}
```

The clone is required: mutating the caller's `*http.Request` would leak the header
back to the proxy's request object.

### The four dial sites

| Site | File | Change |
|---|---|---|
| Reverse proxy | `controlplane/proxy.go` `reverseProxy` | take a base URL + token; `url.Parse(base)`; set `rp.Transport = authTransport{token, nil}` |
| `/agents` healthz | `controlplane/api.go` (`GET /agents`) | client dials `base+"/healthz"` with the auth header |
| Metrics fanout | `internal/obs/fanout.go` `scrapeOne` | `ScrapeTarget` gains `BaseURL`+`Token`; scrape `base+"/metrics"` with the auth header |
| Startup gate | `cmd/runtimed/main.go` `waitAgentHealthy` | local agents only — remote agents skip it (Section 4) |

`ScrapeTarget{Agent, Addr}` becomes `ScrapeTarget{Agent, BaseURL, Token}`;
`main.go`'s target builder fills them from `reg.Get`. The `obs` package stays
decoupled — it learns a richer target shape, no controlplane import.

`reverseProxy`'s signature changes from `reverseProxy(addr string, onError func())`
to `reverseProxy(base, token string, onError func())`. The `/agents/{id}/` handler
(`api.go`) passes `ap.baseURL()` and `ap.AuthToken`.

Every site goes through `baseURL()` + `authTransport`, so there is one code path for
local and remote; the remote/local distinction stays visible in status and metrics
rather than being hidden behind a shim.

---

## Section 4 — Lifecycle: Supervise (local) vs. Monitor (remote)

Today `cmd/runtimed/main.go` gives **every** agent a `Supervisor` goroutine plus a
blocking ~30s health gate. C3 splits this on `ap.Remote`.

### Local agents — unchanged

`Supervisor{Spawn: ap.SpawnFunc(), Backoff: time.Second, OnRestart: ...}` +
`waitAgentHealthy` blocking gate + restart-on-exit with bounded backoff. Byte-for-byte
as today.

### Remote agents — a new monitor loop

```go
// controlplane/monitor.go
type HealthMonitor struct {
    BaseURL  string
    Token    string
    Interval time.Duration        // default 10s
    OnChange func(reachable bool) // status-transition hook (metrics + log); fired edge-triggered
}

func (h *HealthMonitor) Run(ctx context.Context) // polls BaseURL+"/healthz" until ctx done
```

- **No spawn, no PID, no restart.** We do not own the process — we observe it.
- **Never blocks boot.** A remote agent that is down at startup is logged `WARN` and
  left `unreachable`; runtimed serves normally.
- **Status is `reachable | unreachable`,** flipped by the poll. `OnChange` fires only
  on a transition (edge-triggered), logging the change and updating a metric — never
  the word "restarting".
- **Proxying while unreachable** returns the existing `503 "agent unavailable"` from
  `reverseProxy`'s `ErrorHandler` — no new path.
- The probe request carries the bearer (via the same `authTransport`), so a
  token mismatch shows as `unreachable` (401 on health) — fail-closed and visible.

### `main.go` startup loop

```go
for _, info := range reg.List() {
    ap, _ := reg.Get(info.ID)
    if ap.Remote {
        hm := &controlplane.HealthMonitor{
            BaseURL: ap.baseURL(), Token: ap.AuthToken,
            OnChange: func(ok bool) { cm.AgentReachable(ap.AgentID, ok) },
        }
        go hm.Run(ctx)
        slog.Info("monitoring remote agent", "agent", ap.AgentID, "url", ap.baseURL())
        continue // no blocking gate — remote agents may come up later
    }
    sup := &controlplane.Supervisor{
        Spawn: ap.SpawnFunc(), Backoff: time.Second,
        OnRestart: func() { cm.AgentRestart(ap.AgentID) },
    }
    go sup.Run(ctx)
    slog.Info("supervising agent", "agent", ap.AgentID, "addr", ap.Addr)
    if err := waitAgentHealthy(ctx, ap.Addr, 30*time.Second); err != nil {
        slog.Warn("agent not healthy yet; continuing", "agent", ap.AgentID, "err", err)
    }
}
```

### Status surfacing

`GET /agents` already performs a live per-request `/healthz` check, so `healthy:false`
already reflects an unreachable remote agent once that check dials the base URL + token
(Section 3) — no extra wiring needed there. The `HealthMonitor` adds the *continuous*
signal (a reachability metric + transition logs) that a per-request check cannot give.

### New metric

`cm.AgentReachable(agentID string, reachable bool)` on `*obs.ControlMetrics` — a
gauge (1/0) labeled by agent, set on each monitor transition. Nil-receiver-safe like
the other `ControlMetrics` methods.

---

## Section 5 — Remote `agentd`: Optional Bearer-Token Auth

A remote `agentd` listens on a routable address, so it must authenticate runtimed
back. This is the only change to the agent side, and it is local-spawn-compatible.

### New env var (`cmd/agentd/main.go`)

```go
authToken := os.Getenv("RUNTIME_AGENT_AUTH_TOKEN") // "" ⇒ no auth (loopback/local, unchanged)
```

Threaded into `agentruntime.Serve` via a new `Config` field (`AuthToken string`),
set in `main` — preserving `Serve`'s convention that env is read at the edges, never
by a kind builder.

### Middleware (`agentruntime/server.go` `handler()`)

```go
func (m *Manager) handler() http.Handler {
    mux := m.newMux()
    logged := /* existing access-log wrapper */
    var h http.Handler = logged
    if m.authToken != "" {
        h = requireBearer(m.authToken, logged) // just inside RequestID
    }
    return obs.RequestID(h)
}

// requireBearer rejects requests whose Authorization header != "Bearer <token>"
// with 401, using a constant-time compare. Applies to ALL paths including
// /healthz and /metrics — a remote agent's probe endpoints sit on the same
// routable port, so they get the same protection (runtimed sends the token on
// those requests too).
func requireBearer(token string, next http.Handler) http.Handler { /* ... */ }
```

### Key decisions

- **Default off.** Unset token ⇒ no middleware ⇒ local spawned agents and existing
  deployments are byte-for-byte unchanged. The check exists only when an operator
  sets the token.
- **Covers `/healthz` and `/metrics`.** They are on the same routable port; runtimed
  sends the bearer on health checks and metric scrapes (Section 3), so protecting them
  closes the unauthenticated-probe gap. `crypto/subtle.ConstantTimeCompare` avoids a
  timing oracle.
- **Symmetry.** runtimed's `auth_token` and agentd's `RUNTIME_AGENT_AUTH_TOKEN` are
  the **same shared secret**, delivered to each side by its own operator (runtimed via
  `${VAR}` in `runtime.yaml`; agentd via its pod/unit env). A mismatch ⇒ runtimed gets
  `401` on health ⇒ the agent shows `unreachable` (fail-closed, visible).

### Operator note (documentation, not code)

The remote `agentd` is started by the operator with its full environment:
`RUNTIME_PG_DSN`, `RUNTIME_LISTEN_ADDR` (its own bind address), `RUNTIME_AGENT_ID`,
`RUNTIME_AGENT_TENANT`, `RUNTIME_AGENT_KIND`, and `RUNTIME_AGENT_AUTH_TOKEN`. This is
the operator-provisioned half of the secrets-delivery decision.

---

## Section 6 — Testing & Live Proof

### Hermetic unit tests (no Postgres; run in default `go test ./...`)

| Area | Test file | Cases |
|---|---|---|
| Config | `internal/config/config_test.go` | `url` parses; remote/local mutual exclusion (neither/both → error); bad scheme (`ftp://`, no host) → error; `command`/`kind`/`memory`/`gateway` on a remote agent → error; `auth_token` without `url` → error; `url` duplicate detection; `${VAR}` expansion of `auth_token` (set; unset → error) |
| Registry | `controlplane/registry_test.go` | remote agent → `Remote=true`, `BaseURL=url`, `AuthToken` set, `Addr=""`; local → `BaseURL="http://"+listen_addr`, `Remote=false`; `baseURL()` fallback |
| Auth transport | `controlplane/proxy_test.go` | `authTransport` adds `Bearer` when token set; omits when empty; clones request (no header leak to caller) |
| Reverse proxy | `controlplane/proxy_test.go` | proxies to an `httptest` backend over the base URL; sends bearer; `503` when backend down |
| Health monitor | `controlplane/monitor_test.go` | `OnChange` fires on transition only (edge-triggered, not every poll); flips reachable→unreachable when the backend stops; honors ctx cancel; sends bearer on the probe |
| agentd auth | `agentruntime/server_test.go` | no token ⇒ all paths open (back-compat); token set ⇒ `401` on missing/wrong header, `200` on correct, including `/healthz` + `/metrics`; constant-time compare path exercised |
| Metrics fanout | `internal/obs/fanout_test.go` | `ScrapeTarget{BaseURL,Token}` scrapes over an https base + bearer |

### Integration test (`//go:build integration`, Postgres.app)

`test/integration/remote_agent_test.go` — start a real `agentd` (scripted kind,
`RUNTIME_AGENT_AUTH_TOKEN` set) on a port, point a runtimed `Registry` at it via
`url:` + matching `auth_token`, and assert:

- `/agents` shows it `healthy:true`.
- A full session round-trip (`POST /sessions` → stream → `GET`) proxies through with
  the bearer.
- Wrong token ⇒ `healthy:false` + proxy `401`/`503`.
- Killing the agentd flips it to `unreachable` **without** a restart (no new PID, no
  `AgentRestart` metric tick).

Integration tests self-clean their DB and the `dbos` schema, per workspace convention.

### Live proof (the milestone gate; mirrors C2's bar)

1. `make build` both binaries. Start a remote `agentd` in a **separate process**
   (scripted agent, auth token set) — standing in for "another host."
2. Start `runtimed` with a `runtime.yaml` mixing one **local** spawned agent and one
   **remote** `url:` agent (token via `${VAR}`).
3. Prove: `runtimectl conformance` passes against the **remote** agent through the
   proxy; `/agents` shows both healthy; status distinguishes local vs. remote; `/metrics`
   includes the remote agent's scraped series (auth applied).
4. Prove degrade-don't-fail: `kill` the remote agentd → `/agents` flips it to
   `healthy:false`, runtimed stays up, the **local** agent keeps working, proxying the
   dead one returns `503`, and **no restart** occurs. Restart the agentd → it returns to
   `reachable` on its own.
5. Prove fail-closed auth: start the remote agentd with a **different** token →
   runtimed reports it `unreachable` (401 on health) and never proxies to it.

### Conventions honored

- The `go` CLI is ground truth (ignore IDE/LSP diagnostics from the `replace ../harness`
  setup).
- Integration tests use Postgres.app at
  `postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable` and self-clean.
- No secrets committed or echoed: tokens via env, `${VAR}` in YAML.
- Scripted model (`test/scripted`) so no LLM key is needed for the proof.

---

## Open items deferred to later C3 milestones

- **C3 M2 — registration handshake:** remote agent boots with a registration token,
  pulls its DSN + brokered tenant secrets from a new control-plane endpoint
  (centralized brokering for remote hosts). Needs token issuance/rotation and an
  agentd fetch-instead-of-read-env path.
- **mTLS** mutual auth (CA, cert rotation, agentd TLS server).
- **Per-agent-pod scheduling / operator / CRDs** (C2, built on this milestone).
