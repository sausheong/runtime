# Host `nutrition-label-openai` on Runtime — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Run the full `examples/nutrition-label-openai` agent (4 tools, SFA data, typed `NutritionVerdict`, cross-run memory) as a first-class agent hosted by `runtimed`, reusing the `runtime_contract` library and rendering the validated verdict as prose over the HTTP/SSE contract.

**Architecture:** Promote `runtime_contract` to a standalone path-installable package. Extract the example's agent into `agent.py` (importable, lazy env, pure `render_verdict()`); keep `main.py` as a thin CLI. Add `adapter.py` (`NutritionAdapter`) + `serve.py` (entrypoint) that reuse the library. A `Makefile` + config yaml run it under `runtimed`, mirroring `examples/nutrition-label-go`. Remove the shim's stripped-down OpenAI stand-in.

**Tech Stack:** Python 3.12, `uv`, FastAPI/uvicorn (contract library), OpenAI Agents SDK, Pydantic, SQLite (Level-1 durability), Go control plane (unchanged).

**Spec:** `docs/superpowers/specs/2026-06-08-nutrition-openai-on-runtime-design.md`

---

## File structure

```
contrib/shims/python/
  pyproject.toml        MODIFY  rename → runtime-contract; hatchling build backend;
                                drop openai-agents dep (lib is framework-agnostic)
  runtime_contract/     (unchanged code)
  tests/                (unchanged — hermetic, stub adapter)
  README.md             MODIFY  "the contract library; see the example for a full agent"
  main.py               DELETE  stand-in entrypoint
  adapters/             DELETE  stand-in OpenAI adapter (openai_agents.py, __init__.py)
  runtime.openai-shim.yaml  DELETE

examples/nutrition-label-openai/
  agent.py              CREATE  agent definition: build_agent(), 4 tools, SFA loader,
                                memory helpers, NutritionVerdict/Finding, INSTRUCTIONS,
                                render_verdict(), remember_verdict()
  main.py               MODIFY  thin CLI importing agent.py (behaviour unchanged)
  adapter.py            CREATE  NutritionAdapter(AgentAdapter)
  serve.py              CREATE  entrypoint: Store + create_app + uvicorn
  pyproject.toml        MODIFY  + fastapi/uvicorn + runtime-contract path dep
  Makefile              CREATE  run / demo-text / demo-image / sessions / check-env / clean
  runtime.nutrition-openai.yaml  CREATE  command/workdir agent entry, port 8302
  README.md             MODIFY  add "Run under runtimed" section
  tests/test_render.py  CREATE  hermetic unit test for render_verdict()
  sfa_additives.json, milo.jpeg, .env.example  (reused in place)
```

No `.gitignore` change: root already ignores `.env`, `agent_memory.json`, `*.db`, `.venv/`, `__pycache__`, `.pytest_cache`.

---

## Task 1: Repackage `runtime_contract` as a standalone installable package; remove the stand-in

**Files:**
- Modify: `contrib/shims/python/pyproject.toml`
- Delete: `contrib/shims/python/main.py`, `contrib/shims/python/adapters/openai_agents.py`, `contrib/shims/python/adapters/__init__.py`, `contrib/shims/python/runtime.openai-shim.yaml`

- [ ] **Step 1: Confirm the library tests do not reference the stand-in**

Run: `cd contrib/shims/python && grep -rn "adapters" tests/ runtime_contract/`
Expected: only `runtime_contract/app.py` and `__init__.py` import `.adapter` (the protocol). No reference to the top-level `adapters/` package. (If `tests/` references `adapters`, STOP and report.)

- [ ] **Step 2: Rewrite `contrib/shims/python/pyproject.toml`**

Replace the entire file with:

```toml
[project]
name = "runtime-contract"
version = "0.1.0"
description = "Framework-agnostic HTTP/SSE agent-contract server for hosting foreign-SDK agents under sausheong/runtime"
requires-python = ">=3.12"
dependencies = [
    "fastapi>=0.115",
    "uvicorn>=0.30",
]

[dependency-groups]
dev = ["pytest>=8", "httpx>=0.27"]

[build-system]
requires = ["hatchling"]
build-backend = "hatchling.build"

[tool.hatch.build.targets.wheel]
packages = ["runtime_contract"]

[tool.pytest.ini_options]
testpaths = ["tests"]
```

