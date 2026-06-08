# Host the `nutrition-label-openai` example on Runtime via the contract library

**Date:** 2026-06-08
**Status:** Design — approved, pending spec review
**Branch:** `feat/polyglot-shim-openai`
**Related:** `ROADMAP.md` §C1 (polyglot agent hosting); `2026-06-08-polyglot-shim-openai-design.md` (the Level-1 shim this builds on)

## Goal

Run the **full** `examples/nutrition-label-openai` agent — unchanged in behaviour —
as a first-class agent hosted by `runtimed`, using the Python contract shim. "Full"
means full fidelity: all 4 tools, the ~540-entry SFA permitted-additives table, the
typed `NutritionVerdict` output, and cross-run JSON memory. The hosted agent must
produce the same verdict a developer sees from `uv run python main.py`, just driven
over the runtime HTTP/SSE agent contract and gated by the same conformance suite that
gates Go-native agents.

## Background

The platform already hosts foreign-SDK agents through the generalized spawn path
(`command:`/`workdir:` config fields → `controlplane.AgentProcess.SpawnFunc` execs an
arbitrary argv). The Python shim (`contrib/shims/python/`) is two layers:

- `runtime_contract/` — a **framework-agnostic** library serving the 6 contract
  endpoints (FastAPI), SSE framing, `?since=N` replay, and a SQLite session+event
  store (Level-1 durability). Knows nothing about any agent framework.
- `adapters/openai_agents.py` + `main.py` — a **stripped-down stand-in** OpenAI
  nutrition agent: generic prompt, **no tools, no typed output, no SFA data, no
  memory**. A placeholder, not the real example.

The real example (`examples/nutrition-label-openai/main.py`) is the full agent but
runs only as a standalone CLI; it does not speak the contract. This work connects the
two: the real example becomes a hosted agent that reuses the genuinely-reusable
contract library.

## Decisions (from brainstorming)

1. **Fidelity:** Full (all tools, SFA data, typed verdict, memory). [Option A]
2. **Code placement:** The example grows its own `serve.py`; the agent + its adapter
   stay co-located in the example, reusing the shared contract library. [Option C]
3. **Structured output over the contract:** Render the validated `NutritionVerdict`
   to the example's signature pretty prose (reasoning, summary, 🟢/🟡/🔴 findings,
   recommendation) and emit it as a single `text` event. The model is still
   constrained to and validated against the schema — we serialize the validated
   object to prose for transport, rather than discarding the type. [Option A]
4. **Run ergonomics:** A `Makefile` mirroring `examples/nutrition-label-go`
   (`run` / `demo-text` / `demo-image` / `sessions` / `check-env` / `clean`), with its
   own config yaml. [Option A]
5. **Library packaging:** Promote `runtime_contract` to a standalone, path-installable
   package; the example declares it as a path dependency. [Option C]
6. **Stand-in removal:** Delete the shim's stripped-down OpenAI agent (`main.py`,
   `adapters/openai_agents.py`, `runtime.openai-shim.yaml`). The example becomes the
   one true OpenAI demo. The library README keeps the inline adapter template, so no
   pedagogical loss; the hermetic tests use a stub adapter, so no coverage loss.
   [Confirmed]

## Architecture

### Package topology

```
contrib/shims/python/                  ← the reusable contract LIBRARY
  pyproject.toml                       deps: fastapi, uvicorn only; build backend
                                       (hatchling) so it is path-installable as a wheel
  runtime_contract/                    unchanged: app, adapter, events, sse, store
  tests/                               unchanged hermetic tests (stub adapter)
  README.md                            updated: "this is the contract library; see
                                       examples/nutrition-label-openai for a full
                                       OpenAI agent hosted on it"
  ── REMOVED: main.py
  ── REMOVED: adapters/  (openai_agents.py, __init__.py)
  ── REMOVED: runtime.openai-shim.yaml

examples/nutrition-label-openai/
  pyproject.toml                       + path dep on the contract library
  agent.py        NEW   extracted agent: build_agent(), the 4 @function_tool tools,
                        SFA loader + indexes, memory helpers, NutritionVerdict +
                        Finding, INSTRUCTIONS, and render_verdict()
  main.py         SLIM  thin CLI: load_dotenv + argv + investigate() printing;
                        imports agent.py. `uv run python main.py` behaves as today.
  adapter.py      NEW   NutritionAdapter(AgentAdapter): drives the agent, renders
                        the verdict to prose, yields one text event.
  serve.py        NEW   entrypoint: build agent + Store + create_app(...); uvicorn.
  Makefile        NEW   run / demo-text / demo-image / sessions / check-env / clean
  runtime.nutrition-openai.yaml  NEW   one agent: command=[uv,run,python,serve.py],
                        workdir=this dir, listen_addr 127.0.0.1:8302
  sfa_additives.json, milo.jpeg, .env.example   reused in place
```

