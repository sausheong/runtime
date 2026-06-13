# Runtime — Roadmap & Backlog

**Checkpoint date:** 2026-06-13 (Spine A2 — autoscaling)
**Current state:** Runtime spine complete (Milestones 1–3 merged to `master`);
polyglot agent hosting (C1) first two milestones complete — two foreign
frameworks (OpenAI Agents SDK + Claude Agent SDK) hosted via the one Python
contract shim, each running the full SG Nutrition Investigator end-to-end,
merged to `master` (see §C1);
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
Gateway (B1) first three milestones complete — M1 MCP federation core (a central
`/gateway/mcp` Streamable HTTP endpoint federating static-YAML-configured
upstream MCP servers, tenant-filtered via Identity service keys, consumed by
agents via a `gateway: true` opt-in), M2 semantic tool search (a search-first
`?mode=search` consumption mode: one listed `search_tools` tool,
embedding-ranked discovery over the federated catalog, callable-but-unlisted
tools), and M3 REST/OpenAPI→tool adapters (`openapi:` as a third upstream
transport — one generated, tenant-filtered, searchable gateway tool per
selected spec operation, no MCP server required), all merged to `master`
(see §B1);
Sandboxes (B4) first milestone complete — M1 code interpreter (a `cmd/sandboxd`
MCP server giving every gateway-enabled agent an isolated, stateful,
Docker-backed Python+shell sandbox with tenant-scoped ownership), merged to
`master` (see §B4);
Observability (B5) first milestone complete — M1 fleet-wide Prometheus metrics
(control-plane + per-agent series merged behind one auth-free `/metrics` via a
hardened fan-out scrape), `X-Request-ID` correlation end-to-end, and a
Prometheus + Grafana compose overlay with a provisioned dashboard (see §B5).
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

1. **Subprocess pools / replicas per agent — DONE (2026-06-13).** A local agent
   runs `replicas: N` supervised `agentd` processes behind one `/agents/{id}`
   route. New sessions round-robin across the pool; each session pins to its
   owner replica for life via a persisted `sessions.replica` column, because only
   the owning DBOS executor can resume that session's durable workflow (and it
   holds the live SSE subscriber set). Each replica runs as a stable executor
   `DBOS__VMID=<id>#<i>`, so a supervisor restart at the same index recovers
   exactly that replica's in-flight work (M1 durability, per replica) with no
   double execution. `listen_addr` is the base (replica i ⇒ base_port+i; derived
   ports validated unique + in range); `replicas:1`/omitted is byte-for-byte the
   old behavior for routing/SSE/health — but note the DBOS executor id changes
   from `local` to `<id>#0` (see upgrade-in-place migration below); `replicas` is
   rejected on remote agents. Per-replica supervision,
   any-replica-healthy `/agents`, and per-replica metrics (a `replica` label on
   `agent_up`/`agent_reachable`/`agent_restarts`/`scrape_skips` and on the
   agent-exposed series). Owner-down ⇒ 503 until restart; round-robin is blind to
   liveness (skip-down deferred). Unblocks A2 (autoscaling) and C2 M2 (per-agent-pod
   scheduling). Tested: config/registry/routing/store/metrics unit tests + an
   integration test (`TestReplicaPoolsAffinity`: distribution, affinity, kill-one-
   replica durability with no double execution). Spec/plan:
   `docs/superpowers/{specs,plans}/2026-06-13-spine-a1-replica-pools*`. Remaining
   spine items 2-6 below unchanged.
   **Upgrade-in-place migration:** a pre-A1 deployment stamped its workflows with
   the default executor id `local`; A1 processes use `<id>#<i>`, so pre-existing
   in-flight workflows are not auto-recovered after upgrade. Drain in-flight
   sessions before upgrading, or run a one-time `UPDATE dbos.workflow_status SET
   executor_id = '<agentid>#0' WHERE executor_id = 'local'` per single-replica
   agent. Fresh deploys need nothing.
2. **Autoscaling — DONE (2026-06-13).** runtimed floats a local agent's replica
   pool with load: it is **both controller and actuator**, growing the pool by
   spawning an `agentd` replica and shrinking it by draining-then-stopping the
   highest replica — all on one host. Opt-in per local agent via an
   `autoscale: {min, max, target_sessions_per_replica}` block; when absent the
   agent keeps the static A1 `replicas:` pool byte-for-byte, and `autoscale` is
   rejected on remote (`url:`) agents. A `PoolManager` owns the mutable replica
   set plus the policy loop: the scale signal is **active (non-terminal) sessions
   per replica**, `desired = clamp(ceil(active/target), min, max)`, and each poll
   tick takes **at most one step** toward it. Mutation is **suffix-only** (append
   at the top index or remove the highest) and scale-down is **drain-only** — the
   top replica is marked draining (new sessions stop routing there) and stopped
   only at 0 active sessions, with **no force-kill/deadline**, so a single
   long-lived session blocks *that one* scale-down indefinitely by design;
   durability stays absolute. An **un-drain fast path** clears the drain flag if
   load rebounds while the top is draining. **Asymmetric cooldowns** (up=10s,
   down=30s; poll=5s) scale up eagerly and down cautiously, all tunable via
   `RUNTIME_AUTOSCALE_POLL_SECONDS` / `_UP_COOLDOWN_SECONDS` /
   `_DOWN_COOLDOWN_SECONDS`. `listen_addr` is the base (replica i ⇒ base_port+i)
   and the whole `max` port range is reserved + collision/overflow-validated at
   config load. **Degrade-don't-fail boot:** if a pool can't reach `min` at
   startup runtimed warns and the loop retries toward `min` — it never
   `os.Exit`s. Metrics: `runtime_agent_replicas_desired{agent}`,
   `runtime_agent_replicas_current{agent}`,
   `runtime_agent_active_sessions{agent}` (gauges) and
   `runtime_autoscale_events_total{agent,action}`
   (`action ∈ up|down|undrain|reap|blocked`). Suffix-only + drain-only means a
   session is never reassigned to another executor, so this preserves the A1
   **executor-id invariant** (`DBOS__VMID=<id>#<i>`) and the `session_events`
   single-writer invariant (item 4) **by construction**. Tested: unit tests
   (config/store/obs/PoolManager set-ops + pure `decideStep`) plus an integration
   test `TestAutoscaleGrowDrain` (grow to 3 under load, the static `replicas:2`
   path stays exactly 2, no double execution, then drain back to 1). Spec/plan:
   `docs/superpowers/{specs,plans}/2026-06-13-spine-a2-autoscaling*`.
   **Deferred:** scale-down force-kill deadline, richer scale signals
   (CPU/queue/latency), per-agent cooldown config, and a **signal-only mode**
   (emit the desired count and let an external orchestrator actuate — the seam
   toward C2 M2 per-agent-pod scheduling).
