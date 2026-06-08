# Example Agent: Deploying the SG Nutrition Investigator into Runtime

**Date:** 2026-06-08
**Status:** Design approved
**Context:** Runtime spine (M1–M3) complete. This is the first *real* agent deployed
through the spine — a worked example proving the platform hosts a non-trivial,
production-shaped agent, not just the deterministic test agent.

---

## 1. Goal

Take an existing standalone agent — the **Singapore Nutrition Label Investigator**
(originally an OpenAI Agents SDK Python program at
`/Users/sausheong/projects/agents_sdk/openai-demo`) — and deploy it as a
first-class agent **running inside runtime**: a supervised subprocess whose
agent loop is a durable DBOS workflow, reachable through the control plane's
HTTP/SSE contract, visible in the web console, and exercised by the CLI.

The deliverable is twofold:
1. The ported agent (`examples/nutrition/`), harness-native Go.
2. The deployment path: how `runtimed` boots it, how a user invokes it, and the
   docs that make this repeatable for the *next* agent.

## 2. Why a port (not container hosting)

Runtime today hosts **harness-native Go agents**: `agentd` wires a harness
`AgentSpec` + `llm.LLMProvider` + `tool.Registry` into `agentruntime.Serve`
(the durable per-session DBOS loop). Hosting arbitrary-language or containerized
agents is explicitly deferred to a later cross-cutting milestone (ROADMAP §C,
"Containers / Kubernetes"). The agent HTTP/SSE contract was designed to admit
that future, but it is not built yet.

Therefore the faithful way to deploy this Python agent *today* is to port it to
a harness-native Go agent that:
- preserves the **same behavior**: same 4 tools, same SFA additive dataset, same
  cross-run memory + alias learning, same investigator system prompt;
- is **backed by the same model**: harness's OpenAI provider pointed at the same
  LiteLLM proxy the original used.

This is a legitimate "deploy an agent" story for the platform's current
capabilities, and it doubles as the template every future harness-native agent
will copy.

### Fidelity notes (what changes in the port)