### Component responsibilities & interfaces

- **`agent.py`** (pure agent definition; no import-time side effects)
  - `build_agent() -> Agent` — reads `OPENAI_API_KEY` / `OPENAI_BASE_URL` /
    `OPENAI_MODEL` **lazily inside the function** (so importing the module does not
    require a key), constructs the `AsyncOpenAI` client + `OpenAIChatCompletionsModel`,
    registers the 4 tools, sets `output_type=NutritionVerdict`. No network call.
  - `render_verdict(v: NutritionVerdict) -> str` — pure formatter producing the exact
    prose block the CLI prints today (Product / Reasoning / Summary / 🟢🟡🔴 lines /
    Recommendation). Used by both `main.py` and `adapter.py`.
  - Tools, SFA loader/indexes, `agent_memory.json` helpers, `NutritionVerdict`,
    `Finding`, `INSTRUCTIONS` — moved verbatim from today's `main.py`.
  - Depends on: `agents` SDK, `httpx`, `pydantic`, local `sfa_additives.json`.

- **`main.py`** (standalone CLI front-end; behaviour unchanged)
  - `load_dotenv(override=True)`, argv parsing, `investigate(image_path)` printing,
    product-verdict persistence to memory. Imports `build_agent`, `render_verdict`,
    memory helpers from `agent.py`.
  - Depends on: `agent.py`, `python-dotenv`.