3. **Dynamic deploy** — `POST /agents` runtime registration + rollback; today
   agents come from `runtime.yaml` at startup. Tokens are config-only too.
4. **`session_events` concurrency** — `SELECT MAX(seq)+1` is safe only because
   one subprocess owns a session (one writer). Revisit (lock / sequence /
   `ON CONFLICT` retry) if a session ever gets concurrent writers (e.g. pools).
   Note: A2 autoscaling preserves this single-writer invariant **by
   construction** — suffix-only mutation + drain-only scale-down means a session
   is never reassigned to another executor, so its owning replica stays the sole
   writer for life.
5. **DBOS recovery across a recompiled binary** — recovery keys on the agentd
   binary's app-version hash; recovering a workflow across a code change needs
   `DBOS__APPVERSION` pinned. Document/operationalize if doing rolling upgrades.
6. **Cross-agent aggregate session view** — session listing is per-agent; a
   fleet-wide view (and richer console health) is future console work.
7. **Constant-time token compare + token hashing-at-rest** — M3 uses a plain map
   lookup; fine for static config tokens. Folds naturally into Identity (B3).
8. **Access-log already wired** (M3) — metrics + request-id correlation landed
   with Observability M1; tracing still pending (see B5).

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

   **Third milestone DONE (merged to `master`, 2026-06-12):** REST/OpenAPI→tool
   adapters. `openapi:` is a third gateway transport (exactly-one-of
   `command`/`url`/`openapi`; optional `base_url:` override and `operations:`
   allowlist of operationIds or `METHOD /glob` patterns; `${VAR}` header
   expansion reused; `forward_tenant` stays stdio-only): `dialOpenAPI`
   (`internal/gateway/openapi.go`) fetches the spec (file or URL, configured
   headers, 8 MiB cap), parses it with kin-openapi, and generates one
   `tool.Tool` per selected operation — name `<server>__<operationId>`
   (method_path slug fallback, `__` collapsed to `_`, post-sanitization
   collisions skip-with-WARN), description `"METHOD /path — summary"` (1024
   cap), input schema merging path(required)/query/`header_`-prefixed/`body`
   properties with op-level parameter overrides winning. `restConn` implements
   `upstreamConn` so supervision, tenant views, principal binding, M2 search
   indexing, and Obs-M1 metrics (`runtime_gateway_tool_calls_total`,
   `upstream_up`, transport `"openapi"` in `/gateway/status`) all apply
   unchanged; Ping is HEAD→GET-on-405 with ANY HTTP response = alive, and
   reconnect re-fetches the spec (drift heals on redial). Execution returns a
   JSON envelope `{status, headers:{content-type}, body, truncated}` — 4xx/5xx
   are results the agent reasons about, not tool errors; 30s timeout, 1 MiB
   response cap with truncated flag; traversal guard on path params
   (`/`, `..`, encoded forms rejected); config headers inviolable
   (case-insensitive, including undeclared `header_*` args and Content-Type);
   GET/HEAD marked concurrency-safe. Two review-caught fixes worth recording:
   (1) the original `$ref` handling string-matched the marshaled schema for
   `"$ref"` and skipped any operation containing one — which would have zeroed
   every real-world spec (component reuse is the norm); replaced by a
   deep-inline walk (`inlineSchema`) emitting plain JSON Schema, where only
   genuine cycles — ancestor-path repetition, not sibling reuse — skip the
   operation with WARN (external cross-file `$ref`s fail at dial: security
   posture); (2) the same-host-only redirect policy initially covered API
   calls but not the spec fetch, which followed cross-host redirects with
   configured auth headers attached — a credential leak; the exact-same-host
   policy now applies to both. Degrade-don't-fail throughout: unfetchable spec
   = down upstream with backoff re-fetch, zero-match filter = connected with 0
   tools + WARN, unmappable operation = skip-with-WARN, required non-JSON body
   = skip (optional non-JSON body drops `body`), >50 generated tools WARNs
   toward `operations:`. Proven by a through-serve e2e
   (`test/gateway_rest_e2e_test.go`: identity enforced, spec fetched over
   HTTP, generated tools listed+called via `/gateway/mcp`, tenant-hidden from
   the other tenant, metrics/status carry the openapi transport) plus
   `examples/rest-demo` (a stdlib orders API serving its own spec).
   Limitations recorded: JSON request bodies only, comma-joined arrays (no
   explode), shared credentials per upstream, OpenAPI 3.x only, no OAuth2
   flow. LIVE PROOF (2026-06-12, all passed): the bundled
   `examples/rest-demo` orders API on :9000 federated as upstream `orders`
   (transport=openapi, 3 tools) — an external MCP client listed
   `orders__listOrders`/`getOrder`/`createOrder` and called `listOrders`
   with `{"status":"open"}` through `/gateway/mcp`, envelope status 200
   with both open orders returned; a real-world spec we didn't write —
   Open-Meteo's 1000+-line `forecast.yml` fetched from
   raw.githubusercontent.com, `servers[]` absent so the configured
   `base_url` was used — federated as `weather__get_v1_forecast` (1 tool)
   and called through the gateway with
   `current=["temperature_2m","weather_code"]` (the enum-array param fully
   inlined into the tool schema): status 200, a live Singapore reading of
   25.3°C returned through the envelope; an end-to-end agent turn — the
   nutrition agent (`gateway: true`, real LLM via the proxy) used the
   federated weather tool to fetch the current temperature at lat 1.35/lon
   103.82 and folded 25.3°C into its hydration/sugar advice, the REST tool
   call visible in the turn; and `gateway: search` discovery —
   `/gateway/mcp?mode=search` with a `search_tools` query "get the weather
   forecast for a location" returned `weather__get_v1_forecast` ranked
   with its generated description + full inlined schema (embeddings via
   the proxy: `RUNTIME_EMBED_MODEL=azure/text-embedding-3-small-eastus` —
   NOTE the proxy serves embedding models under prefixed names, not bare
   `text-embedding-3-small`). Remaining B1: dynamic upstream registration +
   per-tenant self-service, resources/prompts passthrough, OAuth2 upstream auth,
   per-tenant upstream credentials (secrets-broker integration), console
   panel, auto-minted per-tenant agent keys, and rate limits/quotas.
   Spec/plan: `docs/superpowers/{specs,plans}/2026-06-12-gateway-m3-rest-adapters*`.
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
   **First milestone DONE (merged to `master`, 2026-06-11):** code interpreter
   behind the gateway. A new `cmd/sandboxd` binary (package `internal/sandbox`)
   is an MCP stdio server federated as an ordinary gateway upstream — agents
   opt in with the existing `gateway: true`/`search` and see
   `mcp__gateway__sandbox__<tool>` with zero agent-side changes. Seven tools
   (create_sandbox, execute_code, run_command, write_file, read_file,
   list_sandboxes, close_sandbox) over one locked-down Docker container per
   stateful session: `network=none` always, read-only rootfs, tmpfs
   `/workspace` (files persist across calls; Python variables don't — each
   exec is a fresh process, kernel-mode persistence is the designated M2
   upgrade), CapDrop ALL, no-new-privileges, non-root uid 1000, CPU/mem/pids
   limits, optional gVisor via `RUNTIME_SANDBOX_RUNTIME=runsc`. Exec wraps
   argv in coreutils `timeout` (clamped 30s default/120s max) so a runaway
   process dies without killing the session; idle-TTL (10m) + max-lifetime
   (1h) reaper plus reap-on-start (label `runtime.sandbox=1`) bound runaway
   sessions; per-tenant cap (5) with slot reservation under lock. Tenancy
   rides the milestone's only gateway change: `forward_tenant: true` on a
   stdio upstream makes the gateway strip any caller-supplied `__rt_tenant`
   argument and inject the authenticated principal's tenant (spoof-proof;
   `__` is now reserved in gateway server names to keep the name→upstream
   lookup sound); sandboxd fails closed when the key is absent
   (`RUNTIME_SANDBOX_ALLOW_DIRECT=1` opts out for single-tenant direct use)
   and hides cross-tenant existence (foreign id ⇒ same "no such sandbox" as a
   missing id). Bundled image `deploy/sandbox.Dockerfile` (`make
   sandbox-image`: python:3.12-slim + numpy/pandas/matplotlib/requests —
   requests included deliberately so network-isolation failures are
   meaningful). NOTEWORTHY FINDINGS (final review + live proof): (1) the
   Docker archive API is unusable under the spec's own posture — the daemon
   rejects CopyToContainer on a read-only rootfs and CopyFromContainer can't
   see tmpfs contents — so file I/O is exec-based (`dd of=` stdin /
   `head -c`), argv-only, never a shell string; (2) the exec-stdin plumbing
   initially had a structural backpressure deadlock (stdin written before the
   output drainer started — invisible on Docker Desktop's large vsock
   buffers, a permanent hang on Linux unix sockets past ~1 MiB), fixed by
   drainer-first ordering and pinned by an 8 MiB live-gated regression test
   (`go test -tags live`). Proven by hermetic unit tests (fake backend), a
   through-serve e2e with identity enforced (two tenants; spoofed
   `__rt_tenant` overridden; cross-tenant invisible —
   `test/gateway_sandbox_e2e_test.go`), live-gated Docker tests, and a live
   smoke on real Docker: CSV → pandas → result round-trip, `requests.get`
   blocked, 5s timeout kill with session surviving, 10s-TTL reaper observed
   removing the container, and an end-to-end agent turn (real LLM via the
   proxy) where the nutrition agent used `sandbox__execute_code` to compute
   sugar-per-can (43.875g, 87.75% of the WHO limit) inside its verdict.
   **Second milestone DONE (merged to `master`, 2026-06-12):** browser sandbox
   behind the gateway. A new `cmd/browserd` binary (package `internal/browser`)
   is a sibling to sandboxd, federated as an ordinary `forward_tenant` gateway
   upstream — agents opt in with the existing `gateway: true`/`search` and see
   `mcp__gateway__browser__<tool>` with zero agent-side changes. Ten tools
   (create_browser, navigate, click, type, get_text, extract, screenshot —
   image content riding the gateway's existing image-content passthrough —,
   evaluate, list_browsers, close_browser) drive a Chromium per stateful
   session running in a locked-down Docker container (read-only rootfs, CapDrop
   ALL, no-new-privileges, non-root uid 1000, CPU/mem/pids limits, optional
   gVisor via `RUNTIME_BROWSER_RUNTIME=runsc`) over remote CDP via
   `chromedp.NewRemoteAllocator`. It reuses M1's Manager contract almost
   verbatim — per-tenant cap with slot reservation under lock, idle-TTL (10m) +
   max-lifetime (1h) reaper, reap-on-start by label `runtime.browser=1`,
   existence-hiding lookup (foreign id ⇒ same "no such browser" as a missing
   id) — and the same `forward_tenant` spoof-proofing (caller-supplied
   `__rt_tenant` stripped and overridden by the authenticated principal;
   fails closed when the key is absent). The headline feature is **network
   egress policy**: Chrome's entire network stack is forced through a
   browserd-run HTTP/HTTPS proxy via `--proxy-server` (the agent can only drive
   Chrome over CDP, so the proxy adjudicates all reachable traffic — subresources,
   fetch, redirects, CONNECT), which allows or denies by hostname in three modes
   (`RUNTIME_BROWSER_EGRESS_MODE` = deny-all default, allow-list of hostname globs
   via `RUNTIME_BROWSER_EGRESS_ALLOW`, allow-all-public); the container is on a
   bridge network and a network-level egress boundary so even a
   non-proxy-respecting process is contained is recorded as follow-on hardening.
   Internal/private addresses are blocked unconditionally across all modes with
   DNS-rebind defense (resolve-then-check) and a fail-closed default; because the
   proxy sees subresources, fetch, redirects, and CONNECT — not just the top-level
   URL — it beats DNS/iptables filtering.
   The chromedp action logic is ported (not imported) from harness, alongside
   the stealth script and the SSRF private-network set. Proven by hermetic unit
   tests (egress policy table incl. DNS-rebind + IPv4-mapped IPv6, proxy
   forward/CONNECT allow-deny, Manager lifecycle mirroring M1, extract, tool
   server with in-memory transport incl. absent-tenant fail-closed), a
   live-gated real-Chrome test (`internal/browser/docker_live_test.go`:
   container browse of an allow-listed local server + egress block of a
   non-allowlisted host + real screenshot), and a through-serve e2e with
   identity enforced (`test/gateway_browser_e2e_test.go`: two tenants, spoofed
   `__rt_tenant` overridden, cross-tenant browser invisible). Build:
   `make browser-image`. Live proof (real Docker + Chromium, 2026-06-12): the
   live-gated `TestLiveBrowseAndEgress` drove a real container to browse
   allow-listed `example.com` (extracted "Example Domain") while every
   non-allowlisted host — `www.iana.org` plus Chrome's own background telemetry
   to `accounts.google.com`/`clients2.google.com`/etc. — was denied by the
   proxy; and an end-to-end agent turn (real LLM via the proxy, `gateway: true`
   browser agent over a Docker-backed `browserd` upstream) created a browser,
   navigated to `example.com` through the gateway, and returned the heading
   "Example Domain" verbatim, while a second turn to non-allowlisted
   `www.wikipedia.org` came back `ERR_TUNNEL_CONNECTION_FAILED` (the proxy's
   CONNECT refusal) — `runtime_gateway_tool_calls_total{server="browser"}`
   recorded `browser__navigate` ok=1/error=1, `create_browser`=2, `extract`=1,
   `close_browser`=2. Two live bugs the hermetic suite could not see were
   caught and fixed: Chromium ignores `--remote-debugging-address` and binds
   CDP to container-loopback only (fixed with an in-image socat bridge from the
   published port to `127.0.0.1`), and a dual-stack `0.0.0.0:0` proxy listener
   reports `[::]:port` which the container-proxy-address rewrite must map to
   `host.docker.internal` (fixed + regression-tested). Spec/plan:
   `docs/superpowers/{specs,plans}/2026-06-12-sandboxes-m2-browser*`.
   Remaining B4: kernel-mode variable persistence, pip-install, per-user
   scoping, console panel, instance-scoped reap labels (today: exactly one
   sandboxd/browserd per host/DOCKER_HOST). Spec/plan:
   `docs/superpowers/{specs,plans}/2026-06-10-sandboxes-m1-code-interpreter*`.
