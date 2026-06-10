# C1 M2 — Claude Agent SDK Adapter (Nutrition Investigator, Third Implementation)

**Date:** 2026-06-10
**Status:** Approved design, pre-implementation
**Sub-project:** C1 Polyglot agent hosting, milestone 2 (M1 = Level-1 OpenAI Agents SDK shim, merged 2026-06-08)
**Builds on:** `contrib/shims/python/` (`runtime_contract`, consumed UNCHANGED), `examples/nutrition-label-openai/` (the ported agent + the worked-example template)

## 1. Context & purpose

C1's architectural bet is "one contract layer per language + a thin adapter per
framework." Milestone 1 proved the contract layer with one consumer (OpenAI
Agents SDK). This milestone is the **reuse test**: a second framework — the
Claude Agent SDK (Python) — hosted by writing only an adapter, with the shim
untouched. The Claude Agent SDK is a deliberately hard test: its architecture
(a spawned `claude` CLI subprocess owning state as JSONL transcripts on disk)
is nothing like the OpenAI SDK's in-process client, so an adapter that stays
thin here is strong evidence the seam is right.

The demo agent is the **SG Nutrition Investigator, third implementation**
(after Go/harness and OpenAI SDK) at **full parity**: 4 nutrition tools, typed
`NutritionVerdict` rendered to prose, learned aliases across sessions, live
vision verdict on `milo.jpeg`, Level-1 conversation resume across restarts.

**Honest-measurement clause:** if the port forces ANY change to
`runtime_contract`, that is a milestone finding to surface loudly in the
ROADMAP entry — not to paper over. Likewise the "~30-line adapter" claim gets
measured and reported as whatever it actually is.

## 2. Decisions (settled during brainstorming)

| Decision | Choice | Rationale |
|---|---|---|
| Framework | Claude Agent SDK (Python, `claude-agent-sdk`) | Strongest reuse test (subprocess+disk-state architecture, unlike OpenAI SDK); scouts Anthropic-stack hosting |
| Demo agent | Port the nutrition investigator (3rd implementation) | Apples-to-apples comparison across 3 frameworks of one agent |
| Fidelity | Full parity incl. live vision verdict | Same bar as the OpenAI milestone; vision is the riskiest piece and gets de-risked first |
| L1 memory | SDK-native resume + id map | SDK owns conversation state (JSONL under pinned `CLAUDE_CONFIG_DIR`/`cwd`); adapter stores runtime→SDK session-id mapping in the shim's SQLite; honest test of the SDK's persistence |
| Tool surface | Strip ALL built-ins; 5 in-process MCP tools only | No file/shell access on a network-facing agent; same logical surface as the OpenAI version |
| Typed verdict | Tool-as-output (`submit_verdict` MCP tool) | Schema-enforced via MCP input validation against the same pydantic model; framework-idiomatic |
| Auth | LiteLLM proxy via `ANTHROPIC_BASE_URL` | VERIFIED: proxy serves Anthropic-format `/v1/messages` with namespaced model ids (`claude-sonnet-4-6-asia-southeast1` round-tripped) |
| Turn lifecycle | Per-turn `query()` + `resume=`, not warm `ClaudeSDKClient` | Matches the adapter's turn-scoped calls and the SDK docs' restart-survivable pattern; subprocess-per-turn cost acceptable for a demo (noted as the Level-2-era optimization point) |

## 3. SDK facts the design rests on (verified against current docs, 2026-06-10)

- Package `claude-agent-sdk` (Python ≥3.10). `query(prompt, options) ->
  AsyncIterator[Message]` spawns a bundled `claude` CLI subprocess per session
  (no separate CLI install needed); `ClaudeAgentOptions.cli_path` can override.
- Persistence: transcripts at `$CLAUDE_CONFIG_DIR/projects/<encoded-cwd>/<session-id>.jsonl`.
  Every turn's `ResultMessage.session_id` identifies the SDK session; resuming
  is `ClaudeAgentOptions(resume=<id>)`. **Resume is cwd-keyed** — a cwd
  mismatch silently starts a fresh session, so the adapter pins `cwd` and
  `CLAUDE_CONFIG_DIR`. External ids cannot be supplied; capture-and-map is the
  documented pattern.
- Messages: `AssistantMessage` (content blocks incl. `TextBlock`),
  `ResultMessage{subtype, is_error, result, session_id, ...}`. Drain the
  iterator to the ResultMessage (early `break` has asyncio-cleanup issues).
- Tools: `tools: list[str]` controls which tools EXIST (`allowed_tools` only
  auto-approves, it does not restrict); `disallowed_tools` bare names remove
  tools from context. Custom in-process tools via `@tool` +
  `create_sdk_mcp_server(...)`, wired as `mcp_servers={...}`; names become
  `mcp__<key>__<tool>`. `permission_mode="dontAsk"` denies anything not
  explicitly allowed — the right headless posture here.
