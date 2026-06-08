# Runtime Spine — Milestone 3: Operability Layer — Design Spec

**Date:** 2026-06-08
**Status:** Approved (design phase)
**Builds on:** Milestone 1 (durable spine) + Milestone 2 (multi-agent platform). M1 spec §11 defines M3 as the operability layer.

---

## 1. Goal

Make the multi-agent platform safe to operate and pleasant to observe. M1 proved
durability; M2 made it multi-agent — both deliberately headless and
unauthenticated. M3 adds the operability layer: **token auth** on the control
plane, a **read-only web console**, **structured logging**, a **contract
conformance suite**, and deployment polish (bounded shutdown, friendlier proxy
errors, a working full-stack Compose).

M3 is about *operating and trusting what exists* — NOT new agent capability. It
explicitly does not add Identity/RBAC, Observability dashboards, the Gateway,
Memory, or sandboxes (each its own sub-project).

## 2. Design decisions (locked)

| Decision | Choice | Rationale |
|---|---|---|
| Console tech | **Server-rendered Go `html/template` + vanilla JS/SSE** | Zero build step, no JS toolchain, `//go:embed` into the binary; native `EventSource` covers the live view. Fits single-binary ethos. |
| Auth model | **Named tokens in `runtime.yaml`** (`token → label`) | Config-driven (consistent with agents), supports multiple clients + rotation, label enables log attribution, no DB. |
| Hosting | **Served by `runtimed`, same port** | One binary, one port, one thing to secure. Auth middleware wraps API routes; console under `/ui`. |
| Conformance | **Reusable Go test harness + `runtimectl conformance` wrapper** | CI-usable for agent authors AND operator-runnable against a live agent. |
| Polish included | Bounded graceful shutdown, 503-on-restart, full-stack Compose | Flagged debt; completes the deploy story. |
| Deferred | Per-agent cached proxy; Identity/RBAC; Observability; Gateway; Memory; sandboxes; dynamic token management | Separate sub-projects / lower priority. |

## 3. Architecture (delta from M2)

```
  Browser ─────────▶ /ui (console, token-gated)        ┐
  runtimectl ──────▶ /agents/{id}/*  (token-gated)     │ one runtimed
  (RUNTIME_TOKEN)    /agents, /healthz                 │ HTTP server,
                          │                            │ same port
                   ┌──────▼───────────────────────┐    │
                   │ auth middleware (bearer token)│    │
                   │  • /healthz exempt            │    │
                   │  • everything else gated      │    │
                   └──────┬───────────────────────┘    ┘
                          │ (unchanged M2 router + supervisors)
                          ▼
                  agentd × N  ──▶  Postgres
```

Everything below the auth middleware + console is **unchanged M2**: the
registry, the `/agents/{id}` router, per-agent supervisors, the durable agent
runtime. M3 adds a middleware wrapper, console handlers, a `tokens` config
section, structured logging throughout, and the conformance package.

## 4. Components

### 4.1 Token auth
- **Config**: `runtime.yaml` gains an optional `tokens:` list:
  ```yaml
  tokens:
    - token: "s3cr3t-abc"
      label: "ci"
    - token: "s3cr3t-xyz"
      label: "ops-console"
  ```
  `internal/config` parses + validates (non-empty token; unique tokens; label
  optional but recommended). **If `tokens` is empty/absent, auth is DISABLED**
  (open mode) — preserves the current dev experience and keeps the change
  backward-compatible; `runtimed` logs a clear warning at startup when running
  open.
- **Middleware** (`controlplane`): wraps the API mux. Checks
  `Authorization: Bearer <token>` against the configured set. `GET /healthz` is
  exempt (liveness probes). On miss → `401`. On hit → request proceeds; the
  matched token's label is attached to the request context for log attribution.
- **CLI**: `runtimectl` sends `Authorization: Bearer $RUNTIME_TOKEN` on every
  request when the env var is set.
- **Console**: a token is required to view the console. M3 keeps it simple — a
  token entered in a form is stored in a cookie; the console's own
  fetches/SSE include it (the auth middleware accepts the token from EITHER the
  `Authorization: Bearer` header OR the cookie, so browser navigations and
  `EventSource` — which can't set headers — both work). (No sessions/users —
  just the same bearer token.) **Exempt from auth** (to avoid a chicken-and-egg):
  `GET /healthz` and the console's token-entry page + its static assets; every
  data route and console data view is gated.

### 4.2 Read-only web console (`/ui`)
Served by `runtimed` from `//go:embed`-ed templates + a small static JS/CSS file.
Pages (all read-only):
- **`/ui`** — fleet overview: agents (id, name, model, health), each linking to
  its sessions.
- **`/ui/agents/{id}`** — that agent's sessions (id, status, turn_count), each
  linking to a live view.
- **`/ui/agents/{id}/sessions/{sid}`** — live session view: replays the event
  log then streams live via `EventSource` against the existing
  `/agents/{id}/sessions/{sid}/stream` SSE endpoint.

The console calls the SAME control-plane API it's hosted beside (agents list,
sessions list, SSE stream) — it adds no new data paths, just a rendering layer.
Health for the overview comes from probing each agent's `/healthz` through the
router (or a new lightweight `GET /agents` field — see §4.5).

### 4.3 Structured logging
Standardize on `slog` with consistent structured fields across `runtimed`,
`controlplane`, and `agentruntime`:
- Replace `log.Printf` calls with `slog` (control plane lifecycle, supervisor
  events, proxy errors).
- Consistent keys: `agent`, `session`, `turn`, `token_label` (when authed),
  `remote`. 