5. **Observability** — tracing, metrics, dashboards. The structured `slog` from
   M3 is the lightweight precursor; this is the full version (OpenTelemetry +
   Grafana, or similar). (Absorbs A8.)
   **First milestone DONE (2026-06-11):** fleet metrics + request correlation.
   A new `internal/obs` package owns every Prometheus metric in the module
   (sole importer of client_golang; all helpers nil-receiver-safe no-ops, so
   instrumented code never nil-checks). Control-plane registry:
   `runtime_http_requests_total{route,method,status}` +
   `runtime_http_request_duration_seconds` (matched mux patterns only, never
   raw paths — cardinality-safe by construction; identity rejections counted
   under `route="auth_rejected"`), `runtime_agent_up`,
   `runtime_agent_restarts_total`, `runtime_proxy_errors_total`
   (client-initiated cancellations excluded), gateway series
   (`runtime_gateway_tool_calls_total{server,tool,outcome}` — only calls
   reaching the upstream; authz rejections not counted — plus duration
   histogram and `runtime_gateway_upstream_up`), and
   `runtime_metrics_scrape_skips_total{agent,reason}`. Agent registry (per
   agentd): `agent_turns_total{agent,outcome}`,
   `agent_turn_duration_seconds` (LLM-sized buckets to 120s),
   `agent_tokens_total{agent,direction}` (input/output/cache_creation/
   cache_read), `agent_tool_calls_total{agent,tool}`. runtimed's
   `GET /metrics` is auth-free (like `/healthz` — every label value is an
   operator-level identifier; the cardinality promise: NO tenant/session/user
   labels, adding one is a spec change) and FANS OUT: concurrent sub-scrapes
   of every supervised agent's `/metrics` (500ms cap), expfmt parse, merge by
   family name, re-encode one valid exposition. Agent `/metrics` is OPTIONAL
   — a foreign shim's 404 is skipped as `no_metrics` (DEBUG, not an error)
   and does NOT mark the agent down. `X-Request-ID` is accepted at the edge
   ([A-Za-z0-9._-], ≤128; else regenerated `req-<32hex>`), echoed, forwarded
   through the reverse proxy, present in slog on both sides (access log +
   per-turn lines + failure warnings), and checkpointed in the DBOS workflow
   input (replay-safe); `runtimectl invoke -v` prints it.
   `deploy/docker-compose.obs.yml` overlays Prometheus (:9090) + Grafana
   (:3000, anonymous viewer) with a provisioned 12-panel "Runtime Overview"
   dashboard. KEY REVIEW FINDINGS worth recording: (1) fan-out merge
   hardening — the review found one lying agent could kill the whole scrape
   (and that agents could label-spoof each other), so the merge now enforces
   server-side `agent` labels (the registered target id overwrites whatever
   the agent claimed — series disjoint by construction), drops agent families
   colliding with control families or any `runtime_*` name
   (`reserved_name` — the control plane owns that namespace), drops
   cross-agent TYPE conflicts (`type_conflict`), and encodes each family into
   a buffer first so one bad family is skipped instead of truncating the
   response mid-stream; (2) auth-rejected visibility — requests rejected by
   the identity middleware never reached the instrumented handler and were
   invisible, fixed by an onReject hook recording them under
   `route="auth_rejected"`. Proven by hermetic unit tests
   (`internal/obs/*_test.go`) and a through-serve e2e
   (`test/observability_e2e_test.go`): fan-out merge, route normalization,
   request-id echo, auth-free `/metrics` with identity on. LIVE PROOF
   (2026-06-11, all passed): the compose overlay up (`docker compose -f
   deploy/docker-compose.yml -f deploy/docker-compose.obs.yml up -d`) with
   the Prometheus target `host.docker.internal:8080` health "up", Grafana
   13.0.2 healthy and serving the provisioned "Runtime Overview" dashboard
   with all 12 panels, and PromQL through Prometheus returning real
   per-agent turn counts (`sum by (agent)(agent_turns_total)`: support=8,
   research=2); fan-out + merged exposition live — per-agent
   `agent_turns_total`/`agent_tokens_total` series with correct agent
   labels flowing through runtimed's single `/metrics`; a `kill -9` of the
   support agentd, where the next scrape showed
   `runtime_agent_up{agent="support"} 0` (research stayed 1) and after
   supervisor recovery (~6s) up was back to 1 with
   `runtime_agent_restarts_total{agent="support"} 1`; and request-id
   correlation — `runtimectl invoke -v` printed
   `req-3c31f600efcd5b8b43aaeb94ffaeb53d`, and ONE grep of that id hit 4
   log lines spanning both processes (runtimed access log: POST /sessions
   status=200; agentd http log: POST /sessions; and both turn lines:
   turn=0 reason=continue, turn=1 reason=completed with the session id) —
   the edge→proxy→agent→turn chain proven. Remaining B5: OTel
   tracing/OTLP push (request ids are the seed), sandboxd-internal metrics
   (visible today only as gateway series), per-tenant token accounting,
   alerting/recording rules, console `/ui` metrics panel, log shipping, and
   DBOS-internal metrics. Spec/plan:
   `docs/superpowers/{specs,plans}/2026-06-11-observability-m1-metrics*`.
   **Second milestone DONE (2026-06-13):** OpenTelemetry distributed tracing.
   A new `internal/obs/tracing.go` is the single owner of the tracer setup:
   `InitTracing` is the no-op gate — off by default, with no OTLP endpoint it
   installs a no-op provider for zero overhead (env `OTEL_EXPORTER_OTLP_ENDPOINT`
   presence enables; `RUNTIME_TRACING_ENABLED` is an explicit 1/0 override;
   `RUNTIME_TRACE_SAMPLE_RATIO` 0.0–1.0 default 1.0 drives a parent-based + ratio
   sampler), with a W3C TraceContext + Baggage propagator and `StartSpan` plus
   attribute builders that enforce IDs-only/no-content. Instrumentation lives at
   three otelhttp seams (runtimed edge server span, the reverse-proxy transport
   injecting `traceparent`, and the agentd inbound server span continuing the
   parent) plus in-process spans `session.workflow`/`agent.turn`/`tool.call`
   (live-execution only, created inside the DBOS `RunAsStep` closure and NOT
   checkpointed — replay-safe) and `gateway.upstream`. THE HONEST TRACE SHAPE:
   the synchronous HTTP path (edge → reverse-proxy → agentd handler) is ONE
   trace via `traceparent`, but the durable session workflow is a SEPARATE,
   correlated trace joined by the `request.id` span attribute — because the
   workflow is launched on the long-lived dbos context, not the inbound request
   ctx (a durable workflow outlives its request; inherent to durable async).
   `InitTracing` is wired into both binaries (runtimed `main`, agentd
   `Serve`, flushing on shutdown after the HTTP drain). The obs compose overlay
   adds an OTel Collector (OTLP/HTTP :4318) + Jaeger (UI :16686); host-run
   binaries export to `http://localhost:4318`. Deferred to a later milestone:
   sandboxd internals, content attributes, and live-wrapped tool/LLM spans.
   Spec/plan:
   `docs/superpowers/{specs,plans}/2026-06-13-observability-m2-tracing*`.

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

