# Runtime — Roadmap & Backlog

**Checkpoint date:** 2026-06-09 (Identity M2)
**Current state:** Runtime spine complete (Milestones 1–3 merged to `master`);
polyglot agent hosting (C1) first milestone complete — Level-1 OpenAI Agents SDK
shim merged to `master`, hosting a full foreign agent end-to-end (see §C1);
Identity (B3) first two milestones complete — M1 multi-tenant, edge-enforced
access control (OIDC + service keys + per-agent RBAC), and M2 per-tenant secrets
brokering (AES-256-GCM provider credentials injected into agents at spawn), both
merged to `master` (see §B3).
**Goal:** an on-prem, open-source equivalent of AWS Bedrock AgentCore.

This file is the parking lot for everything *not yet built*. Each item below is a
future unit of work (its own brainstorm → spec → plan → execute cycle, the same
flow used for M1–M3). Design specs and plans live in `docs/superpowers/`.

---

## ✅ Done — the Runtime spine (sub-project 1 of 6)

The first sub-project from the original decomposition is complete, in three
milestones (all merged to `master`):

- **M1 — Durable walking skeleton** (`a163b1f`): one agent as a supervised
  subprocess; harness loop wrapped as a DBOS workflow (turn = durable step);
  kill-mid-turn resume proven. Added `RunTurn` to harness.
- **M2 — Multi-agent platform** (`81a11b8`): config-driven registry
  (`runtime.yaml`), `/agents/{id}` path routing, one subprocess per agent,
  session status/turn tracking, cross-agent session listing, full CLI.
- **M3 — Operability layer** (`755fc6d`): token auth (header/cookie, open mode),
  read-only web console (`/ui`), structured `slog`, contract conformance suite +
  `runtimectl conformance`, bounded shutdown, 503-on-restart, per-agent health,
  full-stack Dockerfile + compose.

Reference docs: `docs/superpowers/specs/2026-06-07-runtime-spine-design.md` (the
overall Approach-2 design + the 6-sub-project decomposition) and the per-milestone
specs/plans dated 2026-06-07/08.

---

## 🔜 Remaining work

### A. Spine hardening (within this sub-project — optional, do when needed)

Carried-forward debt flagged during M1–M3 reviews. None blocking; pick up if/when
the use case demands. Recorded in the M3 README "Status, scope & limitations".

1. **Subprocess pools / replicas per agent** — today one subprocess per agent.
   Needs session→replica affinity (a session must hit the replica whose DBOS
   workflow it is). Pulls in real routing complexity. (Deferred from M2.)
2. **Autoscaling** — scale replicas by load. Depends on pools (A1).
3. **Dynamic deploy** — `POST /agents` runtime registration + rollback; today
   agents come from `runtime.yaml` at startup. Tokens are config-only too.
4. **`session_events` concurrency** — `SELECT MAX(seq)+1` is safe only because
   one subprocess owns a session (one writer). Revisit (lock / sequence /
   `ON CONFLICT` retry) if a session ever gets concurrent writers (e.g. pools).
5. **DBOS recovery across a recompiled binary** — recovery keys on the agentd
   binary's app-version hash; recovering a workflow across a code change needs
   `DBOS__APPVERSION` pinned. Document/operationalize if doing rolling upgrades.
6. **Cross-agent aggregate session view** — session listing is per-agent; a
   fleet-wide view (and richer console health) is future console work.
7. **Constant-time token compare + token hashing-at-rest** — M3 uses a plain map
   lookup; fine for static config tokens. Folds naturally into Identity (B3).
8. **Access-log already wired** (M3) — but no metrics/tracing yet (see B5).

### B. The other 5 sub-projects (from the original decomposition)

Each is a peer of the Runtime spine — its own spec → plan → build cycle. Rough
dependency order: they all sit on the spine; Identity should likely precede
exposing the platform broadly.

1. **Gateway** — tool / MCP federation. Turn APIs/services into agent-callable
   tools; a central MCP endpoint with discovery, auth, and semantic tool search.
   Builds on harness `tools/mcp`. Independently useful (any agent can point at it).
2. **Memory** — managed multi-tenant memory. Short + long term, semantic
   retrieval across sessions, per-tenant isolation. Builds on harness
   `tool/memory` + Postgres/pgvector (pgvector is already provisioned in the
   Compose image, unused so far).