- A single `slog` setup in `runtimed` main (text handler by default; JSON via
  `RUNTIME_LOG_FORMAT=json`). This is the lightweight precursor to the
  Observability sub-project — no metrics/tracing here.

### 4.4 Contract conformance suite
- New package `conformance` exporting `Run(t TestingT, baseURL string)` (or a
  results struct) that exercises an agent's contract at `baseURL`:
  `GET /healthz` (200), `GET /meta` (has agent_id + contract_version), `POST
  /sessions` (returns session_id), `GET /sessions/{id}/stream` (SSE, reaches a
  terminal `done`), `GET /sessions` (lists the session), `GET /sessions/{id}`
  (status). Each check reports pass/fail with a clear message.
- `TestingT` is a minimal interface (`Errorf`, `Fatalf`, `Logf`) so the suite
  runs under `go test` AND from the CLI wrapper (with a tiny adapter).
- **CLI**: `runtimectl conformance --agent <id>` runs the suite against that
  agent *through the control plane* (so it also exercises routing + auth) and
  prints a pass/fail report.
- An integration test runs the suite against the bundled test agent to prove
  the suite itself works.

### 4.5 Deployment polish
- **Bounded shutdown**: `runtimed`'s `srv.Shutdown` gets a timeout
  (`context.WithTimeout`, e.g. 10s) so a hung SSE stream can't block shutdown.
- **503 on restart**: the reverse proxy gets an `ErrorHandler` that returns
  `503 "agent unavailable"` (instead of the default 502) when the backend dial
  fails — clearer for operators and the console.
- **`GET /agents` health field**: add a best-effort `status`/`healthy` field per
  agent (probed via the agent's `/healthz`) so the console overview shows health
  without N client round-trips. (Cheap; cache briefly or probe on request.)
- **Full-stack Compose**: `deploy/docker-compose.yml` extended (or a new
  `docker-compose.full.yml`) with a `runtimed` service built from a Dockerfile
  (multi-stage Go build producing `runtimed` + `agentd`), wired to the
  `postgres` service, mounting a `runtime.yaml`, exposing `:8080`.

## 5. What stays unchanged from M2
Registry, `/agents/{id}` routing mechanics, per-agent supervision, the durable
DBOS workflow + agent contract endpoints, the store schema (auth tokens live in
config, not the DB). The console and CLI are additive consumers; auth is a
middleware wrapper.

## 6. Error handling
- **Missing/invalid token** (auth enabled) → `401` with a plain message;
  `/healthz` exempt.
- **Auth disabled** (no tokens configured) → allowed, with a startup warning
  logged once.
- **Console without token** → redirect to a token-entry form; bad token → 401.
- **Conformance failures** → reported per-check (non-fatal aggregation): the
  CLI exits non-zero if any check fails.
- **Config errors** (dup tokens, empty token string) → `runtimed` exits
  non-zero at startup (consistent with M2 agent validation).
- **Backend agent down** → 503 via proxy ErrorHandler (console shows the agent
  unhealthy).

## 7. Testing strategy
- **Hermetic unit tests**: config token parse/validate; auth middleware
  (no-token-open, valid/invalid/missing token, healthz-exempt, label in
  context) via `httptest`; conformance suite run against an `httptest`
  fake-agent (both passing and deliberately-broken fakes, to prove the suite
  catches violations); console template rendering (handlers return 200 + expected
  content with a stubbed registry/store); proxy ErrorHandler returns 503.
- **Integration tests (gated)**: conformance suite against the real bundled test
  agent through `runtimed` (with auth on); a console smoke test (load `/ui`,
  assert it lists agents) against a live two-agent runtimed; auth end-to-end
  (request without token → 401, with token → 200).
- **Regression**: M1 resume test + M2 multi-agent test must still pass (auth
  middleware must not break the routed contract — tests pass a token).

## 8. Scope boundaries (M3 does NOT do)
Identity/RBAC/users, OAuth, secrets brokering (Identity sub-project);
metrics/tracing/dashboards (Observability sub-project); write actions from the
console (deploy/stop via UI); dynamic token CRUD (config-only in M3); the
Gateway, Memory, sandboxes, containers; per-agent proxy caching (deferred).

## 9. Internal milestones (sequencing hint for planning)
1. **Auth + config tokens + structured logging** — cross-cutting foundation;
   middleware, token config, slog standardization, CLI token header. Regression:
   M1/M2 integration tests pass with a token.
2. **Conformance suite** — the `conformance` package + `runtimectl conformance`;
   high-leverage pure addition.
3. **Read-only console** — templates, static assets, console handlers, the live
   SSE view; the visible payoff.
4. **Deployment polish** — bounded shutdown, 503 ErrorHandler, `/agents` health
   field, full-stack Compose + Dockerfile; final verification + README update.

## Appendix — key signatures to add/evolve
- `config.Config` gains `Tokens []TokenConfig{ Token, Label string }`; `Validate`
  checks non-empty + unique tokens.
- `controlplane.AuthMiddleware(next http.Handler, tokens map[string]string) http.Handler`
  (token→label). **Decision:** `NewAPI(reg)` stays as-is (focused on routing);
  `runtimed` wraps its returned mux with `AuthMiddleware` (and mounts the
  console) before serving. Keeps routing and auth as separate, independently
  testable concerns.
- `conformance.Run(t TestingT, baseURL string)` + `TestingT` interface.
- `runtimectl conformance --agent <id>`; all `runtimectl` requests gain the
  bearer header from `RUNTIME_TOKEN`.
- Console: `controlplane` (or a new `console` package) handlers + embedded
  `templates/*.html` + `static/*`.
- `runtimed`: slog setup, bounded shutdown, proxy ErrorHandler, console mount,
  auth wiring.
