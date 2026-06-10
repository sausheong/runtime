# Gateway M2 — Semantic Tool Search

**Date:** 2026-06-10
**Status:** Approved design, pre-implementation
**Sub-project:** B1 Gateway, milestone 2 (M1 = MCP federation core, merged 2026-06-10)
**Builds on:** Gateway M1 (`internal/gateway`), Memory M2 embedding plumbing (`internal/memory/embed.go`)

## 1. Context & purpose

M1 federates upstream MCP servers into one endpoint, but every consumer gets
the full tool catalog in `tools/list` — the reference filesystem server alone
contributes 14 tool schemas to an agent's prompt. As upstreams multiply, the
catalog becomes prompt bloat and tool-choice noise.

M2 adds a **search-first consumption mode**: a consumer in search mode lists
exactly one tool, `search_tools`. The agent describes what it needs in natural
language; `search_tools` returns the top-K matching tools (name, description,
full input schema) ranked by embedding cosine similarity; the agent then calls
any returned tool by name. All visible tools remain **callable** in search
mode — they are just not **listed**. Full-list mode (M1) is unchanged.

## 2. Decisions (settled during brainstorming)

| Decision | Choice | Rationale |
|---|---|---|
| Search UX | Search-tool + lazy catalog (callable-but-unlisted) | Actually shrinks the prompt; AgentCore-Gateway-like; full list still available |
| Rollout | Opt-in per agent (`gateway: search`); external clients per session | Zero change for M1 consumers |
| Embedding storage | In-memory, embed on connect/build; no schema | 10²–10³ vectors; brute-force cosine is microseconds; no migrations |
| Embedder config | Reuse `RUNTIME_EMBED_*`/`OPENAI_*` via `memory.NewEmbedderFromEnv`; fail-fast when search requested but unconfigured | One platform embedding config; loud misconfig (wireMemory posture) |
| Ranking | Top-K (default 5, cap 20) above a low floor (default 0.2, tunable) with full schemas in results | Schemas inline ⇒ no second round-trip; low floor per the OpenAI-embedding cosine lesson (Memory recall-floor recalibration) |
| Mode selection (wire) | Query param `/gateway/mcp?mode=search` | Rides the URL the platform already injects; zero agentd changes |
| Config shape | `gateway:` becomes string-or-bool union: `true`=full, `"search"`=search | Back-compat, reads naturally |
| Architecture | Separate Index component; Manager untouched | Clean seams; Index testable with fake embedder |
| List-suppression mechanism | SDK `AddReceivingMiddleware` intercepts `tools/list` | Verified present in go-sdk v1.5.0; `tools/call` resolves against full registered set untouched |

## 3. Components

### 3.1 `internal/gateway/index.go` — the Index (new)

Owns embedding + ranking. Constructed once in runtimed when both the gateway
and embeddings are configured; nil Index ⇒ search mode unavailable.

- **Vector cache:** `map[toolKey][]float32` keyed by content identity
  (`sha256(name + "\x00" + description)`), mutex-guarded. Each distinct tool
  text is embedded once per process lifetime — across views, generations, and
  reconnects. Unbounded growth is acceptable (bounded by distinct tool texts
  ever seen; ~KB per tool).
- **`Search(ctx, tools []tool.Tool, query string, k int) ([]Match, error)`**
  — ensures vectors exist for `tools` (embedding any misses, sequentially),
  embeds the query, brute-force cosine over the view's vectors, filters by
  floor, returns top-K sorted descending. `Match{Name, Description, InputSchema
  json.RawMessage, Score float64}`.
- **Embedder:** the `memory.Embedder` interface, injected. Tests use a fake
  with deterministic vectors.
- **Degrade-don't-fail:**
  - A tool whose embed fails is skipped for this search (logged at most once
    per tool text, retried on the next Search call); it remains listed in
    full mode and callable in both modes.
  - A query-embed failure returns an error; the `search_tools` handler maps
    it to an MCP `isError` result ("search temporarily unavailable") — never
    a transport failure.
- **Lazy only — no eager warming in M2.** Vectors are computed on first
  Search that needs them. (A background warm on generation bump is a future
  optimization; correctness must never depend on it.)

### 3.2 `internal/gateway/server.go` — mode-aware views (modified)

