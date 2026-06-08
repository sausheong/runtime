# Polyglot Agent Hosting — Level-1 Python Contract Shim (OpenAI Agents SDK)

**Date:** 2026-06-08
**Status:** Design approved
**Roadmap:** ROADMAP §C1 (polyglot agent hosting). This is the first milestone of C1.
**Context:** The Runtime spine (M1–M3) hosts harness-native Go agents; the nutrition
example proved a real agent runs through the durable loop. This milestone proves the
*other* half of the platform's thesis: that the agent HTTP/SSE contract is a
universal adapter, so an agent written in a **foreign SDK/language** can be hosted by
`runtimed` with no platform changes specific to that SDK.

---

## 1. Goal

Host a **non-Go agent** — the OpenAI Agents SDK agent that already exists at
`../agents_sdk/openai-demo` — as a first-class, supervised agent under `runtimed`:
registered in config, spawned + restarted by the supervisor, health-gated, routed at
`/agents/{id}/…`, authenticated, visible in `/ui`, driven by `runtimectl`, and
**validated by the same conformance suite** that validates Go agents.

Two reusable deliverables:
1. **Platform:** a *generalized spawn path* so the supervisor can launch an arbitrary
   command (e.g. `python main.py`), not only the `agentd` binary.
2. **Shim:** a *reusable Python contract-server library* + a thin *OpenAI-SDK adapter*
   on top, living in `contrib/shims/python/`.

**Durability scope: Level 1 only** — conversation resume across a process restart.
Level 2 (in-flight crash resume) is explicitly out of scope and deferred (§9).

## 2. Why this shape

- **The contract is the seam.** Routing (`reverseProxy`), supervision, auth, `/ui`,
  `runtimectl`, and conformance already operate on the wire contract, not on Go
  types. The control plane strips `/agents/{id}` and proxies the remainder to the
  agent's `addr` (`controlplane/api.go`), so a foreign agent only has to serve the
  bare contract paths on `RUNTIME_LISTEN_ADDR` — exactly what `agentd` does.
- **Only one platform gap exists:** the supervisor today always execs the `agentd`
  binary (`controlplane/proxy.go` `SpawnFunc`). Generalizing that to an arbitrary
  command is the single platform change needed.
- **Library + adapter split** so the contract work (endpoints, SSE, `?since=N`,
  persistence) is written once and every future framework (PydanticAI, CrewAI,
  LangGraph, …) is a small adapter, per ROADMAP §C1.

## 3. The contract a foreign agent must satisfy

(From `agentruntime/server.go` + validated by `conformance/conformance.go`.) Served
on `RUNTIME_LISTEN_ADDR`, bare paths (the control plane adds/strips the `/agents/{id}`
prefix):

| Method + path | Behavior |
|---|---|
| `GET /healthz` | 200 `ok` |
| `GET /meta` | JSON `{"agent_id": "...", "contract_version": "v1"}` |
| `POST /sessions` | body `{"message": str, "image_b64"?: str, "image_mime"?: str}` → `{"session_id": str}`; starts the run asynchronously |
| `GET /sessions/{id}/stream?since=N` | `Content-Type: text/event-stream`; replays stored events with `seq > N`, then live; each SSE record has an `id: <seq>` line and a `data: <json>` line; terminates after a `done` or `error` event |
| `GET /sessions/{id}` | JSON `{"id","status","turn_count"}` |
| `GET /sessions` | JSON array of `{"id","status","turn_count"}` |

**Wire event JSON** (matches `agentruntime.WireEvent`): `{"type": "text"|"tool_result"|"done"|"error", "text"?: str, "error"?: str}`. The shim emits the
same vocabulary so existing clients (the console, `runtimectl`) render it unchanged.

## 4. Work-stream A — Platform: generalized spawn path (Go)

Make the supervisor able to launch any command. Backward compatible: an agent with no
`command` behaves exactly as today.

### Files
- `internal/config/config.go` — `AgentConfig` gains `Command []string \`yaml:"command"\``
  (optional). `Validate` unchanged except: an entry is valid if it has the existing
  required fields; `command` is optional and free-form. (`kind` and `command` are
  independent; `command` agents typically set neither `kind` nor rely on the marker
  table.)