Rationale: `name` becomes `runtime-contract` (what the example will depend on). `openai-agents` is dropped — the library is framework-agnostic; the SDK belongs to the example. The `[build-system]` + `[tool.hatch.build.targets.wheel]` make it installable as a path dependency exposing only the `runtime_contract` package (not `tests/`). `httpx` stays a dev dep (FastAPI's `TestClient` needs it).

- [ ] **Step 3: Delete the stand-in files**

```bash
cd contrib/shims/python
git rm main.py runtime.openai-shim.yaml adapters/openai_agents.py adapters/__init__.py
rmdir adapters 2>/dev/null || true
```

- [ ] **Step 4: Verify the library still builds and tests pass (regression)**

Run: `cd contrib/shims/python && uv sync && uv run pytest -q`
Expected: all tests in `tests/test_contract.py` and `tests/test_store.py` PASS. (`uv sync` re-resolves after the pyproject change; the package now builds via hatchling.)

- [ ] **Step 5: Commit**

```bash
cd /Users/sausheong/projects/runtime
git add contrib/shims/python/pyproject.toml
git commit -m "refactor(shim): make runtime_contract a standalone installable package; remove OpenAI stand-in

Library is now framework-agnostic (drops openai-agents dep) and builds as
a path-installable wheel exposing only runtime_contract. The stripped-down
OpenAI stand-in (main.py, adapters/, runtime.openai-shim.yaml) is removed;
the full example becomes the one true OpenAI demo.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 2: Extract `agent.py` from `main.py` with a pure `render_verdict()` (TDD)

**Files:**
- Create: `examples/nutrition-label-openai/agent.py`
- Create: `examples/nutrition-label-openai/tests/__init__.py` (empty), `examples/nutrition-label-openai/tests/test_render.py`
- Modify: `examples/nutrition-label-openai/main.py`

The agent definition currently lives in `main.py` and cannot be imported (it reads `os.environ["OPENAI_API_KEY"]` and calls `load_dotenv` at module top level). We move the agent into `agent.py` with **lazy** env reading, expose a pure `render_verdict()` and `remember_verdict()`, and reduce `main.py` to a CLI.

- [ ] **Step 1: Write the failing test for `render_verdict()`**

Create `examples/nutrition-label-openai/tests/__init__.py` as an empty file, then create `examples/nutrition-label-openai/tests/test_render.py`:

```python
"""Hermetic unit test for render_verdict — no API key or network."""
from agent import NutritionVerdict, Finding, render_verdict


def _verdict() -> NutritionVerdict:
    return NutritionVerdict(
        reasoning="Read the label; classified each additive.",
        product_name="Milo UHT",
        summary="A chocolate malt beverage, Nutri-Grade C.",
        green=[Finding(category="GREEN", finding="Soy lecithin (E322) permitted", source="check_sfa_additive")],
        amber=[Finding(category="AMBER", finding="Moderate sugar", source="label")],
        red=[Finding(category="RED", finding="High saturated fat", source="label")],
        recommendation="Okay in moderation.",
    )


def test_render_includes_all_sections():
    text = render_verdict(_verdict())
    assert "Product: Milo UHT" in text
    assert "Reasoning: Read the label" in text
    assert "Summary: A chocolate malt beverage" in text
    assert "🟢 Soy lecithin (E322) permitted  [check_sfa_additive]" in text
    assert "🟡 Moderate sugar  [label]" in text
    assert "🔴 High saturated fat  [label]" in text
    assert "Recommendation: Okay in moderation." in text
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd examples/nutrition-label-openai && uv run pytest tests/test_render.py -q`
Expected: FAIL — `ModuleNotFoundError: No module named 'agent'` (agent.py does not exist yet).

- [ ] **Step 3: Create `agent.py` by moving the agent definition out of `main.py`**

Create `examples/nutrition-label-openai/agent.py`. Move these sections **verbatim** from the current `main.py` (they are already-tested code; relocate, do not rewrite):
- The module docstring may be a short new one (see below).
- Imports needed by the moved code: `os, re, json, base64, mimetypes, httpx`, `from pathlib import Path`, `from pydantic import BaseModel`, and from `agents`: `Agent, Runner, function_tool, AsyncOpenAI, OpenAIChatCompletionsModel, set_tracing_disabled`. (Do NOT move `sys`, `asyncio`, or `load_dotenv` — those stay in `main.py`.)
- `HCS_RESOURCE_ID`, `set_tracing_disabled(True)`, `_ADDITIVES` loader, `_norm`, the `_BY_E`/`_BY_ALIAS` index-building loop, `COLLOQUIAL`, `CONSUMER_NOTES`, `_MEMORY_PATH`, `_load_memory`, `_save_memory`, `_MEMORY`, `_resolve_additive`, `_format_entry`, the four `@function_tool` functions (`check_sfa_additive`, `recall_product`, `check_hcs`, `calculate_nutri_grade`), `_safe_json`, `_data_url`, `Finding`, `NutritionVerdict`, `INSTRUCTIONS`.

Use this docstring and **replace** the old `build_agent()` (which read module-level `API_KEY`/`BASE_URL`/`OPENAI_MODEL`) with a lazy version, and add `render_verdict()` + `remember_verdict()`:

```python
"""SG Nutrition Investigator — agent definition (importable, no import-time env).

The standalone CLI (main.py) and the runtime contract adapter (adapter.py) both
import this module. Env is read lazily inside build_agent() so importing the
module requires no API key (e.g. for the hermetic render_verdict test).
"""
```

Append, after `INSTRUCTIONS`:

```python
def build_agent() -> Agent:
    """Construct the Agent from environment (read lazily — not at import time).

    Reads OPENAI_API_KEY (required), OPENAI_BASE_URL (optional), OPENAI_MODEL
    (default gpt-4o). Building the client + Agent makes no network call.
    """
    api_key = os.environ["OPENAI_API_KEY"]
    base_url = os.environ.get("OPENAI_BASE_URL") or None
    model_name = os.environ.get("OPENAI_MODEL", "gpt-4o")
    client = AsyncOpenAI(base_url=base_url, api_key=api_key)
    model = OpenAIChatCompletionsModel(model=model_name, openai_client=client)
    return Agent(
        name="SG Nutrition Investigator",
        model=model,
        instructions=INSTRUCTIONS,
        tools=[recall_product, check_sfa_additive, check_hcs, calculate_nutri_grade],
        output_type=NutritionVerdict,
    )


def render_verdict(v: NutritionVerdict) -> str:
    """Render a validated verdict to the CLI's signature prose block.

    Pure: identical input → identical output, no I/O. Used by both the CLI
    (main.py) and the contract adapter (adapter.py) so the hosted agent's
    streamed text matches what `uv run python main.py` prints.
    """
    lines = [
        f"Product: {v.product_name}",
        "",
        f"Reasoning: {v.reasoning}",
        "",
        f"Summary: {v.summary}",
        "",
    ]
    for f in v.green:
        lines.append(f"🟢 {f.finding}  [{f.source}]")
    for f in v.amber:
        lines.append(f"🟡 {f.finding}  [{f.source}]")
    for f in v.red:
        lines.append(f"🔴 {f.finding}  [{f.source}]")
    lines.append("")
    lines.append(f"Recommendation: {v.recommendation}")
    return "\n".join(lines)


def remember_verdict(v: NutritionVerdict) -> None:
    """Persist a verdict to agent_memory.json so a future run can recall it."""
    _MEMORY["products"][v.product_name.strip().lower()] = {
        "product_name": v.product_name,
        "summary": v.summary,
        "recommendation": v.recommendation,
    }
    _save_memory(_MEMORY)
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `cd examples/nutrition-label-openai && uv run pytest tests/test_render.py -q`
Expected: PASS (1 passed). (No key needed — `render_verdict`, `NutritionVerdict`, `Finding` import without touching env.)

- [ ] **Step 5: Slim `main.py` to a thin CLI that imports `agent.py`**

Replace the entire contents of `examples/nutrition-label-openai/main.py` with:

```python
"""SG Nutrition Label Investigator — standalone CLI (OpenAI Agents SDK).

Thin front-end over agent.py: loads local .env, builds the agent, runs it on a
label photo, and prints the verdict. The agent definition (tools, SFA data,
memory, typed output) lives in agent.py and is shared with the runtime contract
adapter (adapter.py).

Usage:
    cp .env.example .env   # then fill in your proxy key
    uv run python main.py            # defaults to the bundled milo.jpeg
    uv run python main.py path/to/label.jpeg
"""
import sys
import asyncio
from pathlib import Path

from dotenv import load_dotenv

# Load credentials from a local .env (gitignored) BEFORE importing agent, so the
# agent's lazy env reads see them. override=True so the local .env wins over any
# stray OPENAI_* in the shell (which would misroute to api.openai.com).
load_dotenv(Path(__file__).resolve().parent / ".env", override=True)

from agent import build_agent, render_verdict, remember_verdict, NutritionVerdict
from agents import Runner

DEFAULT_IMAGE = str(Path(__file__).resolve().parent / "milo.jpeg")


def _data_url(image_path: str) -> str:
    import base64, mimetypes
    mime = mimetypes.guess_type(image_path)[0] or "image/jpeg"
    b64 = base64.standard_b64encode(Path(image_path).read_bytes()).decode()
    return f"data:{mime};base64,{b64}"


async def investigate(image_path: str) -> None:
    print(f"\nInvestigating image: {image_path}\n{'─' * 60}")
    user_input = [{
        "role": "user",
        "content": [
            {"type": "input_text", "text": "Investigate this nutrition label."},
            {"type": "input_image", "image_url": _data_url(image_path)},
        ],
    }]
    result = await Runner.run(build_agent(), input=user_input)
    v: NutritionVerdict = result.final_output
    print(render_verdict(v))
    remember_verdict(v)


def main() -> None:
    image_path = sys.argv[1] if len(sys.argv) > 1 else DEFAULT_IMAGE
    if not Path(image_path).is_file():
        sys.exit(f"Image not found: {image_path}")
    asyncio.run(investigate(image_path))


if __name__ == "__main__":
    main()
```

Note: `main.py` now prints via `render_verdict()` (one block) instead of the old per-line loop. The information is identical; the format is the shared canonical one. `_data_url` is kept local to `main.py` (it is CLI-only; the adapter builds its own data URL from raw image bytes).

- [ ] **Step 6: Verify `main.py` still imports and the test suite passes**

Run: `cd examples/nutrition-label-openai && uv run python -c "import main; print('import ok')" && uv run pytest -q`
Expected: `import ok` then `1 passed`. (Importing `main` triggers `load_dotenv` + `import agent`; no network/agent run happens at import time.)

- [ ] **Step 7: Commit**

```bash
cd /Users/sausheong/projects/runtime
git add examples/nutrition-label-openai/agent.py examples/nutrition-label-openai/main.py examples/nutrition-label-openai/tests/
git commit -m "refactor(example): extract agent.py with pure render_verdict; slim main.py to CLI

agent.py is now importable with no import-time env (build_agent reads env
lazily), so both the CLI and the contract adapter share one agent definition,
one render_verdict, and one memory layer. Adds a hermetic render_verdict test.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 3: Create `adapter.py` (`NutritionAdapter`)

**Files:**
- Create: `examples/nutrition-label-openai/adapter.py`

- [ ] **Step 1: Create `adapter.py`**

```python
"""Adapter: the SG Nutrition Investigator agent -> runtime contract events.

Implements the runtime_contract AgentAdapter protocol. Drives the shared agent
(agent.py) for one invocation and yields a single text event carrying the
verdict rendered as prose (the same block main.py prints). The validated typed
NutritionVerdict is produced and checked by the SDK; we serialize it to prose
for transport over the text-based contract. Never raises — failures become one
error event; the library appends the terminal done/error.
"""
from __future__ import annotations

import base64
from typing import AsyncIterator, Sequence

from agents import Runner, SQLiteSession

from runtime_contract.events import ContractEvent, Image
from agent import build_agent, render_verdict, remember_verdict, NutritionVerdict


class NutritionAdapter:
    """AgentAdapter backed by the OpenAI Agents SDK nutrition agent."""

    def __init__(self, db_path: str):
        # build_agent() reads OPENAI_API_KEY here, so constructing the adapter
        # (done once at serve.py startup) fails fast if the key is missing.
        self._db = db_path
        self._agent = build_agent()

    async def run(
        self,
        session_id: str,
        message: str,
        images: Sequence[Image],
        history: Sequence[ContractEvent],
    ) -> AsyncIterator[ContractEvent]:
        # Build the SDK input: text, or a content-list with the image data URL
        # (same shape as main.py's investigate()).
        if images:
            img = images[0]
            data_url = f"data:{img.mime};base64,{base64.b64encode(img.data).decode()}"
            user_input = [
                {
                    "role": "user",
                    "content": [
                        {"type": "input_text", "text": message or "Investigate this nutrition label."},
                        {"type": "input_image", "image_url": data_url},
                    ],
                }
            ]
        else:
            user_input = message

        # SQLiteSession keyed on the runtime session id gives Level-1
        # conversation memory in the same shim db.
        session = SQLiteSession(session_id, self._db)
        try:
            # Non-streamed: with output_type=NutritionVerdict the SDK returns the
            # validated structured object at the end (not output_text deltas).
            result = await Runner.run(self._agent, input=user_input, session=session)
            v: NutritionVerdict = result.final_output
            remember_verdict(v)  # learn across sessions, like the CLI across runs
            yield ContractEvent(type="text", text=render_verdict(v))
        except Exception as e:  # never raise out of run(); surface as one error
            yield ContractEvent(type="error", error=str(e))
```

- [ ] **Step 2: Verify it imports (without a key, construction is deferred)**

Run: `cd examples/nutrition-label-openai && uv run python -c "import adapter; print('import ok')"`
Expected: `import ok`. (Importing the module does not construct `NutritionAdapter`, so no key is needed. If this errors with `ModuleNotFoundError: runtime_contract`, Task 5's path dep is not yet wired — that's expected if running before Task 5; re-run after Task 5. To keep tasks ordered, this step may be deferred to Task 5 Step 3.)

- [ ] **Step 3: Commit**

```bash
cd /Users/sausheong/projects/runtime
git add examples/nutrition-label-openai/adapter.py
git commit -m "feat(example): add NutritionAdapter bridging the agent to the runtime contract

Drives the shared agent, persists the verdict to memory, and yields the
prose-rendered verdict as one text event. Never raises.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 4: Create `serve.py` (entrypoint)

**Files:**
- Create: `examples/nutrition-label-openai/serve.py`

- [ ] **Step 1: Create `serve.py`**

```python
"""Entry point: serve the SG Nutrition Investigator over the runtime contract.

runtimed execs this (via the config's command/workdir) as a supervised agent.
It reads RUNTIME_* env injected by the control plane and serves the six contract
endpoints through the reusable runtime_contract library.
"""
from __future__ import annotations

import os

import uvicorn

from runtime_contract.app import create_app
from runtime_contract.store import Store
from adapter import NutritionAdapter


def main() -> None:
    addr = os.environ.get("RUNTIME_LISTEN_ADDR", "127.0.0.1:8302")
    host, _, port = addr.partition(":")
    agent_id = os.environ.get("RUNTIME_AGENT_ID", "nutrition-openai")
    db = os.environ.get("RUNTIME_SHIM_DB", "./shim.db")

    store = Store(db)
    adapter = NutritionAdapter(db_path=db)  # builds the agent; fails fast if no key
    app = create_app(adapter, store, agent_id)
    uvicorn.run(
        app,
        host=host or "127.0.0.1",
        port=int(port or "8302"),
        log_level="info",
    )


if __name__ == "__main__":
    main()
```

- [ ] **Step 2: Commit**

```bash
cd /Users/sausheong/projects/runtime
git add examples/nutrition-label-openai/serve.py
git commit -m "feat(example): add serve.py entrypoint hosting the agent on the contract

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 5: Wire the `runtime-contract` path dependency into the example

**Files:**
- Modify: `examples/nutrition-label-openai/pyproject.toml`

- [ ] **Step 1: Rewrite `examples/nutrition-label-openai/pyproject.toml`**

Replace the entire file with:

```toml
[project]
name = "nutrition-label-openai"
version = "0.1.0"
description = "SG Nutrition Label Investigator — OpenAI Agents SDK example agent for runtime"
readme = "README.md"
requires-python = ">=3.12"
dependencies = [
    "httpx>=0.28.1",
    "openai-agents>=0.17.4",
    "pydantic>=2.13.4",
    "python-dotenv>=1.2.2",
    "fastapi>=0.115",
    "uvicorn>=0.30",
    "runtime-contract",
]

[dependency-groups]
dev = ["pytest>=8"]

[tool.uv.sources]
runtime-contract = { path = "../../contrib/shims/python", editable = true }

[tool.pytest.ini_options]
testpaths = ["tests"]
```

`runtime-contract` is the package name from Task 1; `[tool.uv.sources]` resolves it to the shim directory as an editable path dep. `fastapi`/`uvicorn` are declared explicitly because `serve.py` imports `uvicorn` directly (they also arrive transitively via `runtime-contract`).

- [ ] **Step 2: Sync and verify resolution**

Run: `cd examples/nutrition-label-openai && uv sync`
Expected: resolves and installs, including `runtime-contract` from `../../contrib/shims/python` (editable). No errors.

- [ ] **Step 3: Verify the adapter and serve modules import end-to-end**

Run: `cd examples/nutrition-label-openai && uv run python -c "import serve, adapter; print('import ok')"`
Expected: `import ok` (proves `from runtime_contract.app import create_app` and `from adapter import NutritionAdapter` both resolve). No key needed — neither module constructs the adapter at import time.

- [ ] **Step 4: Re-run the hermetic test under the synced env**

Run: `cd examples/nutrition-label-openai && uv run pytest -q`
Expected: `1 passed`.

- [ ] **Step 5: Commit**

```bash
cd /Users/sausheong/projects/runtime
git add examples/nutrition-label-openai/pyproject.toml examples/nutrition-label-openai/uv.lock
git commit -m "build(example): depend on runtime-contract as an editable path dep

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 6: Config yaml + Makefile

**Files:**
- Create: `examples/nutrition-label-openai/runtime.nutrition-openai.yaml`
- Create: `examples/nutrition-label-openai/Makefile`

- [ ] **Step 1: Create the config yaml**

Create `examples/nutrition-label-openai/runtime.nutrition-openai.yaml`:

```yaml
# Host the OpenAI Agents SDK nutrition agent via the Python contract shim.
# Driven by this directory's Makefile (`make run`), which runs runtimed from the
# repo root, so `workdir` is repo-root-relative.
#
#   cp .env.example .env   # fill in your proxy key
#   make run               # builds binaries, uv sync, runs the control plane
agents:
  - id: nutrition-openai
    name: SG Nutrition Investigator (OpenAI SDK)
    model: openai/gpt-5.4
    listen_addr: 127.0.0.1:8302
    workdir: ./examples/nutrition-label-openai
    command: ["uv", "run", "python", "serve.py"]
```

- [ ] **Step 2: Create the Makefile**

Create `examples/nutrition-label-openai/Makefile`:

```makefile
# SG Nutrition Investigator (OpenAI Agents SDK) — hosted on runtime via the shim.
#
# A worked example of hosting a FOREIGN-SDK agent on the runtime platform: the
# control plane execs `uv run python serve.py` (this dir) as a supervised agent
# speaking the HTTP/SSE contract, reusing the runtime_contract library.
#
# Prerequisites:
#   - Postgres reachable at PG_DSN (from the repo root: `make pg-up`, or Postgres.app).
#     (Required by runtimed/control-plane startup; this agent itself uses SQLite.)
#   - LLM access: OPENAI_API_KEY + OPENAI_BASE_URL + OPENAI_MODEL (LiteLLM proxy or OpenAI).
#
# LLM credentials: copy .env.example to .env and fill in your key. This Makefile
# auto-loads .env (gitignored) and exports it, so the serve.py subprocess (spawned
# by runtimed) inherits it. .env wins over a stray OPENAI_* in your shell.
#
# Quick start:
#   cp .env.example .env        # then edit .env with your proxy key
#   make run                    # start the control plane hosting this agent
#   make demo-text              # (second shell) investigate a pasted label
#   make demo-image IMAGE=milo.jpeg   # investigate a label photo

# Repo root (this Makefile lives in examples/nutrition-label-openai/).
ROOT := $(abspath $(dir $(lastword $(MAKEFILE_LIST)))/../..)

# Load local LLM credentials if present (gitignored). Assignments here override
# the inherited shell environment, so the proxy key in .env beats a stray one.
-include .env

# ---- Configuration (override on the command line) ----
BIN_DIR   ?= $(ROOT)/bin
PG_DSN    ?= postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable
CTL_ADDR  ?= :8080
BASE_URL  ?= http://localhost:8080
AGENT     ?= nutrition-openai
CONFIG    ?= $(ROOT)/examples/nutrition-label-openai/runtime.nutrition-openai.yaml
IMAGE     ?= $(ROOT)/examples/nutrition-label-openai/milo.jpeg

# ---- LLM config (LiteLLM proxy by default; override in .env or on the CLI) ----
OPENAI_BASE_URL ?= https://litellm-stg.aip.gov.sg
OPENAI_MODEL    ?= gpt-5.4
# OPENAI_API_KEY has no default — set it in .env (see check-env).

# Export so the serve.py subprocess (spawned by runtimed) inherits them.
export OPENAI_API_KEY OPENAI_BASE_URL OPENAI_MODEL

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@echo "SG Nutrition Investigator (OpenAI SDK) — make targets:"
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build the platform binaries (delegates to the repo-root Makefile)
	$(MAKE) -C $(ROOT) build

.PHONY: sync
sync: ## Install Python deps for this example (incl. the runtime-contract path dep)
	uv sync

.PHONY: test
test: ## Run this example's hermetic unit tests (no key/network)
	uv run pytest -q

.PHONY: check-env
check-env: ## Verify OPENAI_API_KEY is set (the agent needs a real model)
	@if [ -z "$(OPENAI_API_KEY)" ]; then \
		echo "error: OPENAI_API_KEY is not set."; \
		echo "  Fix: cp .env.example .env  and put your LiteLLM proxy key in it."; \
		echo "  (.env is auto-loaded; OPENAI_BASE_URL defaults to $(OPENAI_BASE_URL))"; \
		exit 1; \
	fi
	@echo "LLM config: model=$(OPENAI_MODEL) base_url=$(OPENAI_BASE_URL)"

.PHONY: run
run: build sync check-env ## Build, sync, then run the control plane hosting this agent
	cd $(ROOT) && \
	RUNTIME_PG_DSN="$(PG_DSN)" \
	RUNTIME_CTL_ADDR="$(CTL_ADDR)" \
	RUNTIME_AGENTD_BIN="$(BIN_DIR)/agentd" \
	RUNTIME_CONFIG="$(CONFIG)" \
	$(BIN_DIR)/runtimed

.PHONY: conformance
conformance: ## Run the contract conformance suite against this agent
	$(BIN_DIR)/runtimectl conformance --agent $(AGENT)

.PHONY: demo-text
demo-text: ## Investigate a pasted label (text); streams the verdict
	$(BIN_DIR)/runtimectl invoke --agent $(AGENT) \
		"Investigate this label (text): Product: Milo UHT. Ingredients: water, skimmed milk, sugar, cocoa, malt extract, soy lecithin (E322), vitamins. Sugar 6g/100ml, saturated fat 1.5g/100ml. It is a beverage."

.PHONY: demo-image
demo-image: ## Investigate a label PHOTO (IMAGE=path); posts base64 + streams the verdict
	@test -f "$(IMAGE)" || { echo "error: image not found: $(IMAGE) (override with IMAGE=/path/to/label.jpeg)"; exit 1; }
	@echo "Posting $(IMAGE) to $(AGENT)..."
	@IMG=$$(base64 -i "$(IMAGE)"); \
	SID=$$(curl -s $(BASE_URL)/agents/$(AGENT)/sessions \
		-d "{\"message\":\"Investigate this label.\",\"image_b64\":\"$$IMG\",\"image_mime\":\"image/jpeg\"}" \
		| sed -E 's/.*"session_id":"([^"]+)".*/\1/'); \
	test -n "$$SID" || { echo "error: no session id returned (is the control plane running? make run)"; exit 1; }; \
	echo "session: $$SID"; \
	curl -sN "$(BASE_URL)/agents/$(AGENT)/sessions/$$SID/stream?since=0"

.PHONY: sessions
sessions: ## List this agent's sessions
	$(BIN_DIR)/runtimectl sessions --agent $(AGENT)

.PHONY: clean
clean: ## Remove this agent's runtime data (memory + shim db)
	rm -f agent_memory.json shim.db shim.db-wal shim.db-shm
```

- [ ] **Step 3: Verify the Makefile parses and config loads**

Run: `cd examples/nutrition-label-openai && make help`
Expected: the target list prints (no make syntax error).

Run: `cd /Users/sausheong/projects/runtime && go run ./cmd/runtimed --help 2>/dev/null; RUNTIME_CONFIG=examples/nutrition-label-openai/runtime.nutrition-openai.yaml go run ./cmd/runtimed 2>&1 & sleep 3; kill %1 2>/dev/null; true`
Expected: runtimed logs `supervising agent "nutrition-openai" at 127.0.0.1:8302` (it will try to exec `uv run python serve.py`; without a key serve.py exits and the supervisor backs off — that's fine, we only need to see the config parse and the agent register). If Postgres is not running, runtimed will fail at store/DBOS init — in that case skip this sub-step and rely on Task 7's full acceptance run. (Do not block the commit on a live run here.)

- [ ] **Step 4: Commit**

```bash
cd /Users/sausheong/projects/runtime
git add examples/nutrition-label-openai/runtime.nutrition-openai.yaml examples/nutrition-label-openai/Makefile
git commit -m "feat(example): Makefile + config to host the OpenAI agent under runtimed

Mirrors examples/nutrition-label-go: run/demo-text/demo-image/sessions/
conformance/clean. runtimed runs from repo root; workdir is repo-root-relative.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 7: Documentation + end-to-end acceptance

**Files:**
- Modify: `examples/nutrition-label-openai/README.md`
- Modify: `contrib/shims/python/README.md`

- [ ] **Step 1: Add a "Run under runtimed" section to the example README**

Append to `examples/nutrition-label-openai/README.md`:

```markdown

## Run under `runtimed` (hosted on the platform)

The same agent can run as a first-class Runtime agent, hosted by the control
plane through the Python contract shim (`../../contrib/shims/python`, the
reusable `runtime_contract` library). `runtimed` execs `uv run python serve.py`
here as a supervised subprocess speaking the HTTP/SSE agent contract; the typed
`NutritionVerdict` is rendered to the same prose block the CLI prints and
streamed as a `text` event.

```bash
cp .env.example .env          # fill in your LiteLLM proxy key
make run                      # builds binaries, uv sync, runs the control plane
# in a second shell:
make conformance              # contract acceptance gate (same suite as Go agents)
make demo-image IMAGE=milo.jpeg   # base64 the photo → POST → stream the verdict
make demo-text                # investigate a pasted label
make sessions                 # list this agent's sessions
```

Requires Postgres for the control plane (`make -C ../.. pg-up`, or Postgres.app).
Durability is Level 1 (sessions/events persist in `shim.db`, replayable via
`?since=N`; conversation memory via `SQLiteSession`); plus the agent's own
`agent_memory.json` learned aliases + product verdicts. Level 2 (in-flight crash
resume) is out of scope — see the repo `ROADMAP.md` §C1.
```

- [ ] **Step 2: Point the shim README at the example**

In `contrib/shims/python/README.md`, replace the worked OpenAI walkthrough (the "Run under `runtimed`" / OpenAI-specific sections) with a pointer. Ensure the README states:
- This directory is the **framework-agnostic contract library** (`runtime_contract`): the six endpoints, SSE framing, `?since=N` replay, SQLite Level-1 store, and the `AgentAdapter` protocol.
- It is consumed as a path dependency; a full worked example hosting the **OpenAI Agents SDK** lives at `examples/nutrition-label-openai/` (see its README + `make run`).
- Keep the existing "Architecture", "Adding another framework (one file implementing `AgentAdapter`)" template, and "Tests" (`uv run pytest`) sections.

Add near the top:

```markdown
> This is the reusable contract **library**. For a complete worked agent hosted
> on it, see [`examples/nutrition-label-openai`](../../../examples/nutrition-label-openai)
> (the SG Nutrition Investigator, OpenAI Agents SDK) — `make run` there boots it
> under `runtimed`.
```

Remove instructions that reference the deleted `main.py` / `adapters/openai_agents.py` / `runtime.openai-shim.yaml` so the README has no dangling references.

- [ ] **Step 3: Commit the docs**

```bash
cd /Users/sausheong/projects/runtime
git add examples/nutrition-label-openai/README.md contrib/shims/python/README.md
git commit -m "docs: hosted-run section for the OpenAI example; shim README points at it

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

- [ ] **Step 4: Full hermetic regression (both Python projects + Go)**

Run:
```bash
cd /Users/sausheong/projects/runtime
go build ./... && go vet ./... && go test ./...
( cd contrib/shims/python && uv run pytest -q )
( cd examples/nutrition-label-openai && uv run pytest -q )
```
Expected: Go build/vet/test all OK; both pytest suites pass. (`go build ./...` confirms removing the shim files didn't affect Go — it never referenced them.)

- [ ] **Step 5: Live end-to-end acceptance (requires a key + Postgres)**

This is the real proof; run manually (not in CI). Ensure Postgres is up and `.env` has a valid key.

Terminal 1:
```bash
cd examples/nutrition-label-openai && make run
```
Expected: control plane logs `control plane listening` and `supervising agent "nutrition-openai" at 127.0.0.1:8302`; the agent becomes healthy (`/healthz` gate passes).

Terminal 2:
```bash
cd examples/nutrition-label-openai
make conformance     # Expected: conformance: PASSED
make demo-image IMAGE=milo.jpeg
# Expected: session: <id>, then SSE frames:
#   id: 1  data: {"type":"text","text":"Product: ... 🟢/🟡/🔴 ... Recommendation: ..."}
#   id: 2  data: {"type":"done"}
make demo-text       # Expected: a streamed prose verdict for the pasted Milo label
make sessions        # Expected: lists sessions with status completed, turns=1
```

If conformance fails or no verdict streams, STOP and debug (use systematic-debugging); do not mark the plan complete. Capture the actual output in the completion report.

- [ ] **Step 6: Final commit (only if any tracked artifact changed; lockfiles, etc.)**

```bash
cd /Users/sausheong/projects/runtime
git add -A
git commit -m "chore: lockfile/artifact updates from nutrition-openai hosted run" || echo "nothing to commit"
```

---

## Self-review

**Spec coverage:**
- Full fidelity (tools/SFA/typed/memory) → Task 2 (agent.py moves all of it verbatim) + Task 3 (adapter runs it, persists verdict). ✓
- Example grows `serve.py`, agent co-located → Tasks 2–4. ✓
- Typed verdict → prose `text` event → `render_verdict` (Task 2) used by adapter (Task 3). ✓
- Makefile mirroring the Go example → Task 6. ✓
- `runtime_contract` standalone path-installable package → Task 1 (pyproject/hatchling) + Task 5 (path dep). ✓
- Remove the stand-in → Task 1. ✓
- Conformance acceptance + demo → Task 7. ✓
- No `.gitignore` change (already covered) → noted; no task needed. ✓
- Hermetic `render_verdict` test → Task 2. ✓

**Placeholder scan:** No TBD/TODO; every code step shows full content. The only "describe, don't paste" is Task 2's verbatim relocation of already-tested tool/data code (listed by exact name) + Task 7 Step 2's README edit (the surrounding code is prose, not code) — both intentional and explicit.

**Type/name consistency:** `build_agent()`, `render_verdict(v)`, `remember_verdict(v)`, `NutritionVerdict`, `Finding` defined in Task 2 and used identically in Tasks 2/3. `NutritionAdapter(db_path=...)` defined in Task 3, constructed identically in Task 4. `runtime-contract` package name consistent across Tasks 1/5. Port `8302` and agent id `nutrition-openai` consistent across Tasks 4/6/7. ✓