3. **Identity** — proper auth done right: agent identity, secrets brokering,
   OAuth, RBAC, per-user/multi-tenant. Supersedes M3's simple bearer tokens.
   **First milestone DONE (merged to `master`, 2026-06-09):** multi-tenant,
   edge-enforced access control. External OIDC for human login + platform-issued
   service keys (`svk-…`, bcrypt-hashed, constant-time verify) for machines;
   roles admin/operator/viewer scoped per agent; tenant-filtered `/agents` and
   console; cross-tenant requests return 404 (existence hidden). New
   `internal/identity` package (Principal/Authorizer/Authenticator/Store) behind
   an identity middleware at the control-plane edge — **agents stay unmodified**.
   Hybrid admin: agent→tenant in `runtime.yaml`, tenants/users/keys in Postgres
   via a `runtimectl admin` API; `RUNTIME_ADMIN_BOOTSTRAP` break-glass superuser.
   Backward-compatible: absent tenant → `default`, no identity configured → open
   mode, legacy `tokens:` still work (deprecated → default-tenant superusers).
   Absorbs A7 (hashing-at-rest + constant-time compare). Spec/plan:
   `docs/superpowers/{specs,plans}/2026-06-08-identity-m1*`.

   **Second milestone DONE (merged to `master`, 2026-06-09):** per-tenant secrets
   brokering. Tenants store provider credentials (generic named env vars)
   encrypted at rest with AES-256-GCM under an operator master key
   (`RUNTIME_SECRETS_KEY`); a `Broker` in `internal/identity` (Cipher + store)
   decrypts them at spawn time and the registry injects them into the tenant's
   agent subprocesses' environment (tenant secrets shadow the inherited operator
   env; fail-closed on a decrypt error). Write-only `/admin/secrets` API +
   `runtimectl admin secret set/ls/rm`. Disabled and fully backward-compatible
   when no master key is set; agents stay unmodified. Spec/plan:
   `docs/superpowers/{specs,plans}/2026-06-09-identity-m2-secrets-brokering*`.

   **Remaining Identity work:** secrets **key rotation** (re-encrypt all rows on a
   master-key change; per-tenant keys; optional AAD binding of tenant/name into
   the AEAD to defeat DB-level row swaps), fine-grained/custom RBAC beyond the 3
   roles, cross-tenant users + user self-service, an admin console UI (incl.
   superuser GET/DELETE secrets across a target tenant — POST already supports
   it), and optional local password accounts (the `Authenticator` interface
   already admits new methods). Console CSRF (`state`/`nonce`) is a known M1
   limitation. (Absorbs A7 — done.)
4. **Sandboxes** — isolated **browser tool** + **code interpreter** for agents.
   Integrate gVisor/Firecracker for isolation; chromedp for browser. The
   conformance suite (M3) already validates the agent contract that sandboxed
   tools would run behind.
5. **Observability** — tracing, metrics, dashboards. The structured `slog` from
   M3 is the lightweight precursor; this is the full version (OpenTelemetry +
   Grafana, or similar). (Absorbs A8.)

### C. Cross-cutting / platform-level (later)

### C1. Polyglot agent hosting (foreign SDK agents via the contract)

**Status (2026-06-08):** First milestone COMPLETE and merged to `master` — the
Level-1 Python contract shim hosts the OpenAI Agents SDK, and the generalized
spawn path (the prerequisite) is in. A real, non-trivial foreign agent (the SG
Nutrition Investigator: 4 tools, SFA additives data, typed `NutritionVerdict`
rendered to prose, cross-run memory) runs as a first-class runtime agent, proven
live (`runtimectl conformance` PASSED + a real vision verdict streamed
end-to-end through the control-plane proxy; Level-1 durability + learned-alias
memory confirmed). The reusable contract layer lives at `contrib/shims/python/`
as the standalone, path-installable `runtime_contract` package; the worked agent
+ its ~30-line adapter live at `examples/nutrition-label-openai/` (`agent.py`,
`adapter.py`, `serve.py`, Makefile, `runtime.nutrition-openai.yaml`). Spec:
`docs/superpowers/specs/2026-06-08-nutrition-openai-on-runtime-design.md`; plan:
`docs/superpowers/plans/2026-06-08-nutrition-openai-on-runtime.md`.