**Remaining C1 work:** the second framework adapter is DONE (Claude Agent SDK —
see the second-milestone entry below). Remaining: Level 2 (in-flight crash
resume), the TS shim, a PydanticAI adapter (M3 candidate), and reconciling the
follow-up-messages endpoint into the Go contract + conformance suite (or
spec'ing it as optional) — the Python shim now serves `POST
/sessions/{id}/messages` (added in M2 for multi-turn-on-one-session) but the Go
`agentruntime` contract does not.

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
a `serve()`-style entry point that reads the operator env), then a thin
per-framework adapter maps that framework's run/stream API to the contract's
`text`/`tool_result`/`done`/`error` events — measured at ~40–100 code lines in
practice (OpenAI SDK adapter: 39 code / 67 total; Claude SDK adapter: 100 code /
139 total — framework friction, not deployment glue, sets the size). The
adapter author writes only the adapter, never deployment glue. One Python shim
then covers OpenAI SDK, PydanticAI, CrewAI, LangGraph, LangChain, ADK; one TS
shim covers the JS frameworks.

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
top of this section).

**Second milestone DONE (merged to `master`, 2026-06-10):** Claude Agent SDK
adapter. The second framework adapter: the Claude Agent SDK (Python, pinned
0.2.95) hosts the SG Nutrition Investigator — its THIRD implementation
(Go/harness, OpenAI SDK, Claude SDK) — at full parity through the same
`runtime_contract` shim: 5 in-process MCP tools (the 4 investigator tools +
`submit_verdict` as the typed-output channel replacing the SDK's `output_type`),
vision verdict on `milo.jpeg` live through the control-plane proxy, learned
aliases, and Level-1 resume across a full platform restart via the SDK-native
`resume=` (transcripts live under a pinned `CLAUDE_CONFIG_DIR`/cwd) plus a
runtime→SDK session-id map in the shim SQLite. Security posture: built-ins
disabled via `tools=[]` (primary) + a `disallowed_tools` deny-list (backup) +
`permission_mode` dontAsk — the agent has ONLY the nutrition tools. Proxy
wiring: `ANTHROPIC_BASE_URL` → LiteLLM with namespaced model ids
(`claude-sonnet-4-6-asia-southeast1`); `spike_vision.py` stays in-repo as living
documentation of the proven shapes. Live proof: `runtimectl conformance` PASSED
(6 checks, via the `--agent` flag + `RUNTIME_CTL_URL`); a FizzPop text verdict
with the E211+ascorbic-acid benzene interaction correctly connected; the MILO
vision verdict; a restart-resume follow-up correctly recalling the Milo verdict;
and the alias blorbium→E211 learned in one session, resolved hint-free in
another. HONEST MEASUREMENTS — the milestone's purpose: (1) the adapter is **139
total / 100 code lines**, NOT this section's former "~10-30 line" claim (the
OpenAI adapter is 67/39); the CONTRACT seam held — one file drives the
framework, no deployment glue — but "thin" is relative to framework friction:
the Claude SDK needed the session-id map, builtin stripping, the tool-as-output
pattern, and the streaming-input image form. The seam paragraph above now reads
~40–100 lines. (2) **The shim did NOT survive unchanged**: commit `613f266`
added `POST /sessions/{id}/messages` to `runtime_contract/app.py` because the v1
contract had NO follow-up-message endpoint — `POST /sessions` always creates a
new session, so multi-turn-on-one-session (the Level-1 resume proof) was
impossible as specced. The addition is framework-agnostic and additive
(conformance unaffected; benefits all adapters) — but the second-order finding
is a CONTRACT DIVERGENCE: the Python shim now implements a 7th endpoint that the
Go `agentruntime` contract does NOT have (verified: `agentruntime/server.go` has
no `/messages` route; Go sessions are single-turn workflows). New backlog
(listed in "Remaining C1 work" above): reconcile the follow-up-messages endpoint
into the Go contract + conformance suite (or spec it as optional). (3) SDK
quirks worth recording for the TS shim: a bundled CLI subprocess per session (no
Node needed); resume is cwd+`CLAUDE_CONFIG_DIR`-keyed; `allowed_tools` is NOT a
restriction (only auto-approval); and `options.env` merges (Python). Code:
`examples/nutrition-label-claude/`. Spec/plan:
`docs/superpowers/{specs,plans}/2026-06-10-claude-agent-sdk-adapter*`.

### C2. Containers / Kubernetes

**First milestone DONE (merged to `master`, 2026-06-13):** container image + Helm
chart. The monolith-pod packaging, faithful to the current single-node supervisor
(runtimed `exec`-spawns agentd children in its own process tree — NOT decomposed
services). Two artifacts: (1) a **single all-binaries image** (`deploy/Dockerfile`
extended to build + ship `runtimed`+`agentd`+`sandboxd`+`browserd`+`runtimectl`),
non-root uid 10001, OCI labels, `make docker-image` (build context is the PARENT of
`runtime/`+`harness/` per the `replace` directive); (2) a **Helm chart** at
`deploy/charts/runtime/` — Deployment (hard-pinned `replicas: 1`, `Recreate`
strategy: a single-writer DBOS supervisor must never double against one Postgres),
Service, ConfigMap (`runtime.yaml`, with a `checksum/config` pod annotation that
auto-rolls on `helm upgrade`), Secret (only set keys; suppressed when
`existingSecret` is used, with `optional:true` refs), ServiceAccount, and toggle-
gated Ingress / NetworkPolicy / obs (ServiceMonitor + a `grafana_dashboard`-labeled
ConfigMap packaging the obs-M1 dashboard). Postgres is an optional Bitnami subchart
(`postgresql.enabled`); secure-by-default pod posture (runAsNonRoot, fsGroup,
`readOnlyRootFilesystem` with only a `/tmp` emptyDir — verified: agentd does no disk
writes, DBOS is Postgres-backed); two fail-closed render guards (no DSN source; no
agents). Make targets `helm-lint`/`helm-template`/`helm-deps`/`helm-package`; no CI
(build-locally, manual push). Docker-dependent sandbox/browser ship in the image but
are OFF by default (a plain pod has no Docker daemon), surfaced via a `DOCKER_HOST`
knob + a documented `extraContainers` DinD opt-in (privileged sidecar, single-node
only). Per-agent-pod scheduling landed in C2 M2 (below); an operator/CRDs to a
later C2 milestone. THE FINAL HOLISTIC REVIEW (pre-live-proof) EARNED ITS KEEP
AGAIN — it caught FOUR integration bugs invisible to per-task render checks, each an
independent live-install failure: (1) the bundled Postgres image 404'd — Bitnami's
2025 catalog migration moved the pinned `docker.io/bitnami/postgresql:<x>-rN` tags
to `bitnamilegacy/postgresql` (verified: bitnami 404, bitnamilegacy 200); re-pointed
the subchart image. (2) the synthesized DSN host used `runtime.fullname-postgresql`
but the Bitnami subchart names its Service `<release>-postgresql` — a mismatch on
ANY release not literally named "runtime" ⇒ DNS failure ⇒ CrashLoop, and the test
harness masked it by using release "r"; fixed to derive the host from `.Release.Name`
+ added a regression guard. (3) the default `config.agents: []` rendered fine but
runtimed fatal-exits on a zero-agent registry ⇒ CrashLoop; added a `requireAgents`
render guard + fail-closed test. (4) the README quick-start sample used a nonexistent
`script:` field and omitted required `id`/`listen_addr`; rewritten to the real schema.
Plus an IMPORTANT foot-gun: the chart's default `image.tag` resolves to appVersion
`0.1.0` which `make docker-image` never built — now also tagged. LIVE PROOF (real
kind cluster + bundled Postgres, 2026-06-13, all passed): `make docker-image` →
`kind load` → `helm install` with `postgresql.enabled=true` and two scripted agents
→ the pod reached **1/1 Running** (3 self-healed restarts during the initial
DB-not-ready race — the readiness gate + Recreate doing their job), runtimed
connected to `runtime-postgresql:5432` (DSN-host-matches-Service proven live),
launched DBOS for both agents, and served `/healthz` 200; **`runtimectl conformance`
PASSED all 6 checks** against the in-cluster Service (create session + stream + get +
list — the exec-spawn supervisor model working inside a pod, end to end); a
`helm upgrade` adding a third agent flipped the `checksum/config` annotation
(`41f277bd…`→`0a27ce92…`), rolled a new ReplicaSet to 1/1, and `/agents` then
reported all THREE agents healthy; clean `helm uninstall` + `kind delete`. Hermetic
gate green (7-permutation `test.sh`, `go build`/`go vet`). Remaining C2: a Kubernetes operator/CRDs, multi-arch image
publish + CI, a pgvector-capable bundled Postgres (the Bitnami image lacks the
extension, so bundled-PG can't do semantic memory), and HPA/autoscaling (blocked on
the single-replica supervisor model). Spec/plan:
`docs/superpowers/{specs,plans}/2026-06-12-c2-packaging*`.

   **Second milestone DONE (merged to `master`, 2026-06-13):** per-agent-pod
   scheduling. A `scheduling.mode: monolith | perAgentPods` chart toggle. In
   `perAgentPods` the chart renders one **StatefulSet + headless Service per
   agent** (agentd-only pods; the ordinal derives `RUNTIME_AGENT_REPLICA` +
   `DBOS__VMID=<id>#<ordinal>` from `$HOSTNAME`), and runtimed runs
   **control-plane-only** with a **generated** `runtime.yaml` that rewrites each
   `config.agents` entry into a **remote replica pool**. This is **C3-remote ×
   A1-pool**: a remote agent may now set `replicas: N` paired with an `{i}`
   ordinal placeholder in `url:`, expanding to N per-ordinal attach entries at
   stable headless DNS (`<id>-<i>.<svc>`); `NextReplica` round-robins the
   **reachable** ordinals (new liveness-aware routing fed by one `HealthMonitor`
   per ordinal), while session affinity pins each session to its ordinal for
   life (durability absolute — a pinned-ordinal-down session 503s until it
   returns, never re-pins). StatefulSet ordinals = A1 executor ids and
   StatefulSet highest-ordinal-first scale-down = A2's suffix-only rule, now
   **enforced by Kubernetes**. Static replica count from config; scale-down is
   handled live (skip-unreachable), scale-up needs `helm upgrade` (documented
   seam). Single shared agent bearer (`secrets.agentAuthToken`) authenticates
   runtimed → each pod. **Known limitation:** brokered per-tenant secrets are
   spawn-time only, so per-agent-pod agents get provider creds via the chart
   Secret (backlog: brokered-secrets delivery to scheduled pods, home in C3 M2).
   Tested: config (remote-pool validation + `RemoteReplicaURL`), registry
   (pool expansion + skip-unreachable `NextReplica`), an integration test
   (`TestRemoteReplicaPoolAttach`: distribution, kill-one-ordinal liveness
   routing + affinity/durability, no restart), and chart render permutations
   (StatefulSet/headless/generated-config, single-replica concrete url, mode
   guards, monolith regression). THE FINAL HOLISTIC REVIEW + LIVE PROOF EARNED
   THEIR KEEP (as in C2 M1) — each caught an independent install-only bug
   invisible to per-task render/grep checks: (1) the holistic review found
   `podManagementPolicy: Parallel` would CrashLoop all-but-one ordinal on first
   install — in perAgentPods mode runtimed is control-plane-only and never
   Launches DBOS, so each agentd pod creates the DBOS schema itself, and DBOS's
   unlocked non-IF-NOT-EXISTS `CREATE SCHEMA/TABLE` raises non-retryable
   duplicate-object errors when N pods race an empty DB; fixed by dropping to the
   default `OrderedReady` so ordinal 0 creates the schema and goes Ready before
   ordinal 1 (exactly the serialization the integration test relies on). (2) the
   live kind proof found the runtimed `Service` had FOUR endpoints, not one: agent
   StatefulSet pods carry the same base `selectorLabels` (name+instance) as
   runtimed, so the Service load-balanced control-plane requests across the agent
   pods and `/agents/*` routes intermittently 404'd; fixed with an
   `app.kubernetes.io/component=control-plane` discriminator on the runtimed
   Deployment/Service/ServiceMonitor/NetworkPolicy, mode-gated so monolith renders
   byte-for-byte unchanged (and the immutable Deployment selector is untouched for
   monolith upgrades). LIVE PROOF (real kind cluster + bundled Postgres,
   2026-06-13, all passed): `make docker-image` → `kind load` → `helm install`
   with `postgresql.enabled=true`, `scheduling.mode=perAgentPods`, and two agents
   (support `replicas:2`, research `replicas:1`) → two StatefulSets + two headless
   Services + a control-plane-only runtimed; **OrderedReady proven** —
   `support-1` started only after `support-0` went Ready (0 restarts on the
   second), no DBOS schema race; **per-ordinal executor ids proven** — pod
   `support-0`→`DBOS__VMID=support#0`/`REPLICA=0`, `support-1`→`support#1`/`1`
   (derived from `$HOSTNAME`); runtimed attached all three ordinals at the correct
   per-ordinal headless DNS; **runtimed Service had exactly 1 endpoint** after the
   label fix; **`runtimectl conformance` PASSED all 6 checks** against BOTH the
   pool agent and the single agent through the in-cluster Service (create + stream
   + get + list routed to the per-agent pods); **distribution proven** — 11 pool
   sessions split 6/5 across ordinals 0/1, with `dbos.workflow_status` carrying
   distinct `support#0`/`support#1`/`research#0` executor ids (no double
   execution); and **scale-down skip-unreachable proven** — `kubectl scale
   support --replicas=1` then 8 new sessions all landed on ordinal 0 while
   runtimed stayed healthy (HealthMonitor marked ordinal 1 unreachable,
   `NextReplica` skipped it; K8s removed the highest ordinal = A2 suffix-only,
   enforced by the StatefulSet); clean `helm uninstall` + `kind delete`. Remaining
   perAgentPods follow-ups: per-agent `gateway:` opt-in env is not yet wired into
   the agent pod StatefulSet (gateway agents are monolith-only for now); brokered
   secrets to scheduled pods (C3 M2); RFC1123 agent-id validation; a per-agent
   NetworkPolicy; and runtimed-driven K8s-API scaling / HPA (a later C2
   milestone). Spec/plan:
   `docs/superpowers/{specs,plans}/2026-06-13-c2-m2-per-agent-pod-scheduling*`.

- **Containers / Kubernetes** — once C1 makes foreign agents first-class, package
  them as containers and add Helm charts / an operator for orchestrated scale (the
  "K8s later" half of the deploy decision). The conformance suite already validates
  any binary/container against the contract.

### C3. Remote agents (attach instead of spawn)

**C3 M1 — DONE (2026-06-13).** `runtimed` can now ATTACH to an already-running
remote `agentd` instead of only spawning local children. Config: an agent sets
`url:` (http/https) instead of `listen_addr:` (mutually exclusive, exactly one
required) plus an optional `${VAR}`-expanded `auth_token:`. The data plane was
already location-agnostic (reverse-proxy + `/healthz`), so the change upgraded
the dial identity from a bare host:port to a full base URL + optional bearer
across all four dial sites (reverse proxy, `/agents` health, metrics fan-out,
metrics target builder) via `AgentProcess.{Remote,BaseURL,AuthToken}` +
`baseURL()`/`DialBase()` + an `authTransport`. Lifecycle: remote agents get a
non-restarting `HealthMonitor` (poll `/healthz`, edge-triggered
`reachable|unreachable`, new `runtime_agent_reachable` metric) instead of a
`Supervisor` — degrade-don't-fail: a down remote never blocks boot, is never
restarted, and proxying returns 503 until it returns. `agentd` gained an
optional bearer middleware (`RUNTIME_AGENT_AUTH_TOKEN`, constant-time compare,
guards all paths incl. `/healthz` and `/metrics`); default-off so local spawns
are byte-for-byte unchanged. Decisions settled in the brainstorm:
operator-provisioned secrets (the remote agentd owns its env; a registration
handshake is deferred to C3 M2), opt-in bearer (mTLS deferred), `url:` schema.
Spawn-time-only fields (command/kind/memory/gateway) are rejected on a remote
agent. Tested hermetically (config validation, authTransport, registry,
`/agents` dial, fan-out, HealthMonitor edge-trigger, agentd auth) plus an
integration test (`TestRemoteAgentAttach`: mixed local+remote, proxy
round-trip to the remote, kill→unreachable while local stays healthy, no
restart). This unblocks per-agent-pod scheduling (C2): a K8s-scheduled agent is
exactly a remote agent whose lifecycle the orchestrator owns. Remaining C3:
the registration handshake (M2) and mTLS. Spec/plan:
`docs/superpowers/{specs,plans}/2026-06-13-c3-remote-agents*`.

- **Remote agents** — let an agent run on a different host while runtimed still
  manages it. The data plane is already location-agnostic: the control plane
  reverse-proxies plain HTTP to `listen_addr` and health-checks via
  `GET /healthz` (`controlplane/proxy.go`), so proxying, sessions, console,
  and identity all work against any reachable address today. What's local-only
  is the spawn/supervise step (`SpawnFunc` execs agentd or `command:` and
  babysits the PID). The milestone: an `agents: - url:` (or `remote: true`)
  config variant that **skips spawn** and attaches to an already-running
  contract-conformant agent — keeping health checks, proxying, and status
  reporting, and marking the agent `unreachable` (rather than restarting) when
  health fails. Open design questions to settle in the brainstorm:
  1. **Env/secrets delivery** — spawn-time env injection (PG DSN, tenant,
     brokered secrets, gateway URL/key, memory opt-in) doesn't exist for a
     process runtimed didn't start. Needs an attach-time handshake (agent pulls
     config from the control plane with a registration token) or operator-side
     provisioning, fail-closed either way.
  2. **Trust & transport** — agent ports currently assume only runtimed can
     reach them (no auth of their own); a remote agent needs mutual auth
     (shared token at minimum, mTLS ideally) and the remote host needs reach to
     Postgres (or the contract grows a way to avoid direct DB access).
  3. **Lifecycle semantics** — no restart-on-exit for a process we don't own;
     define what `runtimectl status` shows and how degrade-don't-fail applies.
  Natural stepping stone to C2: K8s-scheduled agents are exactly "remote
  agents whose lifecycle is owned by the orchestrator". (Backlogged 2026-06-11
  after confirming the workaround — a placeholder `command:` plus a manually
  started remote agentd — works but loses secrets brokering and supervision.)

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