- `controlplane/proxy.go` — `AgentProcess` gains `Command []string`. `SpawnFunc`:
  - If `len(a.Command) > 0`: `exec.CommandContext(ctx, a.Command[0], a.Command[1:]...)`.
  - Else: today's `exec.CommandContext(ctx, a.BinPath)`.
  - **Env in both cases:** keep injecting `RUNTIME_PG_DSN`, `RUNTIME_LISTEN_ADDR`,
    `RUNTIME_AGENT_ID`, `RUNTIME_AGENT_KIND`, on top of `os.Environ()` (so the shim
    inherits `OPENAI_API_KEY`/`OPENAI_BASE_URL`/`OPENAI_MODEL` from runtimed's env).
    The shim only needs `RUNTIME_LISTEN_ADDR` (+ its own envs); the extra `RUNTIME_*`
    vars are harmless to a process that ignores them.
  - **Working directory:** add `WorkDir string` to `AgentProcess`/`AgentConfig`
    (`yaml:"workdir"`, optional). When set, `cmd.Dir = workDir`. Needed so
    `python main.py` / `uv run` resolves the shim's project. Absolute, or resolved
    relative to runtimed's cwd; the spec uses absolute in examples.
  - stdout/stderr inherit as today (so the shim's logs interleave in runtimed's
    output).
- `controlplane/registry.go` — `NewRegistry` copies `a.Command` and `a.WorkDir` into
  `AgentProcess`.

### Determinism / safety
- No behavioral change when `command` is absent — existing integration tests
  (multiagent/resume/operability/nutrition) must stay green; they exercise the
  `agentd` path.
- The supervisor's restart/backoff and the `/healthz` readiness gate work unchanged:
  they only care that *something* binds the addr and answers `/healthz`.

### Go testing (hermetic — must NOT depend on Python)
- A unit test for `SpawnFunc` with `Command` set to a **fake** command that binds the
  addr and serves `/healthz` — e.g. a tiny Go helper built into a temp binary, or a
  shell one-liner. Asserts the supervised process comes up healthy and the env/workdir
  are applied. The real Python shim is NEVER invoked from `go test`.
- A `config` test: `command:`/`workdir:` round-trip through YAML.

## 5. Work-stream B — Shim: `contrib/shims/python/`

A reusable contract server + an OpenAI-SDK adapter.

### Layout
```
contrib/shims/python/
  pyproject.toml                  # uv project; deps: fastapi, uvicorn, openai-agents, ...
  README.md                       # how to run + how to add an adapter
  runtime_contract/
    __init__.py
    app.py                        # FastAPI app: the 6 endpoints, binds RUNTIME_LISTEN_ADDR
    sse.py                        # SSE framing: id:<seq>\ndata:<json>\n\n
    store.py                      # SQLite session+event store (Level-1 persistence)
    events.py                     # ContractEvent dataclasses + JSON shape
    adapter.py                    # AgentAdapter Protocol
  adapters/
    __init__.py
    openai_agents.py              # OpenAI Agents SDK adapter (uses SQLiteSession)
  main.py                         # wire library + chosen adapter; serve
  tests/
    test_contract.py             # hermetic: SSE framing, ?since=N replay, store, with a fake adapter
  runtime.openai-shim.yaml        # example runtimed config using command:/workdir:
```

### 5.1 `runtime_contract` — the reusable library (framework-agnostic)

- **`events.py`** — `ContractEvent` = `{type: "text"|"tool_result"|"done"|"error",
  text?: str, error?: str}` with a `to_json()` matching `WireEvent`.
- **`adapter.py`** — the seam every framework implements:
  ```python
  class AgentAdapter(Protocol):
      async def run(self, session_id: str, message: str,
                    images: list[Image], history: Sequence[ContractEvent]
                    ) -> AsyncIterator[ContractEvent]: ...
  ```
  `Image = {mime: str, data: bytes}`. The library decodes `image_b64` → `Image`
  before calling the adapter. The adapter yields contract events; the library frames,
  persists, and fans them out. The adapter never touches HTTP/SSE/SQLite.
- **`store.py`** — SQLite at `RUNTIME_SHIM_DB` (default `./shim.db`). Two tables:
  - `sessions(id TEXT PK, status TEXT, turn_count INT, created_at)`.
  - `events(session_id TEXT, seq INT, payload TEXT, PRIMARY KEY(session_id, seq))`.
  - API: `create_session() -> id`; `append_event(id, event) -> seq`;
    `events_since(id, after_seq) -> [(seq, event)]`; `get_session(id)`;
    `list_sessions()`; `set_status(id, status)`; `set_turn_count(id, n)`.
  - This is **Level-1 durability**: after a process restart, sessions + their event
    logs are still on disk, so `GET /sessions`, `GET /sessions/{id}`, and
    `/stream?since=N` all work against past sessions. Seq is per-session, assigned by
    `append_event` (single writer per session — the run task — so a simple
    `MAX(seq)+1` is safe, mirroring the Go store's documented assumption).
- **`sse.py`** — `format(seq, event) -> "id: {seq}\ndata: {json}\n\n"`. Helper to
  stream: replay `events_since` first, flush, then subscribe to live events for that
  session until a terminal (`done`/`error`).
- **`app.py`** — FastAPI:
  - `GET /healthz` → `ok`.
  - `GET /meta` → `{"agent_id": AGENT_ID, "contract_version": "v1"}` (AGENT_ID from
    `RUNTIME_AGENT_ID` env, else a configured name).
  - `POST /sessions` → create session row; decode optional image; launch the run as a
    background task (`asyncio.create_task`) that drives the adapter and appends each
    yielded event to the store + an in-memory live queue; return `{"session_id"}`
    immediately.
  - `GET /sessions/{id}/stream?since=N` → `StreamingResponse(media_type=
    "text/event-stream")`: replay stored events with `seq > N`, then live queue until
    terminal. (If the session already finished, replay alone yields the terminal
    event and the stream closes — matches the Go server's pure-replay behavior.)
  - `GET /sessions/{id}` and `GET /sessions` → from the store.
  - Binds `host=0.0.0.0` (or 127.0.0.1) `port` parsed from `RUNTIME_LISTEN_ADDR`
    (format `host:port`).
  - **Turn accounting:** increment `turn_count` once per completed run for Level-1
    (the SDK loop is opaque; we count runs, not internal turns). `status`:
    `running` → `completed` (clean) / `error`. Good enough for the contract +
    conformance, which only assert presence of `status`.

### 5.2 `adapters/openai_agents.py` — the OpenAI Agents SDK adapter

- Builds the agent by reusing the existing `openai-demo` agent definition (tools,
  prompt, `output_type`). Import from the demo package if importable, else vendor a
  minimal `build_agent()`; the spec prefers importing to avoid drift (document the
  PYTHONPATH/dependency so `openai-demo` is importable, or copy `build_agent` +
  tools — decide in the plan; importing is cleaner).
- `run(...)`:
  - Construct input: text + any images as the SDK's content-list (`input_text` +
    `input_image` data URL) — same shape the demo uses.
  - Use **`SQLiteSession(session_id, db_path)`** as the SDK's own conversation memory
    (Level-1: the *model-visible* history persists and is keyed by our session id).
  - `Runner.run_streamed(agent, input, session=sqlite_session)`; iterate
    `result.stream_events()`:
    - text deltas → `ContractEvent(type="text", text=delta)` (or one text event with
      the final output; streaming deltas preferred for live feel).
    - tool call results → `ContractEvent(type="tool_result", text=summary)`.
    - on completion → the adapter returns (the library emits the terminal `done`).
    - on exception → `ContractEvent(type="error", error=str(e))` then return (library
      emits terminal `error`); never raise out of `run` (don't crash the server).
  - For the typed `NutritionVerdict` output: serialize to readable text in the final
    `text` event (the contract is text/SSE; structured output becomes prose/JSON in
    the stream — same fidelity note as the Go nutrition port).
- The adapter does NOT persist the contract event log (the library does) — it only
  uses `SQLiteSession` for the SDK's own memory. Two stores, two purposes:
  `SQLiteSession` = model conversation; `store.py` = contract event log + session
  metadata. (A later refactor could unify; kept separate now for clarity.)

### 5.3 `main.py`
Reads env, instantiates the OpenAI adapter, mounts it on the library app, runs uvicorn
on `RUNTIME_LISTEN_ADDR`. `python main.py` is the spawn target.

### 5.4 Example config `runtime.openai-shim.yaml`
```yaml
# Host the OpenAI Agents SDK agent via the Python contract shim.
# Requires env (inherited by the shim): OPENAI_API_KEY, OPENAI_BASE_URL, OPENAI_MODEL.
agents:
  - id: openai
    name: OpenAI SDK Nutrition Agent
    model: openai/gpt-5.4
    listen_addr: 127.0.0.1:8301
    workdir: /ABS/PATH/runtime/contrib/shims/python
    command: ["uv", "run", "python", "main.py"]
```

## 6. How it runs end-to-end

1. `runtimed` loads `runtime.openai-shim.yaml`, sees `command:` → supervises
   `uv run python main.py` in `workdir`, with `RUNTIME_LISTEN_ADDR=127.0.0.1:8301`
   injected and `OPENAI_*` inherited.
2. Shim binds `:8301`, serves the contract; runtimed health-gates `/healthz`.
3. `runtimectl invoke --agent openai "Investigate: …"` → control plane proxies
   `POST /agents/openai/sessions` → shim starts the OpenAI SDK run → streams
   `text`/`tool_result`/`done` back through the proxy as SSE.
4. Restart `runtimed` (or the shim): past sessions still listable/streamable from
   `shim.db` (Level-1). A *new* invoke continues the SDK conversation via
   `SQLiteSession`.

## 7. Testing strategy

- **Conformance is the acceptance gate.** `runtimectl conformance --agent openai`
  (the Go suite in `conformance/`) MUST pass against the running shim. This is the
  primary proof the foreign agent satisfies the contract. Documented as a manual/CI
  step (it needs the shim process up).
- **Go hermetic:** the spawn-path unit test (fake command, §4) + the config
  round-trip test. `go test ./...` stays Python-free and green. Existing integration
  suite stays green (no `command` ⇒ unchanged).
- **Python hermetic (`contrib/shims/python/tests/`):** with a **fake adapter**
  (yields scripted events, no network): SSE framing format; `?since=N` replay
  (post events, reconnect with since=k, assert only seq>k returned); store
  create/append/list/status; terminal-event stream closure. Run via
  `uv run pytest`; NOT part of `go test`.
- **Manual E2E (documented in the shim README):** boot `runtimed` with the shim
  config + real `OPENAI_*`; `runtimectl invoke` → verdict; `runtimectl conformance`
  → pass; restart → `runtimectl sessions --agent openai` still lists the session and
  `/stream?since=0` replays it (Level-1 proof).
- **Live:** the real openai-demo agent answering a nutrition label through the shim
  (needs the proxy key; same creds as the Go nutrition example).

## 8. Success criteria

- `go build ./...`, `go vet ./...`, `go test ./...` green (Python-free).
- Existing integration suite green (spawn-path change is backward compatible).
- `contrib/shims/python` `uv run pytest` green (hermetic contract-lib tests).
- With the shim running under `runtimed`: `runtimectl conformance --agent openai`
  passes, `runtimectl invoke --agent openai` returns a verdict over SSE, and after a
  restart the session is still listable + replayable (Level-1 durability).
- The library/adapter split is clean enough that "add a framework" = write one new
  file in `adapters/` (documented in the README with a stub example).

## 9. Out of scope (explicit)

- **Level-2 crash resume.** A run killed mid-execution is lost; no whole-run
  checkpoint/recovery, no DBOS on the Python side. This is the documented next
  milestone (ROADMAP §C1 Level 2): wrap the run as a durable step (DBOS-Python in
  the shim, or a Go external-kind `agentd` driving the shim), with idempotent tools.
- **Other frameworks.** The library is *shaped* for them (the `AgentAdapter` seam);
  each is a later adapter. PydanticAI+DBOS deep integration is its own future spec.
- **Containerization / K8s** (ROADMAP §C2).
- **Auth between control plane and shim.** The shim trusts the loopback proxy (same
  trust model as `agentd` today). Platform auth still applies at the control-plane
  edge.

## 10. Risks & mitigations

- **`go test` accidentally depending on Python** → the spawn-path tests use a fake
  command only; Python tests live under `contrib/` outside the Go module's test run.
- **`uv`/venv resolution at spawn** → pin `workdir` + `command: ["uv","run",...]`;
  document the one-time `uv sync` in the shim README; runtimed surfaces a clear
  supervisor error if the command fails to start.
- **openai-demo import/version drift** → prefer importing the demo's `build_agent`;
  if that couples too tightly, vendor a minimal copy and note it. Decide in the plan.
- **SSE flushing through two hops** (uvicorn → Go `reverseProxy` with
  `FlushInterval=-1`) → the proxy already flushes immediately; ensure uvicorn streams
  unbuffered (FastAPI `StreamingResponse` does). Validate in the manual E2E.
- **Port collisions** with other example configs → the shim uses `:8301`
  (nutrition uses `:8201`, test agents `:81xx`).