**Author-surface cleanup (2026-06-08, post-milestone):** both contract layers now
keep operator/deployment parameters off the agent-author surface, symmetrically.
Go — `agentruntime.Config` shrank to `{Spec, Provider, Tools}`; `Serve` reads
`RUNTIME_PG_DSN` + `RUNTIME_LISTEN_ADDR` from the injected env, so a builder never
carries connection details (a builder that tried wouldn't compile). Python —
`runtime_contract.serve(adapter)` is the analog: it reads `RUNTIME_LISTEN_ADDR` /
`RUNTIME_AGENT_ID` / `RUNTIME_SHIM_DB` itself and builds the store + app + server,
so an adapter author only writes `run()`. This is the reusable pattern the next
language/framework should follow.

**Remaining C1 work:** Level 2 (in-flight crash resume), a second framework/adapter
to prove reuse, and a TS shim — all below.

Host agents written in **any language / framework** (OpenAI Agents SDK, Claude
Agent SDK, PydanticAI, CrewAI, LangGraph/LangChain, Google ADK, …), not just
harness-native Go subprocesses. The agent HTTP/SSE contract was deliberately
designed to admit this, and most of the platform is already framework-agnostic:
routing (`reverseProxy`), supervision, auth, the `/ui` console, `runtimectl`, and
the conformance suite all operate on the wire contract, not on Go types. The only
harness-specific layer is the DBOS durable loop inside `agentd`/`agentruntime`.

**The integration seam is "one contract layer per language + a thin adapter per
framework."** A foreign agent just has to speak the 6 contract endpoints
(`/healthz`, `/meta`, `POST /sessions`, `GET /sessions/{id}/stream?since=N`,
`GET /sessions/{id}`, `GET /sessions`). Write the contract server once per language
(~100 lines: endpoints + SSE framing + session bookkeeping + `?since=N` replay +
a `serve()`-style entry point that reads the operator env), then a ~10-30 line
per-framework adapter maps that framework's run/stream API to the contract's
`text`/`tool_result`/`done`/`error` events — the adapter author writes only the
adapter, never deployment glue. One Python shim then covers OpenAI SDK, PydanticAI,
CrewAI, LangGraph, LangChain, ADK; one TS shim covers the JS frameworks.

**Two durability levels** (a foreign agent being *hosted* is separate from being
*durable*):

- **Level 1 — conversation resume** (restart the process, the chat continues).
  Cheap: a persistent per-session message store (e.g. the SDK's own Session /
  SQLite/Postgres) keyed by the runtime `session_id`. The contract shim just uses
  a persistent store instead of in-memory. **DONE** for the OpenAI shim — the
  `runtime_contract` SQLite store persists sessions + an append-only event log
  (replayable via `?since=N`), and the adapter keys an `SQLiteSession` on the
  runtime `session_id` for conversation memory across restarts.
- **Level 2 — in-flight crash resume** (a run that died mid-execution completes
  without losing work). Requires wrapping the foreign run in a durable engine —
  either DBOS-Python *inside* the shim, or a Go "external-kind" `agentd` that
  drives the shim as a `RunAsStep`. Granularity is **whole-run** for opaque-loop
  SDKs (OpenAI/Claude/CrewAI/LangChain) — a crash re-runs the whole agent, so
  tools must be idempotent (at-least-once). Frameworks with their OWN durability
  (LangGraph checkpointers, PydanticAI+DBOS) should keep ownership of it rather
  than be double-wrapped. **Deferred** — Level 1 first.

  - **PydanticAI + DBOS is the standout** for a future deep integration: its
    durable-execution backend can be DBOS — the same engine runtime uses — so a
    PydanticAI agent and a runtime session could align on one Postgres rather than
    nest. Worth its own spec when Level 2 is taken up.

**Platform prerequisite — DONE.** The **generalized spawn path** is in:
`AgentConfig` has optional `command:`/`workdir:` fields, threaded
`config` → `registry` → `AgentProcess` → `SpawnFunc`, so the supervisor execs an
arbitrary argv (e.g. `uv run python serve.py`) with the same `RUNTIME_*` env it
gives `agentd`, instead of only launching the `agentd` binary.

**First milestone — DONE (merged to `master`, 2026-06-08):** Level-1 contract
shim for the OpenAI Agents SDK + the generalized spawn path, proven end-to-end by
hosting the full `examples/nutrition-label-openai` agent (see Status note at the
top of this section). **Next up in C1:** Level 2 (in-flight crash resume); a
second framework adapter (e.g. PydanticAI or CrewAI) to prove the
"one-file-per-framework" reuse claim; and the TS shim for the JS frameworks.

### C2. Containers / Kubernetes

- **Containers / Kubernetes** — once C1 makes foreign agents first-class, package
  them as containers and add Helm charts / an operator for orchestrated scale (the
  "K8s later" half of the deploy decision). The conformance suite already validates
  any binary/container against the contract.

---

## How to resume a piece

Pick an item, then run the standard flow (it's worked well for M1–M3):
1. `brainstorming` skill → settle the open design decisions, write the spec to
   `docs/superpowers/specs/`.
2. `writing-plans` skill → TDD plan to `docs/superpowers/plans/`.
3. `subagent-driven-development` → execute task-by-task with two-stage review,
   on a `feat/<name>` branch.
4. `finishing-a-development-branch` → merge to `master`.

**Conventions that matter** (learned across M1–M3):
- The `go` CLI is ground truth; the IDE/LSP is confused by the
  `replace github.com/sausheong/harness => ../harness` cross-module setup —
  ignore its diagnostics, trust `go build`/`go test`.
- Integration tests are `//go:build integration`, need Postgres at
  `postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable` (local
  Postgres.app, not Docker), and self-clean their DB + the `dbos` schema.
- DBOS v0.16.0 API notes: `docs/superpowers/plans/dbos-v0.16.0-api-notes.md`.
- harness lives at `../harness` (owned; M1 added `RunTurn` to it).
