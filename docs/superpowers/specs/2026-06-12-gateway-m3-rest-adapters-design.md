# Gateway M3 — REST/OpenAPI → Tool Adapters

**Date:** 2026-06-12
**Status:** Approved design, pre-implementation
**Sub-project:** B1 Gateway, milestone 3 (after M1 MCP federation core, M2 semantic tool search)
**Builds on:** Gateway M1 (Manager/upstream/dialFunc/upstreamConn seams, supervision, tenant views), Gateway M2 (search indexes whatever the catalog holds — generated tools included automatically), Observability M1 (gateway metrics apply to REST upstreams for free).

## 1. Context & purpose

The gateway federates only MCP servers today. AgentCore's Gateway headline
feature is turning *any* HTTP API into agent tools. M3 closes that gap: an
operator points `runtime.yaml` at an OpenAPI 3.x document and every selected
operation becomes an ordinary federated gateway tool — named, tenant-filtered,
searchable, metered, and callable by any `gateway: true|search` agent with
zero agent-side changes.

## 2. Decisions (settled during brainstorming)

| Decision | Choice | Rationale |
|---|---|---|
| Tool-definition source | OpenAPI 3.x document (file path or URL) | AgentCore parity; most services already have a spec. Hand-written inline routes deferred |
| Upstream auth | Static headers with `${VAR}` expansion (existing mechanism) | One mechanism covers bearer/API-key/basic; OAuth2 client-credentials deferred |
| Operation selection | Optional `operations:` allowlist (operationIds or `METHOD /glob` patterns); omit ⇒ all | Big specs would flood the catalog; explicit choice beats silent truncation |
| Architecture | Third transport inside the existing Manager (new dialFunc branch) | The `dialFunc`/`upstreamConn` seam is transport-agnostic; supervision, tenancy, search, metrics all operate on `[]tool.Tool` and apply unchanged. A sidecar process (sandboxd pattern) buys nothing here — no isolation requirement |
| OpenAPI library | `github.com/getkin/kin-openapi` | De-facto Go OpenAPI 3.x parser; resolves $refs; validates documents |
| Live proof | Bundled demo API + one public API (Open-Meteo) | Controlled reproducibility plus a spec we didn't write — the bug class here is "real specs are messier than ours" |

## 3. Config surface

```yaml
gateway:
  servers:
    - name: orders
      openapi: ./specs/orders.yaml          # NEW: file path or https:// URL
      base_url: http://orders.internal:9000 # NEW: optional; default = first spec servers[] entry
      headers: {Authorization: "Bearer ${ORDERS_TOKEN}"}  # existing field, reused
      operations: ["listOrders", "GET /orders/*"]         # NEW: optional allowlist
      tenants: [acme]                                     # existing field, unchanged
```

`GatewayServer` gains `OpenAPI string`, `BaseURL string`, `Operations []string`.

Validation rules (config load):

- Exactly ONE of `command:` / `url:` / `openapi:` (extends the existing
  exactly-one rule).
- `forward_tenant: true` invalid with `openapi:` (same trust argument as
  `url:` — only stdio children are unreachable except via the gateway).
- `base_url` and `operations` valid ONLY with `openapi:`.
- `operations` entries are either bare operationIds (`listOrders`) or
  `METHOD /path-glob` patterns (`GET /orders/*`); method uppercase; glob is
  `path.Match` syntax against the spec path template.
- `base_url` absent is legal at load (the spec may declare `servers[]`); it
  becomes a dial error if the fetched spec has no usable server entry either.

## 4. Architecture

```
runtime.yaml (openapi:) ──▶ Manager.supervise (unchanged)
                                 │ dial
                            dialOpenAPI (NEW)
                                 │ 1. fetch spec (file read / HTTP GET + configured headers, 10s timeout)
                                 │ 2. kin-openapi parse + validate, $refs resolved
                                 │ 3. resolve base_url (config override > spec servers[0])
                                 │ 4. apply operations filter
                                 │ 5. generate []tool.Tool (one per operation)
                                 ▼
                            restConn (implements upstreamConn)
                                 ├─ Tools() → generated tools (already gateway-named)
                                 ├─ Ping()  → HEAD base_url (GET fallback on 405);
                                 │           ANY HTTP response = alive; transport error = down
                                 └─ Close() → no-op (stateless)
```