- **View key gains the mode:** cache key becomes e.g. `"t:acme|full"` /
  `"t:acme|search"` (and `"*|full"` / `"*|search"`). `viewKey` returns the
  mode-qualified key; the per-call view re-check in `toolHandler` compares
  the principal-view BASE (the key minus its trailing `|<mode>` segment, cut
  at the LAST pipe — tenant IDs are free strings and may contain `|`), so a
  session presented by a different principal is rejected. Mode is a session
  property, not part of the principal check: the same principal may hold
  full- and search-mode sessions concurrently (same principal-binding
  posture as M1).
- **Mode parsing:** `modeFromRequest(r)` reads `?mode=`; absent/empty ⇒ full;
  `search` ⇒ search; anything else ⇒ 400 before session creation. `?mode=search`
  when the Index is nil (embeddings unconfigured) ⇒ 400 "search mode requires
  embeddings (RUNTIME_EMBED_MODEL)".
- **Search-mode server build (`serverFor`):** register ALL visible tools (so
  `tools/call` resolves them — callable-but-unlisted) plus `search_tools`,
  then `srv.AddReceivingMiddleware(listFilter)` where `listFilter` intercepts
  method `tools/list` and rewrites the result to contain only the
  `search_tools` entry. Pagination is irrelevant at one tool. All other
  methods pass through.
