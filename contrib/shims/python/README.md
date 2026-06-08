# Python contract shim — hosting a foreign-SDK agent under Runtime

Runtime normally hosts **Go** agents: you link the `agentruntime` SDK and it
binds the HTTP/SSE agent contract for you. This shim lets a **foreign-SDK** agent
— here, one built with the [OpenAI Agents SDK](https://github.com/openai/openai-agents-python)
— be hosted by the *same* control plane, unchanged. The agent contract (the six
HTTP/SSE endpoints) is the only interface Runtime cares about, so any process
that speaks it can be supervised, routed, health-gated, and restarted like a
native agent.

Runtime hosts this shim through the generalized `command:`/`workdir:` config
fields (see [`runtime.openai-shim.yaml`](runtime.openai-shim.yaml)): when an
agent entry sets `command`, `runtimed`'s supervisor execs that argv in `workdir`
instead of the bundled `agentd` binary, injecting `RUNTIME_LISTEN_ADDR`,
`RUNTIME_AGENT_ID`, etc., and inheriting the parent environment (so `OPENAI_*`
flows through).

---

## Architecture

The shim is two layers:

- **`runtime_contract/`** — a reusable, framework-agnostic library that serves
  the contract. It is a FastAPI app exposing the six endpoints (`/healthz`,
  `/meta`, `POST /sessions`, `GET /sessions/{id}/stream?since=N`,
  `GET /sessions/{id}`, `GET /sessions`), frames events as SSE
  (`id: <seq>\ndata: <compact-json>\n\n`), replays buffered events on
  `?since=N`, and persists sessions + an append-only event log to a SQLite store
  (`shim.db`) for Level-1 durability. It knows nothing about any agent framework.
- **`adapters/openai_agents.py`** — a thin per-framework adapter that drives the
  actual SDK and translates its stream into contract events.

Adding support for another framework is **one new file** implementing the
`AgentAdapter` protocol (`runtime_contract/adapter.py`):

```python
from typing import AsyncIterator, Sequence
from runtime_contract.events import ContractEvent, Image

class MyFrameworkAdapter:
    async def run(
        self,
        session_id: str,
        message: str,
        images: Sequence[Image],
        history: Sequence[ContractEvent],
    ) -> AsyncIterator[ContractEvent]:
        # Drive your framework; yield ContractEvent(type="text", text=...) as
        # output streams. Never raise — surface failures as
        # ContractEvent(type="error", error=...). Do NOT emit the terminal
        # 'done'/'error' lifecycle event; the library appends it for you.
        yield ContractEvent(type="text", text="hello")
```

Then point `main.py` (or your own entrypoint) at the new adapter. The library
handles persistence, SSE fan-out, `?since=N` replay, and the terminal event.

---

## Prerequisites

- [`uv`](https://docs.astral.sh/uv/) (installs Python + deps from `pyproject.toml`).
- Environment for the agent:
  - `OPENAI_API_KEY` (required)
  - `OPENAI_BASE_URL` (optional; e.g. a LiteLLM proxy base URL)
  - `OPENAI_MODEL` (defaults to `gpt-4o` if unset)

Install dependencies:

```bash
cd contrib/shims/python
uv sync
```

---

## Run standalone (for development)

The shim is a normal HTTP server; you can run it without Runtime to develop the
adapter. It serves the contract endpoints **unprefixed** (no `/agents/{id}`
prefix — that prefix is added by the control-plane proxy):

```bash
RUNTIME_LISTEN_ADDR=127.0.0.1:8301 \
OPENAI_API_KEY=... OPENAI_BASE_URL=... OPENAI_MODEL=... \
uv run python main.py
```

Then drive it directly:

```bash
curl localhost:8301/healthz
# ok

# Create a session (returns {"session_id": "..."}):
curl -s localhost:8301/sessions \
  -d '{"message":"Investigate: ... Sugar 11g/100ml. Beverage."}'
# {"session_id":"..."}

# Stream it (replays from the start with since=0, then live to a terminal event):
curl -N "localhost:8301/sessions/<id>/stream?since=0"
# id: 1
# data: {"type":"text","text":"…the verdict…"}
# id: 2
# data: {"type":"done"}
```

`POST /sessions` also accepts optional `image_b64` / `image_mime` fields for
vision input, mirroring the Go agents' contract.

---

## Run under `runtimed` (the real integration)

This is the path that proves the shim is a first-class Runtime agent. From the
**repo root**:

```bash
# 1. Build the platform binaries (./bin/{agentd,runtimed,runtimectl}).
make build

# 2. Export the agent's environment (runtimed inherits and passes it down).
export OPENAI_API_KEY=...
export OPENAI_BASE_URL=https://litellm-stg.aip.gov.sg
export OPENAI_MODEL=gpt-5.4

# 3. Run the control plane with this config. runtimed execs `uv run python main.py`
#    in contrib/shims/python (workdir) and supervises it like any agent.
RUNTIME_CONFIG=contrib/shims/python/runtime.openai-shim.yaml ./bin/runtimed
# control plane on :8080 hosting 1 agent
# supervising agent "openai" at 127.0.0.1:8301
```

Then, from another shell, validate and drive it through the control plane:

```bash
# Acceptance gate: the same conformance suite that gates Go agents.
./bin/runtimectl conformance --agent openai
# conformance: PASSED

# Invoke and stream a verdict:
./bin/runtimectl invoke --agent openai \
  "Investigate: ... Sugar 11g/100ml. Beverage."
# session: ...
# data: {"type":"text","text":"…the verdict…"}
# data: {"type":"done"}

./bin/runtimectl sessions --agent openai
# ...   completed   turns=1
```

`--agent openai` may be omitted when `openai` is the only registered agent.

---

## Tests

```bash
cd contrib/shims/python
uv run pytest
```

The tests are hermetic (they exercise the contract library + store with a stub
adapter; no API key or network required).

---

## Durability

**Level 1 is implemented.** The shim persists, in `shim.db` (path overridable
via `RUNTIME_SHIM_DB`):

- the **session list** and per-session **status / turn count**, and
- an **append-only event log** per session.

So after a restart, prior sessions remain **listable** (`GET /sessions`) and
their events are **replayable** (`GET /sessions/{id}/stream?since=N`).
Conversation memory also continues across restarts: the OpenAI adapter keys an
`SQLiteSession` (in the same db) on the runtime session id, so a follow-up turn
sees the prior turns.

**Level 2 (in-flight crash resume) is NOT implemented.** A run that is killed
*mid-execution* is lost — there is no checkpoint/replay of a partially completed
run, unlike Go agents' DBOS-backed per-turn durability. This is documented as
future work in the repo `ROADMAP.md` (§C1, Level 2).

---

## A note on trust

The shim trusts the **loopback proxy** — it expects to be reached only by
`runtimed` over `127.0.0.1` and does not authenticate requests itself. This is
the same model as the Go `agentd` agents: platform authentication is enforced at
the **control-plane edge** (the bearer tokens in `runtime.yaml`), not at each
agent subprocess.