- **`adapter.py`** (the per-framework contract seam)
  - `class NutritionAdapter` implementing `AgentAdapter.run(session_id, message,
    images, history) -> AsyncIterator[ContractEvent]`.
  - Builds SDK input (text, or content-list with image data URL — same shape as
    `main.py`'s `investigate`). Keys an `SQLiteSession` on `session_id`. Calls
    `Runner.run(...)` **non-streamed** (with `output_type` set the SDK returns the
    structured object at the end, not `output_text` deltas), gets the validated
    `NutritionVerdict`, yields one `ContractEvent(type="text",
    text=render_verdict(v))`. Persists the verdict to `agent_memory.json` (so the
    hosted agent learns across sessions exactly like the CLI does across runs).
  - Never raises: any exception → `ContractEvent(type="error", error=str(e))`. Does
    NOT emit the terminal `done`/`error` (the library appends it).
  - Depends on: `agent.py`, `runtime_contract.events`, `runtime_contract.adapter`,
    `agents` SDK.

- **`serve.py`** (entrypoint)
  - Reads `RUNTIME_LISTEN_ADDR` (host:port), `RUNTIME_AGENT_ID` (default
    `nutrition-openai`), `RUNTIME_SHIM_DB` (default `./shim.db`).
  - Constructs `Store`, `NutritionAdapter` (which calls `build_agent()` once — a
    missing key fails fast at startup; the supervisor retries with backoff and the
    `runtimed` health-gate logs it), `create_app(adapter, store, agent_id)`; runs
    uvicorn.
  - Depends on: `runtime_contract.{app,store}`, `adapter.py`.

- **`runtime_contract` library** — unchanged code. Only `pyproject.toml`/README change:
  declare it as a buildable package (so a path dep resolves to an installable wheel)
  and reword the README to point at the example as the worked OpenAI agent.

### Data flow (one invocation)

```
runtimectl invoke / curl
  → control plane :8080  /agents/nutrition-openai/sessions  (auth at edge)
  → reverseProxy → serve.py :8302  POST /sessions {message, image_b64?}
  → create_app: store.create_session(); background run_session task
      → NutritionAdapter.run(): build SDK input, SQLiteSession(session_id),
        Runner.run(agent) → NutritionVerdict (validated) → render_verdict()
        → yield ContractEvent(type="text", text=<prose>)
      → library persists each event (seq), appends terminal done
  → GET /sessions/{id}/stream?since=0 → SSE: id:1 text, id:2 done
```

### Error handling

- Adapter never raises; failures become a single `error` event; library sets session
  status `error` and appends terminal `error`. Matches the existing shim contract.
- Missing/invalid `OPENAI_API_KEY` → `build_agent()` raises at `serve.py` startup →
  process exits → supervisor backs off and retries → `runtimed` logs "agent not
  healthy yet" (same behaviour as a misconfigured Go agent).
- HCS tool network errors are already handled inside the tool (returns a prose error
  string, not an exception) — unchanged.

### Durability

Level 1, consistent with the shim: sessions + event log persist in `shim.db`
(replayable via `?since=N`, listable after restart); `SQLiteSession` keyed on the
runtime session id gives conversation memory across restarts. Plus the agent's own
`agent_memory.json` (learned aliases + product verdicts) persists across sessions and
restarts. Level 2 (in-flight crash resume) remains out of scope (ROADMAP §C1).

## Run flow & acceptance

From `examples/nutrition-label-openai/`:

```bash
cp .env.example .env          # fill in LiteLLM proxy key
make run                      # builds platform binaries, uv sync, runs runtimed
                              # with runtime.nutrition-openai.yaml; runtimed execs
                              # `uv run python serve.py` as a supervised agent
# in a second shell:
make demo-text                # POST a pasted label → stream the prose verdict
make demo-image IMAGE=milo.jpeg   # base64 the photo → POST → stream the verdict
make sessions                 # runtimectl sessions --agent nutrition-openai
```

`.env` is auto-loaded by the Makefile (gitignored; `.env` overrides the shell so a
stray `OPENAI_API_KEY` cannot misroute to api.openai.com). `OPENAI_BASE_URL` /
`OPENAI_MODEL` default to the LiteLLM proxy, overridable in `.env` or on the CLI.

**Acceptance gates:**
1. `./bin/runtimectl conformance --agent nutrition-openai` → PASSED (same suite as Go
   agents).
2. `make demo-image IMAGE=milo.jpeg` streams a real, correct verdict (reasoning,
   findings, Nutri-Grade for the beverage, recommendation).
3. `make demo-text` likewise for a pasted label.
4. `make sessions` lists the completed sessions with `turns=1`.

## Testing

- **Library:** existing hermetic `tests/` unchanged (contract + store via stub
  adapter; no key/network).
- **Example unit:** a hermetic test for `render_verdict()` (pure function) over a
  constructed `NutritionVerdict` — asserts the prose contains product name, each
  findings tier, and the recommendation. No key/network.
- **Example live:** exercised via `make demo-*` (manual, needs a key). No live test in
  the default run, mirroring the Go example's gated `live_test.go`.
- **CI default run** (`go test ./...`, `uv run pytest`) stays hermetic.

## Files changed/created (summary)

Created: `examples/nutrition-label-openai/{agent.py, adapter.py, serve.py, Makefile,
runtime.nutrition-openai.yaml}`, a `render_verdict` unit test.
Modified: `examples/nutrition-label-openai/{main.py, pyproject.toml, README.md}`,
`contrib/shims/python/{pyproject.toml, README.md}`.
Removed: `contrib/shims/python/{main.py, adapters/, runtime.openai-shim.yaml}`.
No `.gitignore` change needed — the root `.gitignore` already covers `.env`,
`agent_memory.json`, `*.db` (`shim.db`), `.venv/`, `__pycache__`, `.pytest_cache`.

## Out of scope / non-goals

- Level-2 in-flight crash resume (ROADMAP §C1).
- A second consumer of the contract library (only the example consumes it now; lifting
  more consumers in later is non-churning given the standalone package).
- Changing the Go control plane, contract, conformance suite, or `runtime_contract`
  library code (only its packaging metadata + README change).
- Emitting structured JSON or `tool_result` events (verdict is prose `text` by
  decision 3).
```