- **Structured output → free text.** The original returns a typed Pydantic
  `NutritionVerdict` via `output_type=`. Harness agents return free text. The
  port produces the same GREEN/AMBER/RED verdict as **prose** (matching the
  Claude demo's style), with the reasoning-first ordering preserved by the
  system prompt. Not a regression in capability — a difference in surface.
- **Image input.** The original reads a *photo* of the label. The runtime M1
  contract (`POST /sessions {message}` → SSE) is text-only. We extend the
  contract to carry an optional image (see §5), so vision fidelity is preserved.
- **Memory location.** The JSON memory file lives under a configurable dir
  (`RUNTIME_NUTRITION_DATA_DIR`, default the working dir) so the deployed agent
  has a writable, inspectable memory just like the original.

## 3. Components & file structure

```
runtime/
  examples/nutrition/              NEW — the ported agent (harness-native)
    agent.go                       BuildConfig(deps) → agentruntime.Config (spec, provider, tools, prompt)
    tools.go                       4 harness tools: check_sfa_additive, recall_product, check_hcs, calculate_nutri_grade
    additives.go                   SFA additive index (load + normalize + alias/E-number resolution)
    memory.go                      cross-run JSON memory (learned_aliases + products) with alias learning
    prompt.go                      investigator system prompt (const)
    data/sfa_additives.json        embedded SFA permitted-additives table (//go:embed), copied from the original
    agent_test.go / tools_test.go  hermetic unit tests (no network)
  cmd/agentd/main.go               MODIFIED — agent-kind selection (testagent | nutrition)
  internal/agentkind/registry.go   NEW — kind → Config builder map (keeps agentd main thin + testable)
  internal/config/config.go        MODIFIED — AgentConfig gains optional `kind`
  controlplane/proxy.go            MODIFIED — AgentProcess carries Kind; SpawnFunc sets RUNTIME_AGENT_KIND
  controlplane/registry.go         MODIFIED — pass Kind from config into AgentProcess
  agentruntime/server.go          MODIFIED — POST /sessions accepts optional image; durable plumb-through
  agentruntime/turnstep.go        MODIFIED — turnInput carries optional image (checkpointed, replay-safe)
  agentruntime/serve.go           MODIFIED — pass image into RunTurn
  runtime.nutrition.yaml           NEW — single-agent registry for the example
  test/nutrition_test.go           NEW — //go:build integration: boot + drive one session
  examples/nutrition/live_test.go  NEW — //go:build live: smoke against the real proxy (skips w/o key)
```

Everything new is additive; existing agents (`testagent`) keep working unchanged
(empty `kind` ⇒ testagent).

## 4. The ported agent (`examples/nutrition`)

### 4.1 Tools (harness `tool.Tool`)

Each implements `Name/Description/Parameters/Execute/IsConcurrencySafe`. Behavior
is a faithful Go port of the Python `@function_tool` functions:

| Tool | Behavior | ConcurrencySafe |
|---|---|---|
| `check_sfa_additive(additive, e_number_hint?)` | Resolve against the embedded SFA table by E-number / INS / normalized name / colloquialism; if a name misses but a hint number hits, **learn** the alias (persist). Returns the permitted/“not found” text + consumer note. | **false** (may write learned alias) |
| `recall_product(product_name)` | Look up prior verdict in memory; return “first investigation” or the prior summary+recommendation. | true |
| `check_hcs(product_name)` | Query data.gov.sg HCS dataset over HTTP; return certified/not-found. | true |
| `calculate_nutri_grade(sugar_per_100ml, saturated_fat_per_100ml)` | Pure A/B/C/D banding. | true |

The normalization rules (drop parentheticals/stereo markers, index E **and** INS
incl. base numbers, colloquial map, consumer-notes overlay) are ported verbatim
from the Python so additive matching matches the original.

### 4.2 Data & memory

- `data/sfa_additives.json` is embedded with `//go:embed` (copied from the
  original; ~540 entries). The index is built once at construction.
- Memory is a JSON file (`learned_aliases`, `products`) under
  `RUNTIME_NUTRITION_DATA_DIR` (default `.`), loaded at construction and written
  on change. Reads/writes are mutex-guarded (a session’s tools may run in the
  durable loop; harness `RunTurn` dispatches serially, but the guard is cheap
  insurance and documents intent). After a verdict, the agent does **not**
  auto-write the product record (the original wrote it in its CLI `main`, outside
  the agent loop) — instead `recall_product`/`check_sfa_additive` cover the
  learning surface that lives *inside* tools. Product-verdict persistence is
  noted as a future enhancement (would require a `remember_verdict` tool so it
  stays inside the loop). This keeps the port honest: only learning that the
  original did *inside a tool* is ported inside a tool.

### 4.3 Provider & spec

- `BuildConfig(deps)` reads `OPENAI_API_KEY`, `OPENAI_BASE_URL`, `OPENAI_MODEL`
  from env and builds `openai.NewOpenAIProviderWithKind(key, baseURL,
  "openai-compatible")` (compat kind: LiteLLM proxy, suppresses reasoning knob).
- `AgentSpec{ID, Name:"SG Nutrition Investigator", Model: "openai/"+model,
  SystemPrompt: investigatorPrompt, MaxTurns: 12}`. The provider is supplied
  explicitly in `agentruntime.Config.Provider`, so the `Model` string is a label
  for display/logging; the live provider is the one we construct.
- Missing `OPENAI_API_KEY` ⇒ `BuildConfig` returns a clear error (agentd logs and
  exits). Per the chosen design we back the agent with the **live proxy**, no
  offline fallback.

## 5. Contract extension: optional image input

The durable loop already supports images end-to-end at the harness layer
(`RunTurn(ctx, userMsg, []llm.ImageContent, emit)` →
`session.UserMessageWithImagesEntry`). We expose it on the wire:

- `POST /sessions` body becomes
  `{ "message": string, "image_b64"?: string, "image_mime"?: string }`.
  `image_b64` is standard base64 of the raw image bytes; `image_mime` defaults to
  `image/jpeg` when an image is present.
- `turnInput` (the **checkpointed** workflow input) gains
  `ImageB64 string`, `ImageMime string`. Because it’s part of the durable input,
  the image is replay-safe: on recovery DBOS re-supplies the same input, so the
  re-driven first turn sees the same image. (Base64 string keeps `turnInput`
  plain-JSON-serializable, which DBOS requires.)
- In `sessionWorkflow`, the first turn decodes the base64 into
  `[]llm.ImageContent{{MimeType, Data}}` and passes it to `RunTurn`;
  continuation turns pass `nil` (as today). Decode failure ⇒ turn proceeds
  text-only with a logged warning (never crashes the durable loop).
- **Conformance unaffected:** image fields are optional; the existing suite
  (which posts `{message}`) still passes. We add no new required field.

Size note: a label photo is tens-to-hundreds of KB base64; it is stored once in
the `session_events`/DBOS workflow input. Acceptable for an example. Documented
as a known characteristic (a production image path would use object storage +
a reference, a Sandboxes/Gateway concern — out of scope here).

## 6. Agent-kind selection

`agentd` must build different agents. Introduce `internal/agentkind`:

```go
package agentkind
type Builder func(deps Deps) (agentruntime.Config, error)
type Deps struct { AgentID, ListenAddr, PostgresDSN string; DB *sql.DB }
func Get(kind string) (Builder, bool)   // "" or "testagent" → test agent; "nutrition" → nutrition agent
```

- `agentd/main.go` reads `RUNTIME_AGENT_KIND` (default `testagent`), looks up the
  builder, calls it, and serves the returned Config. Removes the hardcoded
  testagent wiring (testagent becomes one registered kind).
- `config.AgentConfig` gains `Kind string \`yaml:"kind"\`` (optional; empty ⇒
  testagent). `Validate` accepts empty; unknown kinds are caught at agentd build
  time with a clear error (config stays decoupled from the kind registry).
- `controlplane.AgentProcess` gains `Kind`; `SpawnFunc` exports
  `RUNTIME_AGENT_KIND=<kind>`. `NewRegistry` copies `a.Kind` from config.

This keeps `agentd` thin and makes kind→builder mapping unit-testable without a
subprocess.

## 7. Deployment path (the “how to deploy” story)

1. **Build:** `go build -o agentd ./cmd/agentd && go build -o runtimed ./cmd/runtimed && go build -o runtimectl ./cmd/runtimectl`.
2. **Configure:** `runtime.nutrition.yaml`:
   ```yaml
   agents:
     - id: nutrition
       name: SG Nutrition Investigator
       model: openai/gpt-5.4
       kind: nutrition
       listen_addr: 127.0.0.1:8201
   ```
3. **Env:** `OPENAI_API_KEY`, `OPENAI_BASE_URL=https://litellm-stg.aip.gov.sg`,
   `OPENAI_MODEL=gpt-5.4`, `RUNTIME_PG_DSN=...`, `RUNTIME_CONFIG=runtime.nutrition.yaml`.
4. **Run:** `./runtimed` — supervises the `nutrition` agentd subprocess, gates on
   `/healthz`, serves the control plane on `:8080`.
5. **Invoke (text):** `runtimectl invoke --agent nutrition "Investigate: <pasted label text>"`
   then `runtimectl logs --agent nutrition <session>` (or watch SSE).
6. **Invoke (image):** `curl -s localhost:8080/agents/nutrition/sessions
   -d '{"message":"Investigate this label.","image_b64":"'"$(base64 -i milo.jpeg)"'","image_mime":"image/jpeg"}'`
   then stream `GET /agents/nutrition/sessions/<id>/stream`.
7. **Observe:** the session appears at `/ui` (web console) and via
   `runtimectl sessions --agent nutrition`.

## 8. Testing strategy

- **Hermetic unit (default `go test ./...`):**
  - additive resolution (E-number, INS, base-number, name, colloquial, miss);
  - alias learning persists and resolves on a second lookup;
  - nutri-grade banding (A/B/C/D boundaries);
  - `recall_product` first-vs-seen;
  - `agentkind.Get` returns the right builder for ""/testagent/nutrition/unknown;
  - `turnInput` JSON round-trips with image fields (replay-safety guard).
- **Integration (`//go:build integration`, Postgres.app):** boot the nutrition
  agent via `agentruntime.Serve` with a **deterministic scripted provider stub**
  (so the test is network-free but exercises the real durable loop + tools +
  image plumbing), POST a session with a tiny image, assert SSE reaches a
  terminal `done` and a tool ran. Self-cleans tables + `dbos` schema like the
  other integration tests.
- **Live smoke (`//go:build live`):** if `OPENAI_API_KEY` is set, build the real
  Config and run one turn against the proxy; assert a non-empty verdict. Skips
  cleanly when unset, so it never breaks CI.

## 9. Out of scope

- Typed/structured output (harness returns text).
- Auto-persisting product verdicts inside the loop (needs a `remember_verdict`
  tool; noted as future).
- Container/non-Go hosting (ROADMAP §C).
- Object-storage image path, multi-image, streaming partial verdicts.

## 10. Success criteria

- `go build ./...` and `go test ./...` green (hermetic).
- Integration test boots the nutrition agent and drives a session with an image
  through the durable loop to a terminal event.
- With env set, `runtimed` boots the agent and a real label photo posted to
  `/agents/nutrition/sessions` yields a GREEN/AMBER/RED verdict over SSE.
- README documents the full deploy path so the next agent is a copy-paste away.