- **`search_tools` definition:** name `search_tools` (no upstream prefix —
  upstream-tool names always contain `__`, so collision is impossible);
  description written for LLM consumption ("Search the tool catalog by
  describing what you want to do; returns matching tools you can then call
  directly"); input schema `{query: string (required), k: integer (optional)}`.
- **`search_tools` handler:** same per-call gates as M1 tool handlers, with
  one deliberate difference — **viewers may call `search_tools`** (search is
  a read, like `tools/list`) but still cannot call result tools. Handler
  parses input, clamps k to [1, cap], calls `Index.Search` over
  `Manager.ToolsFor(tenant)` for the caller's view, returns a JSON array of
  matches as a single `TextContent`. Zero matches ⇒ success with `[]` plus a
  "no tools matched; try a broader query" hint line.

### 3.3 Config & wiring

- **`config.GatewayMode`** (new type in `internal/config`): `off` / `full` /
  `search`. Custom `UnmarshalYAML` accepting bool (`true`→full, `false`→off)
  or string (`"search"`→search; `"full"`→full; anything else → load error).
  `AgentConfig.Gateway` changes type from `bool` to `GatewayMode`; zero value
  is `off`. Helper methods `Enabled() bool` (full or search) keep call sites
  readable. Registry/AgentProcess: `GatewayOn` derives from `Enabled()`; a
  new `GatewaySearch bool` rides along to buildEnv.
- **`buildEnv`:** for search-mode agents the injected URL becomes
  `<base>/gateway/mcp?mode=search`. agentd and agentkind are untouched (the
  URL passes through verbatim into `mcp.ServerConfig.URL`).
- **runtimed:** builds the Index when `cfg.Gateway.Enabled()` AND
  `memory.NewEmbedderFromEnv()` reports enabled; passes it to `NewHandler`
  (signature gains the Index or a setter — plan's choice). Fail-fast startup
  check (alongside `validateGatewayKeys`): any agent with `gateway: search`
  while embeddings are unconfigured ⇒ refuse to start, naming the agent.
- **Tunables:** `RUNTIME_GATEWAY_SEARCH_FLOOR` (default **0.2** — question↔
  description cosine runs ~0.25–0.40 on OpenAI-family embeddings, unrelated
  near 0; same calibration lesson as the Memory recall floor),
  `RUNTIME_GATEWAY_SEARCH_K` (default 5), hard cap 20.

## 4. Data flow

```
agent (gateway: search) → RUNTIME_GATEWAY_URL=…/gateway/mcp?mode=search
  tools/list  → [search_tools]                          (1 schema in prompt)
  tools/call search_tools {"query":"read a file"}
      → Index: embed(query) → cosine vs view vectors → top-K ≥ floor
      → TextContent JSON: [{"name":"fs__read_text_file","description":…,
                            "inputSchema":{…},"score":0.41}, …]
  tools/call fs__read_text_file {...}                   (callable though unlisted)
```

## 5. Error handling summary

| Condition | Behavior |
|---|---|
| `gateway: search` agent, embeddings unconfigured | runtimed refuses to start (named agent) |
| `?mode=search`, Index nil | HTTP 400 before session creation |
| `?mode=<junk>` | HTTP 400 |
| Query embed fails | `search_tools` returns `isError` "search temporarily unavailable" |
| Tool embed fails during index build | Tool excluded from this search, logged once per text, retried next Search; still listed (full mode) and callable |
| Zero matches above floor | Success, `[]` + broaden-query hint |
| Viewer calls `search_tools` | Allowed (read) |
| Viewer calls a result tool | `isError` forbidden (M1 rule unchanged) |
| Session replay across principals | Rejected by per-call principal-view re-check (mode is a session property: the same principal may hold full- and search-mode sessions concurrently) |

## 6. Testing & done criteria

Conventions as M1 (hermetic units; `//go:build integration` + Postgres.app;
go CLI is ground truth).

**Unit (fake embedder with deterministic vectors):**
- `GatewayMode` YAML union: true/false/"search"/"full"/invalid/absent.
- Mode wire: URL param parsing; 400 on junk mode; 400 on search-without-Index.
- Search-mode list = exactly `[search_tools]`; full-mode list unchanged (M1
  tests keep passing).
- Callable-but-unlisted: a tool absent from search-mode tools/list succeeds
  via tools/call.
- Tenancy: search results and callability both respect the caller's view;
  a tenant-scoped tool never appears in another tenant's search results.
- Ranking: floor excludes weak matches; K and cap respected; descending order.
- Vector cache: embed called exactly once per unique tool text across two
  searches and across a generation bump.
- Degradation: query-embed failure ⇒ isError; one tool's embed failure ⇒
  that tool absent from results, others present.
- Role: viewer can call `search_tools`, cannot call a result tool.
- Mode-qualified session binding: search-mode session replayed by a
  full-mode principal context is rejected (and vice versa).
- buildEnv: search-mode agent gets `?mode=search` URL; full-mode agent
  doesn't; runtimed fail-fast check (table test alongside
  `validateGatewayKeys`).

**Integration (through-serve):**
- runtimed + fake Streamable HTTP upstream (several tools with distinct
  descriptions) + a fake embedding endpoint (the e2e harness stubs
  `OPENAI_BASE_URL` with a deterministic embedder HTTP server): a
  `gateway: search` agent's view lists one tool; an external client in
  search mode searches, gets the expected tool top-1, and calls it.

**Live proof (manual, recorded):**
- Real embeddings (LiteLLM proxy, `text-embedding-3-small`) + the reference
  filesystem MCP server: `search_tools("read a file's contents")` surfaces
  `fs__read_text_file` top-1; the call round-trips. Floor sanity-checked
  against real cosine scores.

**Done =** all green + README/ROADMAP updated.

## 7. Out of scope (later Gateway milestones)

- REST/OpenAPI → tool adapters.
- Dynamic upstream registration; per-tenant self-service.
- Per-tenant embedding models; persistent (pgvector) embedding cache.
- `tools/list_changed` notifications / per-session dynamic lists.
- Search analytics, query logging, result-click feedback.
- Re-ranking beyond cosine (e.g. LLM re-rank).

## 8. Risks & mitigations

- **SDK middleware contract drift:** `AddReceivingMiddleware` + method-name
  matching on `tools/list` is the one SDK-internals-adjacent piece; pinned to
  go-sdk v1.5.0 (already a direct dep). A unit test locks the filtered-list
  behavior so an SDK upgrade that breaks the hook fails loudly.
- **Floor miscalibration:** defaulted low (0.2) and env-tunable; the live
  proof records real scores, and the zero-match path degrades to a helpful
  hint rather than silence.
- **Embedding latency on first search:** lazy embed of a 50-tool view ≈ 50
  sequential embed calls on the first search after a (re)connect. Acceptable
  for M2 (search results are not turn-latency-critical); the optional warm
  path and a batch-embed endpoint are future optimizations.
- **Two modes × N tenants cache growth:** bounded by 2 × tenant count SDK
  servers; same lifecycle as M1's cache (rebuilt on generation bump).