New files: `internal/gateway/openapi.go` (spec fetch/parse/filter/generate),
`internal/gateway/restconn.go` (restConn + the generated tool type's Execute).
`connect.go`'s production dialer branches on `cfg.OpenAPI != ""`.

`renameTools` must SKIP REST tools: they are generated directly with gateway
names `<server>__<opname>` and carry no `mcp__` prefix to strip. Explicit
branch, not pattern-matching on the name.

Everything downstream is untouched: supervision/backoff (reconnect re-fetches
the spec — drift handling for free), per-tenant views, principal binding,
search indexing (M2), `runtime_gateway_*` metrics (Observability M1),
`/gateway/status`.

## 5. Spec → tool mapping

One tool per selected operation.

- **Name:** `operationId`; fallback when absent: `<method>_<path>` slugified
  (lowercase, `/`→`_`, `{}` stripped, non-alphanumerics→`_`). Any `__` in the
  result collapses to `_` (`__` is the reserved gateway separator). Collisions
  after sanitization ⇒ later operation skipped with WARN (deterministic spec
  order).
- **Description:** `summary` + `description` joined, truncated at 1024 chars,
  ALWAYS prefixed with a generated one-liner `"<METHOD> <path> — "` so the LLM
  knows the operation shape even when the spec has no prose.
- **Input schema:** one JSON object merging:
  - path parameters → required top-level properties;
  - query parameters → top-level properties (required per spec);
  - header parameters declared in the spec → properties prefixed `header_`;
  - `requestBody` (application/json media type) → inlined under a `body`
    property, required if the spec marks the body required.
- `$ref`s are resolved by kin-openapi at parse time and then **deep-inlined**
  into plain JSON Schema with no `$ref` emission (kin-openapi's `MarshalJSON`
  re-emits any *nested* component reference as a literal `{"$ref": ...}` —
  only the top level is expanded — which is useless in a tool input schema,
  so generation recurses into the typed structure instead). Component reuse
  is the norm in real specs and is fully supported: the cycle check tracks
  the current ANCESTOR PATH only (marked on entry, unmarked on exit), so
  sibling reuse of the same component is legal — only ancestor-path
  repetition (e.g. `Node.children → Node`) is a genuine cycle. An operation
  with a genuinely cyclic schema (or nesting beyond a depth-30 backstop) is
  SKIPPED with one WARN naming the operation — never a dead upstream.
  External $refs are disallowed (security posture): a spec using them fails
  at dial, not per-operation.
- Operations with a REQUIRED request body that has no `application/json`
  media type: skipped with WARN (non-JSON bodies are out of scope, §10). An
  OPTIONAL non-JSON body just drops the `body` property — the operation stays
  usable bodyless.

## 6. Execution semantics

Each generated tool's `Execute(ctx, input)`:

1. Unmarshal the input object; validate required fields explicitly (schema
   validation is advisory — the sandboxd lesson: handlers validate, always).
2. **URL build:** interpolate `{pathParam}` segments with URL-escaped values.
   A path-param value containing `/`, `..`, or an encoding thereof (`%2F`,
   `%2E%2E`) is rejected with a tool error — traversal guard. Query params
   appended (absent optionals skipped; arrays serialized per spec `style`,
   default form/comma).
3. **Headers:** configured static headers set first; spec-declared `header_*`
   inputs set after BUT an input header whose canonical name matches a
   configured header is REJECTED (case-insensitive check) — an agent can
   never override Authorization. Content-Type set to application/json when a
   body is present.
4. **Body:** the `body` property marshaled as JSON.
5. **Client:** one `http.Client` per upstream, created at dial: 30s timeout,
   redirect policy = same-host only, max 3 (cross-host redirect ⇒ error — a
   compromised upstream cannot bounce gateway credentials elsewhere).
   Response read through `io.LimitReader` at 1 MiB.
6. `Execute`'s ctx flows into the request (caller cancellation propagates).
7. `IsConcurrencySafe`: true for GET/HEAD operations, false otherwise.

**Response contract** (tool result text, JSON envelope):

```json
{"status": 200, "headers": {"content-type": "application/json"}, "body": {...}, "truncated": false}
```

- `body`: parsed JSON when the response Content-Type is JSON and parses;
  otherwise the raw string. Truncated at 1 MiB with `"truncated": true`.
- Only `content-type` echoed in `headers`.
- **HTTP 4xx/5xx is a RESULT, not a tool error** — a 404 from `getOrder` is
  information for the agent, not an MCP isError. Tool errors are reserved
  for: input validation failures, traversal rejections, header-override
  attempts, transport failures, timeouts.

## 7. Security posture

- **Config headers inviolable:** case-insensitive denylist prevents
  `header_authorization` (or any casing) from shadowing a configured header.
- **SSRF containment:** the agent controls only parameter VALUES — never
  host, scheme, or path structure. Traversal guard on path params;
  cross-host redirects refused.
- **Shared credentials per upstream** (recorded limitation): all tenants
  with visibility share the upstream's credentials, exactly as HTTP MCP
  upstreams today; `tenants:` still controls visibility. Per-tenant
  credentials deferred (needs secrets-broker integration).
- **Spec fetch** uses the same configured headers (spec endpoints behind the
  same auth work; public specs unaffected) — and therefore the same
  exact-same-host redirect policy as API calls (review-caught: Go's default
  client follows cross-host redirects, only stripping Authorization-class
  headers, not custom auth headers or subdomain hops — a spec URL redirect
  must not bounce gateway credentials elsewhere).
- Secrets stay in env vars via existing `${VAR}` expansion.

## 8. Failure posture (degrade-don't-fail)

| Failure | Behavior |
|---|---|
| Spec unfetchable / unparseable at dial | Upstream down; error in `/gateway/status`; capped-backoff retry re-fetches |
| Spec valid but zero operations match the filter | Upstream connects with 0 tools + one WARN (visible operator typo, not fatal) |
| Single operation unmappable | Skipped with WARN naming it; rest of the spec proceeds |
| API down mid-session | Ping transport-fails ⇒ markDown ⇒ backoff redial (re-fetches spec); in-flight calls return isError |
| API returns 4xx/5xx | Valid result with `status` — agent reasons about it |
| Response not JSON despite spec | String body, never an error (specs lie; bodies don't) |
| Response > 1 MiB | Truncated with flag, never an error |

Ping semantics: `HEAD <base_url>`, falling back to `GET` on 405; ANY HTTP
response (including 4xx/5xx) = alive — REST APIs have no standard health
endpoint, and any response proves reachability. Only transport errors mark
the upstream down.

## 9. Testing & done criteria

**Hermetic unit tests** (httptest servers play spec host and API):

- `internal/config`: exactly-one-of-three; `forward_tenant`+`openapi`
  rejected; `base_url`/`operations` only with `openapi:`; operations pattern
  syntax validation.
- Generation (`openapi.go`): operationId naming, fallback slug, `__`
  collapse, collision skip; param mapping (path required / query / `header_`
  prefix / body + required propagation); $ref resolution; bad-operation skip;
  allowlist by id AND by `METHOD /glob`; description prefix + truncation;
  base_url resolution order (config > spec servers[0] > dial error).
- Execute (`restconn.go`): path interpolation + escaping + traversal
  rejection (incl. encoded forms); query serialization (arrays, absent
  optionals); header precedence (config wins, case-insensitive);
  body marshaling + Content-Type; 4xx-as-result; non-JSON body; 1 MiB
  truncation flag; same-host redirect followed, cross-host refused; ctx
  cancellation aborts the request.
- restConn: Ping alive on 2xx/4xx/5xx, down on transport error, GET fallback
  on 405; reconnect re-fetch (serve spec A → kill → serve spec B → tools
  updated, generation bumped).
- `renameTools` skip branch: REST tools pass through un-renamed.

**Through-serve e2e** (`test/gateway_rest_e2e_test.go`, integration tag):
runtimed + identity on + httptest REST API with spec; external MCP client
calls `<server>__<op>` through `/gateway/mcp`; tenant filtering hides the
upstream from the other tenant; `runtime_gateway_tool_calls_total` series
appears for the REST call.

**Live proof (recorded in the ROADMAP entry):**

1. Bundled demo API (`examples/rest-demo/`: small Go orders service + its
   OpenAPI spec) federated; external MCP client lists and calls generated
   tools through the gateway.
2. Open-Meteo (public, no auth) federated from its real spec; a forecast
   call succeeds — parser proven against a spec we didn't write.
3. End-to-end agent turn: a gateway-enabled agent answers a question that
   requires a generated REST tool.
4. `gateway: search` discovers a REST tool by natural-language query.

Done = all suites green + live proof recorded + ROADMAP/README updated +
merged to master.

## 10. Out of scope (recorded for later milestones)

- OAuth2 client-credentials token flow (cache/refresh lifecycle).
- Per-tenant upstream credentials (secrets-broker integration).
- Non-JSON request bodies (form-encoded, multipart).
- OpenAPI callbacks, webhooks, links; response-schema validation.
- Swagger/OpenAPI 2.0 (kin-openapi targets 3.x; 2.0→3.x conversion deferred).
- Dynamic upstream registration (separate backlog item).
- Inline hand-written route definitions (YAML routes without a spec).
- Form-style explode:true query arrays (repeated params) — arrays always serialize comma-joined in M3.

## 11. Risks & mitigations

- **Real-world spec messiness** (missing operationIds, vendor extensions,
  enormous schemas) — skip-with-WARN posture per operation; live proof
  includes a third-party spec; description/schema size caps.
- **kin-openapi dependency weight** — standard, widely used, maintained;
  isolated to `internal/gateway/openapi.go`.
- **Catalog flooding from large specs** — `operations:` allowlist + search
  mode; a WARN when a single spec generates > 50 tools nudges operators
  toward filtering.
- **Credential leakage via redirects or header override** — same-host-only
  redirect policy; case-insensitive config-header denylist; both unit-tested.
- **Spec drift vs running API** — reconnect re-fetches; mismatches surface
  as 4xx results the agent can reason about, never silent corruption.
