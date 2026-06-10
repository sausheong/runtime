# Runtime — Roadmap & Backlog

**Checkpoint date:** 2026-06-10 (Gateway M2 — semantic tool search)
**Current state:** Runtime spine complete (Milestones 1–3 merged to `master`);
polyglot agent hosting (C1) first milestone complete — Level-1 OpenAI Agents SDK
shim merged to `master`, hosting a full foreign agent end-to-end (see §C1);
Identity (B3) first three milestones complete — M1 multi-tenant, edge-enforced
access control (OIDC + service keys + per-agent RBAC), M2 per-tenant secrets
brokering (AES-256-GCM provider credentials injected into agents at spawn), and
M3 secrets key rotation (a multi-key keyring with self-describing, AAD-bound
blobs + an explicit re-encrypt command), all merged to `master` (see §B3);
Memory (B2) first three milestones complete — M1 multi-tenant durable
MemoryStore, M2 semantic recall (pgvector embeddings + KnowledgeGraph recall into
the prompt), and M3 auto-ingestion (background LLM fact extraction + semantic
dedup), all merged to `master` — plus the KG→RunTurn wiring that makes recall and
ingest actually fire on the production turn path, and a recall-floor recalibration
for OpenAI-family embeddings (see §B2);
Gateway (B1) first two milestones complete — M1 MCP federation core (a central
`/gateway/mcp` Streamable HTTP endpoint federating static-YAML-configured
upstream MCP servers, tenant-filtered via Identity service keys, consumed by
agents via a `gateway: true` opt-in) and M2 semantic tool search (a search-first
`?mode=search` consumption mode: one listed `search_tools` tool,
embedding-ranked discovery over the federated catalog, callable-but-unlisted
tools), merged to `master` (see §B1).
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
   **First milestone DONE (merged to `master`, 2026-06-10):** MCP federation
   core. A new `internal/gateway` package: a Manager supervises upstream MCP
   servers declared in `runtime.yaml` (`gateway.servers:` — stdio `command:` or
   Streamable HTTP `url:`, both via harness `tools/mcp`), connecting
   asynchronously with capped-backoff reconnect — degrade-don't-fail: startup
   never blocks on upstreams, calls against a down upstream return MCP `isError`
   results, and a mid-flight failure marks down only the observed connection so
   a stale report can't kill a healthy replacement. Upstream tools are
   re-exposed namespaced `<server>__<tool>` on a central `/gateway/mcp`
   Streamable HTTP endpoint serving per-tenant MCP server views behind the
   identity middleware (service-key Bearer; per-upstream `tenants:` allowlist;
   hidden tools are absent from tools/list and tool-not-found on call; sessions
   are principal-bound per call; viewers can list but not call; an unwired
   handler fails 503; open mode is an explicit sentinel). Agents opt in with
   `gateway: true`: the platform injects `RUNTIME_GATEWAY_URL` (+
   `RUNTIME_GATEWAY_KEY` from `gateway.agent_keys`) at spawn — fail-closed at
   startup when identity is on and a tenant key is missing — and agentd appends
   the gateway to `AgentSpec.MCPServers`, so agents see
   `mcp__gateway__<server>__<tool>`; foreign shim agents get the same env, and
   non-opted-in agents get empty-value shadows so an operator env can't leak the
   feature in. `GET /gateway/status` (tenant-scoped, ≥ operator) reports
   per-upstream state. Proven by a through-serve e2e
   (`test/gateway_e2e_test.go`) plus a live smoke against the reference
   filesystem MCP server (stdio via npx: 14 tools federated, an external MCP
   client doing list+call through the gateway, and a gateway-enabled agent turn
   completing with its MCP connects on the access log). Spec/plan:
   `docs/superpowers/{specs,plans}/2026-06-10-gateway-m1-mcp-federation*`.

   **Second milestone DONE (merged to `master`, 2026-06-10):** semantic tool
   search. A search-first consumption mode for the federated catalog: an agent
   sets `gateway: search` in `runtime.yaml` (`GatewayMode` is a string-or-bool
   union, so `gateway: true` keeps its M1 meaning; the platform appends
   `?mode=search` to the injected gateway URL) — or an external MCP client hits
   `/gateway/mcp?mode=search` directly — and tools/list returns exactly one
   tool, `search_tools`, while the principal's full visible catalog stays
   CALLABLE but unlisted (an SDK `AddReceivingMiddleware` list filter over the
   same per-tenant view; the per-view server cache is mode-qualified).
   `search_tools(query, k)` returns JSON matches (name, description, full input
   schema, score) ranked by embedding cosine over an in-memory Index with a
   content-hash vector cache — each distinct tool text embeds once per process;
   lazy, no schema or persistence — with floor `RUNTIME_GATEWAY_SEARCH_FLOOR`
   (default 0.2) and k `RUNTIME_GATEWAY_SEARCH_K` (default 5, cap 20);
   embeddings reuse the Memory `RUNTIME_EMBED_*` config. Posture: fail-fast
   where config is wrong (a search-mode agent without embeddings configured
   refuses startup; `?mode=search` without an Index ⇒ 400; a gateway-enabled
   agent with zero `gateway.servers` is now a config load error),
   degrade-don't-fail where upstreams are (a tool-embed failure skips that tool
   from search but it stays callable; a query-embed failure returns an MCP
   isError "search temporarily unavailable"); viewers may search but not call,
   and principal-bound sessions are preserved across modes. Proven by a
   through-serve e2e (`test/gateway_search_e2e_test.go`, fake embeddings
   server) plus a live smoke against the reference filesystem MCP server over
   the real LiteLLM proxy (`azure/text-embedding-3-small-eastus`):
   `search_tools("read a file's contents")` ranked `fs__read_text_file` top-1
   at cosine 0.586 (next non-read tool 0.396; floor 0.2), and a discovered-tool
   call round-tripped. NOTE: tool-description↔query cosines run HIGHER
   (~0.4–0.6) than the declarative-memory↔question range (~0.25–0.40) on the
   same model — tool descriptions are task-phrased like queries — so the 0.2
   floor is comfortable. Remaining B1: REST/OpenAPI→tool adapters, dynamic
   upstream registration + per-tenant self-service, resources/prompts
   passthrough (tools only today), console panel, auto-minted per-tenant agent
   keys, and rate limits/quotas. Spec/plan:
   `docs/superpowers/{specs,plans}/2026-06-10-gateway-m2-semantic-tool-search*`.