- Auth: `ANTHROPIC_API_KEY`; `ANTHROPIC_BASE_URL` routes through a proxy.
  Model via `ClaudeAgentOptions(model=...)`.
- Hosting notes: one subprocess per session; pin per-tenant/agent
  `CLAUDE_CONFIG_DIR`; `setting_sources=[]` isolates filesystem settings (a
  Python SDK bug ≤0.1.59 is noted in docs — verify at plan time and use the
  env-var isolation `CLAUDE_CODE_DISABLE_AUTO_MEMORY=1` as backup).

## 4. Layout

```
examples/nutrition-label-claude/
  agent.py        # shared domain logic, SDK-free: SFA data load, alias memory,
                  # NutritionVerdict (pydantic), render_verdict, remember_verdict
                  # (ported from the openai example's agent.py, minus SDK types)
  tools.py        # 5 in-process MCP tools (@tool + create_sdk_mcp_server):
                  #   lookup_additive(code_or_name)
                  #   find_additives_in_ingredients(ingredients_text)
                  #   recall_verdict(product_name)        # learned-alias memory read
                  #   remember_alias(alias, product_name) # memory write
                  #   submit_verdict(<NutritionVerdict schema>)  # typed output
  sessions.py     # runtime session_id → SDK session_id map; one table in the
                  # SQLite at RUNTIME_SHIM_DB (shared file, separate table —
                  # same pattern as the OpenAI adapter's SQLiteSession)
  adapter.py      # the AgentAdapter — the milestone's exhibit; target: thin
  serve.py        # mirrors the openai example: print resolved model, serve(AdapterFactory)
  Makefile        # uv-driven: install / run / test (mirrors openai example)
  pyproject.toml  # uv project; deps: claude-agent-sdk, pydantic
  runtime.nutrition-claude.yaml  # command: uv run python serve.py, workdir, gateway off
  README.md       # run instructions + the 3-implementations comparison table
  .env            # gitignored: ANTHROPIC_API_KEY, ANTHROPIC_BASE_URL, ANTHROPIC_MODEL
  milo.jpeg, sfa_additives.json  # copied fixtures
  tests/          # hermetic unit tests (no SDK subprocess, no network)
```

`contrib/shims/python/runtime_contract` is consumed as the same editable path
dependency the OpenAI example uses. NO shim changes expected.

## 5. The adapter

### 5.1 Turn flow

```
run(session_id, message, images, history):
  sdk_id = sessions.lookup(session_id)            # None on first turn
  opts   = ClaudeAgentOptions(
             resume=sdk_id,                        # None ⇒ fresh SDK session
             mcp_servers={"nutrition": NUTRITION_SERVER},
             tools/disallowed_tools = <strip built-ins, §5.3>,
             permission_mode="dontAsk" + allowed mcp__nutrition__* tools,
             system_prompt=<investigator prompt + "always finish by calling submit_verdict">,
             model=os.environ["ANTHROPIC_MODEL"],
             cwd=<example dir>, max_turns=<bounded>)
  prompt = <text, or streaming-input form carrying the image block (§5.2)>
  drive query(prompt, opts) to ResultMessage:
    - capture submit_verdict input via the tool handler (turn-scoped holder)
    - capture ResultMessage.session_id → sessions.store(session_id, sdk_id)
  if verdict captured: remember_verdict(v); yield text event (render_verdict(v))
  elif ResultMessage.is_error: yield error event (subtype + detail)
  else: yield text event (ResultMessage.result or accumulated text) — the
        no-verdict fallback; logged as a fidelity warning
  never raise out of run(); catch-all → one error event
```

`history` is unused (SDK resume owns memory) — same stance as the OpenAI
adapter, documented in the same place.

### 5.2 Vision (riskiest piece — de-risked first in the plan)

Image turns cannot ride a plain string prompt. The adapter uses the SDK's
streaming-input form: an async iterable yielding one user message whose
content is a list of blocks — the text plus an image block (base64 +
media-type from the contract `Image`). The plan's FIRST task is a standalone
spike script proving text+image round-trips through `query()` against the
proxy (model `claude-sonnet-4-6-asia-southeast1` or another vision-capable
namespaced id), pinning the exact input shape before any porting begins. If
the streaming-input image path fails against the proxy, fallback order:
(1) different namespaced model; (2) image via a temp file + a read-image MCP
tool the agent calls; (3) ONLY as last resort, escalate scope back to the
operator — vision parity is a done-criterion, not droppable silently.

### 5.3 Tool surface

Only the 5 nutrition MCP tools exist. Built-ins are stripped via `tools=[]`
(if the empty list is honored as "no built-ins" — plan task verifies) AND
`disallowed_tools` bare names for the dangerous set (Bash, Write, Edit, Read,
WebFetch, WebSearch, ...) as belt-and-braces. `permission_mode="dontAsk"`
with the `mcp__nutrition__*` tools allowed. The spike task confirms the
combination yields an agent that can call nutrition tools and nothing else.

