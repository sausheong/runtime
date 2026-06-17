# `runtime_contract` — the Python contract library for hosting foreign-SDK agents

Runtime normally hosts **Go** agents: you link the `agentruntime` SDK and it
binds the HTTP/SSE agent contract for you. This directory is the **Python
equivalent** — a reusable, framework-agnostic library (`runtime_contract`) that
serves that same agent contract so a process built with *any* Python agent
framework can be hosted by the *same* control plane, unchanged. The agent
contract (the six HTTP/SSE endpoints) is the only interface Runtime cares about,
so any process that speaks it can be supervised, routed, health-gated, and
restarted like a native agent.

> This is the reusable contract **library**. Two complete worked agents are
> hosted on it — the same SG Nutrition Investigator on two frameworks:
> [`examples/nutrition-label-openai`](../../../examples/nutrition-label-openai)
> (OpenAI Agents SDK) and
> [`examples/nutrition-label-claude`](../../../examples/nutrition-label-claude)
> (Claude Agent SDK) — `make run` in either boots it under `runtimed`.

Runtime hosts a library consumer through the generalized `command:`/`workdir:`
config fields: when an agent entry sets `command`, `runtimed`'s supervisor execs
that argv in `workdir` instead of the bundled `agentd` binary, injecting
`RUNTIME_LISTEN_ADDR`, `RUNTIME_AGENT_ID`, etc., and inheriting the parent
environment (so the framework's own credentials, e.g. `OPENAI_*`, flow through).

---

## Architecture

The library is two layers:

- **`runtime_contract/`** — a reusable, framework-agnostic library that serves
  the contract. It is a FastAPI app exposing the six contract endpoints
  (`/healthz`, `/meta`, `POST /sessions`, `GET /sessions/{id}/stream?since=N`,
  `GET /sessions/{id}`, `GET /sessions`) plus one shim extension —
  `POST /sessions/{id}/messages` for follow-up turns on an existing session
  (not yet in the Go agent contract) — frames events as SSE
  (`id: <seq>\ndata: <compact-json>\n\n`), replays buffered events on
  `?since=N`, and persists sessions + an append-only event log to a SQLite store
  (`shim.db`) so they survive a restart. It also serves Prometheus `/metrics`
  (turns, turn duration, tokens, tool calls) wire-compatible with the Go
  `agentruntime` emitter, so the control plane fan-out scrape picks it up
  automatically. It knows nothing about any agent framework.
- **A thin per-framework adapter** — a small object implementing the
  `AgentAdapter` protocol that drives the actual SDK and translates its stream
  into contract events. The adapter lives with the consumer, not in this library;
  see the example for a working one (`NutritionAdapter`).

A consumer's entrypoint just builds the adapter and calls `serve()`. The helper
reads the **operator parameters** from the environment the control plane injects
— it is the Python analog of the Go `agentruntime.Serve`, which likewise reads
`RUNTIME_PG_DSN`/`RUNTIME_LISTEN_ADDR` from env rather than from the agent
author. The adapter author never handles them:

| Env var | Meaning | Source |
|---|---|---|
| `RUNTIME_LISTEN_ADDR` | `host:port` to bind (required) | injected by `runtimed` |
| `RUNTIME_AGENT_ID` | agent id surfaced on `/meta` (default `agent`) | injected by `runtimed` |
| `RUNTIME_SHIM_DB` | SQLite path for the durable store (default `./shim.db`) | optional; *not* injected — defaults under the agent's workdir |

`serve()` resolves those, builds the `Store` and the FastAPI app, and runs
uvicorn. The Runtime config's `command:` points at the entrypoint (e.g.
`uv run python serve.py`):

```python
from runtime_contract import serve
from adapter import MyFrameworkAdapter

serve(MyFrameworkAdapter)   # reads RUNTIME_* from env; builds Store + app + uvicorn
```

`serve(adapter)` accepts either a ready adapter instance or a **factory**
`make(db_path) -> AgentAdapter`. A class whose constructor takes the db path
(e.g. `MyFrameworkAdapter(db_path)`) is itself a factory, so `serve(MyClass)`
works directly — and lets the adapter key its own per-session store (e.g. an
SDK's `SQLiteSession`) on the same `RUNTIME_SHIM_DB` the contract store uses.
(The lower-level `create_app(adapter, store, agent_id)` and `Store` remain
exported if you need to assemble the app yourself.)

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

Then point your entrypoint at the new adapter (`serve(MyFrameworkAdapter)`). The
library handles persistence, SSE fan-out, `?since=N` replay, and the terminal
event.

### Metrics (optional telemetry)

The library records one turn per invocation — count, outcome, and wall-clock
duration — automatically. To also surface **token usage** and **tool calls**, an
adapter may yield two extra telemetry events. They feed Prometheus only; they are
**never** sent to the client SSE stream or persisted:

```python
# one per tool the framework invoked this turn
yield ContractEvent(type="tool_call", tool="check_additive")
# token counts for the turn (yield at most one; the last wins)
yield ContractEvent(type="usage", usage={"input": 1234, "output": 567,
                                          "cache_creation": 0, "cache_read": 0})
```

These become `agent_tool_calls_total{tool=...}` and
`agent_tokens_total{direction=...}` on `/metrics`. An adapter that yields neither
still reports turn count and duration. See the OpenAI and Claude SDK examples for
how to extract these from each SDK's run result.

---

## Prerequisites

- [`uv`](https://docs.astral.sh/uv/) (installs Python + deps from `pyproject.toml`).

Install dependencies:

```bash
cd contrib/shims/python
uv sync
```

The agent's own runtime environment (model credentials, base URLs, etc.) is the
consumer's concern — it flows through from `runtimed` to the supervised
subprocess. See the example for a concrete `.env` setup.

---

## Run it

This directory is a library, not a runnable server — there is no entrypoint here
to start. To see it hosting a real agent, use the worked example, which provides
the adapter, a `serve.py` entrypoint, and a `Makefile` that boots everything
under `runtimed`:

```bash
cd ../../../examples/nutrition-label-openai
cp .env.example .env          # fill in your model/proxy credentials
make run                      # builds binaries, uv sync, runs the control plane
# in a second shell:
make conformance              # the same conformance gate that gates Go agents
make demo-image IMAGE=milo.jpeg
```

To wire your own consumer, follow the same shape: an entrypoint that builds an
adapter and calls `serve(adapter)`, plus a Runtime config whose `command:` points
at it.

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

**Sessions and events survive a restart.** The library persists, in `shim.db`
(path overridable via `RUNTIME_SHIM_DB`):

- the **session list** and per-session **status / turn count**, and
- an **append-only event log** per session.

So after a restart, prior sessions remain **listable** (`GET /sessions`) and
their events are **replayable** (`GET /sessions/{id}/stream?since=N`).
Conversation memory can also continue across restarts when an adapter keys its
own per-session store (e.g. an SDK's `SQLiteSession`) on the runtime session id,
so a follow-up turn sees the prior turns.

**A run killed mid-execution is not resumed.** There is no checkpoint/replay of
a partially completed run, unlike Go agents' DBOS-backed per-turn durability — a
process killed during a run loses that in-flight turn (its prior sessions and
completed events remain intact).

---

## A note on trust

A library consumer trusts the **loopback proxy** — it expects to be reached only
by `runtimed` over `127.0.0.1` and does not authenticate requests itself. This is
the same model as the Go `agentd` agents: platform authentication is enforced at
the **control-plane edge** (the bearer tokens in `runtime.yaml`), not at each
agent subprocess.