2. **Memory** — managed multi-tenant memory. Short + long term, semantic
   retrieval across sessions, per-tenant isolation. Builds on harness
   `tool/memory` + Postgres/pgvector (pgvector is already provisioned in the
   Compose image, unused so far).
   **First milestone DONE (merged to `master`, 2026-06-09):** multi-tenant
   durable memory. A Postgres backend (`internal/memory`) implements harness's
   `tool/memory.MemoryStore` over an append-only `memory_events` table with a
   SQL live-set projection; agents opt in with `memory: true` in `runtime.yaml`
   and get harness's stock `memory` tool. Per-tenant pool (shared across a
   tenant's agents), isolated by construction (the store is pinned to its tenant;
   the platform injects `RUNTIME_AGENT_TENANT`). Tag/id retrieval only —
   auto-ingestion, compaction/TTL, finer (per-agent/per-user) scoping, and
   per-tenant embedding models remain. Spec/plan:
   `docs/superpowers/{specs,plans}/2026-06-09-memory-m1-pg-memorystore*`.

   **Second milestone DONE (merged to `master`, 2026-06-09):** semantic recall.
   Memory entries are embedded on save into a pgvector `vector(N)` column on
   `memory_events`; harness's `KnowledgeGraph` seam (wired via a new optional
   `agentruntime.Config.KGFn`) embeds each turn's query and injects the nearest
   tenant memories (top-K above a cosine floor) into the prompt — tenant-isolated
   (reuses M1's live-set projection) and best-effort (embed failure ⇒ NULL on
   write / "" on recall, never breaks a turn). Embeddings come from the
   OpenAI-compatible proxy (`RUNTIME_EMBED_MODEL`/`RUNTIME_EMBED_DIM`, reusing
   `OPENAI_*`); unset ⇒ M1 behavior. The pgvector extension must be pre-created by
   a superuser. Spec/plan:
   `docs/superpowers/{specs,plans}/2026-06-09-memory-m2-semantic-recall*`.

   **Third milestone DONE (merged to `master`, 2026-06-09):** auto-ingestion.
   Harness's `KnowledgeGraph.Ingest` (previously a no-op) now captures memories
   automatically — after each chat turn a bounded background goroutine runs an LLM
   extractor (`internal/memory/ingest.go`, OpenAI-compatible `/chat/completions`)
   over the thread, semantically dedups the candidate facts against existing
   memory (reusing M2's `SearchSimilar`), and saves the new ones (embed-on-save ⇒
   recallable next turn). Opt-in via `RUNTIME_INGEST_ENABLED`, layered on semantic
   recall; degrade-don't-fail throughout (extraction/embed/save errors never break
   a turn); growth-gated + inflight-capped (drop, not queue). Auto-captured entries
   carry origin `ingest` + the `auto` tag. Remaining B2: compaction/TTL/GC of dead
   rows, finer (per-agent/per-user) scoping, per-tenant embedding models,
   refinement/merge dedup (Update-on-similar), and session-level synthesis.
   Spec/plan: `docs/superpowers/{specs,plans}/2026-06-09-memory-m3-auto-ingestion*`.

   **Wiring correction (merged with M3):** the M3 final review found harness's
   `RunTurn` (the runtime's sole turn executor) never consulted `r.KG`, so M2
   recall AND M3 ingest were inert on the serve path. Fixed by wiring the KG seam
   into `RunTurn` (bounded-synchronous recall on the first round, ingest on the
   completing round); replay-safe because `RunTurn` runs inside the DBOS step.
   Harness `RunTurn` is owned code, so this was an in-scope change. A through-serve
   integration test (`test/kg_runturn_e2e_test.go`) now guards the path. Spec:
   `docs/superpowers/specs/2026-06-09-kg-runturn-wiring-design.md`.

   **Recall-floor recalibration (2026-06-10):** a live smoke against a real
   LiteLLM proxy (`text-embedding-3-small`) showed ingest working but recall
   returning nothing — the default `RUNTIME_EMBED_RECALL_FLOOR=0.7` was far too
   high for OpenAI-family embeddings, where a question scores only ~0.25–0.40
   cosine against the declarative memory it should recall (unrelated text sits
   near 0). Default lowered to **0.25**, with a per-embedding-model guidance table
   in the README. The ingest dedup floor stays at 0.85 (fact↔fact similarities run
   ~0.74 distinct / ~0.92 near-duplicate, so 0.85 separates them correctly —
   verified by measurement).
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

   **Third milestone DONE (merged to `master`, 2026-06-09):** secrets key
   rotation. The master key is now a keyring (`RUNTIME_SECRETS_KEYS` +
   `RUNTIME_SECRETS_PRIMARY`; the legacy `RUNTIME_SECRETS_KEY` is the back-compat
   single key). Ciphertext blobs are self-describing (versioned `0x01` prefix +
   embedded key id) and AAD-bound to `(tenant, name)` to defeat DB row swaps. An
   explicit, idempotent `runtimectl admin secret rotate` re-encrypts a tenant
   (superuser: all tenants) under the primary so retired keys can be dropped.
   Legacy M2 rows decrypt transparently until rotated. Spec/plan:
   `docs/superpowers/{specs,plans}/2026-06-09-identity-m3-secrets-key-rotation*`.

   **Remaining Identity work:** per-tenant keys, fine-grained/custom RBAC beyond
   the 3 roles, cross-tenant users + user self-service, an admin console UI (incl.
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