### 5.4 Typed verdict (tool-as-output)

`submit_verdict`'s input schema is generated from the `NutritionVerdict`
pydantic model (`model_json_schema()`), so MCP-level validation enforces the
shape the OpenAI version got from `output_type=`. The handler returns a
short "verdict recorded" result to the model and stashes the validated object
in a turn-scoped holder the adapter reads after the run. The system prompt
makes calling it mandatory; the no-call fallback (§5.1) keeps the agent
usable while flagging fidelity loss.

### 5.5 Sessions map + state pinning

`sessions.py`: one table `sdk_sessions(runtime_id TEXT PRIMARY KEY, sdk_id
TEXT NOT NULL)` in the `RUNTIME_SHIM_DB` SQLite (shared file with the shim's
contract store — separate table, no schema interference; same co-location
pattern the OpenAI adapter used for `SQLiteSession`). `CLAUDE_CONFIG_DIR` is
pinned to `<dir(RUNTIME_SHIM_DB)>/claude-config` and `cwd` to the example
dir, satisfying the cwd-keyed-resume gotcha; both derived, not operator-set.

## 6. Config & ops

`runtime.nutrition-claude.yaml` mirrors the OpenAI example's: one agent,
`command: [uv, run, python, serve.py]`, `workdir` at the example dir,
`listen_addr` on a free port. Env (operator/.env): `ANTHROPIC_API_KEY`,
`ANTHROPIC_BASE_URL=https://litellm-stg.aip.gov.sg`,
`ANTHROPIC_MODEL=claude-sonnet-4-6-asia-southeast1` (any vision-capable
namespaced id works). The platform injects `RUNTIME_*` as for any agent.
Secrets brokering (Identity M2) can inject the key per-tenant later —
out of scope here, noted in the README.

## 7. Testing & done criteria

**Hermetic unit tests (no SDK subprocess, no network):**
- `sessions.py`: map round-trip, absent ⇒ None, upsert on re-store.
- `tools.py`: lookup/find against `sfa_additives.json` fixtures; alias
  remember/recall round-trip; `submit_verdict` handler validates good input,
  rejects schema violations, stashes the object.
- `adapter.py` with a faked query iterator (monkeypatched `query`): text turn
  yields one rendered-verdict text event; image turn builds the §5.2 input
  shape; ResultMessage.is_error ⇒ error event; verdict-never-submitted ⇒
  fallback text event; exception inside ⇒ single error event, nothing raised;
  session id captured and stored.
- Shim regression: the existing `runtime_contract` test suite passes
  UNCHANGED (`pytest` in `contrib/shims/python`).

**Live proof (manual, recorded in ROADMAP entry):**
1. Vision spike green (plan task 1) — text+image through query() via proxy.
2. Hosted end-to-end: `runtimed` + the example; `runtimectl conformance`
   PASSED against the hosted agent.
3. Real vision verdict: milo.jpeg through the control-plane proxy ⇒ rendered
   `NutritionVerdict` prose streamed back.
4. Level-1 resume: kill + restart the platform; a follow-up question in the
   SAME runtime session references the earlier verdict correctly.
5. Learned alias: teach an alias in one session; recall it in a NEW session.

**Honest measurements recorded in the ROADMAP entry:** adapter.py line count;
any shim changes (expected: zero); any deviations from the OpenAI example's
authoring experience worth noting for the future TS shim.

## 8. Out of scope

- Level 2 (in-flight crash resume) and the PydanticAI+DBOS deep integration.
- SessionStore mirroring (the SDK's write-through store protocol).
- TS shim; further framework adapters (PydanticAI remains the M3 candidate).
- Gateway consumption from this example (`gateway:` opt-in is orthogonal).
- Per-tenant key injection via secrets brokering (works already; not demoed).

## 9. Risks & mitigations

- **Image input shape** — top risk; dedicated first-task spike against the
  real proxy before any port work (§5.2 with explicit fallback ladder).
- **Proxy compat beyond /v1/messages basics** (streaming, tool use through
  LiteLLM's Anthropic passthrough): the spike exercises tool calls + image +
  streaming together; surprises surface on day one.
- **`tools=[]` semantics** (does empty list mean "none"?): verified in the
  spike; `disallowed_tools` belt-and-braces regardless.
- **Subprocess-per-turn latency** (~1-3s spawn overhead): acceptable for the
  demo; documented in README; the warm `ClaudeSDKClient` upgrade is noted as
  the Level-2-era optimization.
- **SDK version drift** (young SDK, options renamed across releases): pin the
  exact `claude-agent-sdk` version in pyproject; record it in the README.
- **`setting_sources` isolation bug (≤0.1.59)**: use env-var isolation
  (`CLAUDE_CODE_DISABLE_AUTO_MEMORY=1`, pinned `CLAUDE_CONFIG_DIR`) regardless
  of SDK version, so host-level `~/.claude` config can never leak into the
  hosted agent.
