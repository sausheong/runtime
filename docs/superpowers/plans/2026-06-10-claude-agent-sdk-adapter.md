# Claude Agent SDK Adapter (C1 M2) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Host the SG Nutrition Investigator (third implementation, Claude Agent SDK) as a first-class runtime agent through the UNCHANGED Python contract shim, proving the "thin adapter per framework" claim.

**Architecture:** New `examples/nutrition-label-claude/` consuming `runtime_contract` as an editable path dep (zero shim changes). Domain logic (SFA data, alias memory, NutritionVerdict, render) ported SDK-free into `agent.py`; the 4 nutrition tools + a 5th `submit_verdict` typed-output tool become in-process MCP tools (`@tool` + `create_sdk_mcp_server`); `adapter.py` drives per-turn `query()` with SDK-native `resume=` (runtime→SDK session-id map in the shim's SQLite); built-ins stripped, `permission_mode="dontAsk"`.

**Tech Stack:** Python ≥3.12 (uv), `claude-agent-sdk` (pinned), pydantic v2, `runtime_contract` (editable ../../contrib/shims/python), LiteLLM proxy via `ANTHROPIC_BASE_URL` with namespaced models (verified: `claude-sonnet-4-6-asia-southeast1` works on `/v1/messages`).

**Spec:** `docs/superpowers/specs/2026-06-10-claude-agent-sdk-adapter-design.md`

**Branch:** `feat/claude-sdk-adapter` (create from `master` before Task 1).

**Conventions:**
- Go side untouched; this is a pure examples/ + docs milestone. Still run `go build ./...` once at the end (sanity).
- Python: `uv` projects; run all `uv`/`pytest` from `examples/nutrition-label-claude/` unless stated. Shim tests run from `contrib/shims/python/`.
- LLM credentials live in the example's gitignored `.env` — copy the key from `examples/nutrition-label-openai/.env` (same proxy). NEVER commit keys.
- The OpenAI example (`examples/nutrition-label-openai/`) is the structural template: read its files before porting each counterpart.

## File Structure

| File | Responsibility |
|---|---|
| `examples/nutrition-label-claude/agent.py` | SDK-free domain: data load, `_resolve_additive`, COLLOQUIAL/CONSUMER_NOTES, memory JSON, `Finding`/`NutritionVerdict`, `INSTRUCTIONS`, `render_verdict`, `remember_verdict` |
| `tools.py` | 5 MCP tools + `NUTRITION_SERVER` + turn-scoped verdict holder |
| `sessions.py` | `SessionMap`: runtime session_id ↔ SDK session_id (SQLite table in RUNTIME_SHIM_DB) |
| `adapter.py` | `NutritionClaudeAdapter` (AgentAdapter) — the milestone's exhibit |
| `serve.py` | entry point, mirrors openai example |
| `spike_vision.py` | Task-1 spike script (kept in repo as living documentation of the input shape) |
| `pyproject.toml`, `Makefile`, `runtime.nutrition-claude.yaml`, `README.md`, `.env.example`, `.gitignore` | project scaffolding (mirror openai example) |
| `tests/` | hermetic unit tests |
| copied fixtures | `sfa_additives.json`, `milo.jpeg` from the openai example |

Key API facts (verified 2026-06-10; re-verify ONLY if something fails):
- `from claude_agent_sdk import query, ClaudeAgentOptions, tool, create_sdk_mcp_server, AssistantMessage, TextBlock, ResultMessage`
- `query(prompt=<str | AsyncIterable[dict]>, options=...) -> AsyncIterator[Message]`; drain to `ResultMessage` (don't `break` early).
- `ResultMessage`: `.subtype`, `.is_error`, `.result` (str|None), `.session_id`.
- Streaming-input message shape for images: `{"type": "user", "message": {"role": "user", "content": [{"type": "text", "text": ...}, {"type": "image", "source": {"type": "base64", "media_type": <mime>, "data": <b64 str>}}]}}` — THE SPIKE VERIFIES/CORRECTS THIS; whatever the spike proves becomes the adapter's shape.
- `@tool(name, description, input_schema)` where input_schema may be a JSON-schema dict; handler `async def f(args: dict) -> dict` returning `{"content": [{"type": "text", "text": ...}]}`.
- `create_sdk_mcp_server(name=..., version=..., tools=[...])`; options `mcp_servers={"nutrition": server}`; tool names `mcp__nutrition__<tool>`.
- Restriction: `disallowed_tools=[...]` removes built-ins from context; `allowed_tools` only auto-approves (NOT a restriction); `permission_mode="dontAsk"` denies anything not allowed — combine: `permission_mode="dontAsk"`, `allowed_tools=[the 5 mcp__nutrition__* names]`, `disallowed_tools=[built-in names]`. The spike verifies the combination.
- Env knobs: `ANTHROPIC_API_KEY`, `ANTHROPIC_BASE_URL`, `CLAUDE_CONFIG_DIR` (transcript home), `CLAUDE_CODE_DISABLE_AUTO_MEMORY=1`. Options: `resume=`, `cwd=`, `model=`, `max_turns=`, `system_prompt=`, `env=` (merges onto inherited), `setting_sources=[]`.

---

### Task 1: Project scaffold + vision/tool spike (DE-RISK FIRST — live network task)

This task needs the proxy reachable and the `.env` key. It proves the riskiest unknowns before any porting: (a) the SDK works through `ANTHROPIC_BASE_URL`→LiteLLM with a namespaced model; (b) text+image input round-trips; (c) in-process MCP tools get called; (d) built-in stripping works; (e) `resume` continuity works.

**Files:**
- Create: `examples/nutrition-label-claude/pyproject.toml`, `.gitignore`, `.env.example`, `spike_vision.py`
- Copy: `milo.jpeg`, `sfa_additives.json` from `../nutrition-label-openai/`
- Create: `.env` locally (NOT committed) by copying the OPENAI_API_KEY value from `examples/nutrition-label-openai/.env` as ANTHROPIC_API_KEY + the proxy URL

- [ ] **Step 1: Scaffold the uv project**

`pyproject.toml`:

```toml
[project]
name = "nutrition-label-claude"
version = "0.1.0"
description = "SG Nutrition Label Investigator — Claude Agent SDK example agent for runtime"
readme = "README.md"
requires-python = ">=3.12"
dependencies = [
    "claude-agent-sdk>=0.1.0",
    "httpx>=0.28.1",
    "pydantic>=2.13.4",
    "python-dotenv>=1.2.2",
    "fastapi>=0.115",
    "uvicorn>=0.30",
    "runtime-contract",
]

[dependency-groups]
dev = ["pytest>=8", "pytest-asyncio>=0.24"]

[tool.uv.sources]
runtime-contract = { path = "../../contrib/shims/python", editable = true }

[tool.pytest.ini_options]
testpaths = ["tests"]
asyncio_mode = "auto"
```

`.gitignore`:
```
.env
.venv/
__pycache__/
agent_memory.json
shim.db
claude-config/
```

`.env.example`:
```
ANTHROPIC_API_KEY=sk-...
ANTHROPIC_BASE_URL=https://litellm-stg.aip.gov.sg
ANTHROPIC_MODEL=claude-sonnet-4-6-asia-southeast1
```

Copy fixtures: `cp ../nutrition-label-openai/{milo.jpeg,sfa_additives.json} .`
Create `.env` from the openai example's key: read `examples/nutrition-label-openai/.env`, take its OPENAI_API_KEY value as ANTHROPIC_API_KEY, plus the two other lines from `.env.example`.

Run `uv sync` — must resolve (this also pins claude-agent-sdk; note the locked version in your report).

- [ ] **Step 2: Write the spike script**

`spike_vision.py` — standalone, NOT part of the served agent; kept as living documentation:

```python
"""Spike: prove the Claude Agent SDK works through the LiteLLM proxy BEFORE porting.

Run:  uv run python spike_vision.py
Needs .env (ANTHROPIC_API_KEY / ANTHROPIC_BASE_URL / ANTHROPIC_MODEL).

Proves, in order:
  1. text round-trip through query() via the proxy (namespaced model)
  2. an in-process MCP tool gets called (and built-ins are stripped)
  3. text+image (milo.jpeg) round-trips via the streaming-input form
  4. resume= continues a conversation in a NEW query() call

Prints PASS/FAIL per stage; exits non-zero on the first failure.
"""
from __future__ import annotations

import anyio
import base64
import os
import sys
from pathlib import Path

from dotenv import load_dotenv

load_dotenv()

from claude_agent_sdk import (  # noqa: E402
    query,
    tool,
    create_sdk_mcp_server,
    ClaudeAgentOptions,
    AssistantMessage,
    TextBlock,
    ResultMessage,
)

HERE = Path(__file__).resolve().parent
CONFIG_DIR = HERE / "claude-config"

CALLED = {"ping": False}


@tool("ping_tool", "Returns a fixed marker. Call this when asked to ping.", {"type": "object", "properties": {}})
async def ping_tool(args: dict) -> dict:
    CALLED["ping"] = True
    return {"content": [{"type": "text", "text": "PONG-MARKER-12345"}]}


SERVER = create_sdk_mcp_server(name="spike", version="0.1.0", tools=[ping_tool])

BUILTINS_OFF = ["Bash", "Read", "Write", "Edit", "Glob", "Grep", "WebFetch", "WebSearch", "NotebookEdit", "TodoWrite", "Task"]


def opts(resume: str | None = None) -> ClaudeAgentOptions:
    return ClaudeAgentOptions(
        model=os.environ["ANTHROPIC_MODEL"],
        mcp_servers={"spike": SERVER},
        allowed_tools=["mcp__spike__ping_tool"],
        disallowed_tools=BUILTINS_OFF,
        permission_mode="dontAsk",
        cwd=str(HERE),
        env={"CLAUDE_CONFIG_DIR": str(CONFIG_DIR), "CLAUDE_CODE_DISABLE_AUTO_MEMORY": "1"},
        setting_sources=[],
        max_turns=10,
        resume=resume,
    )


async def drive(prompt, o) -> tuple[str, ResultMessage]:
    text = []
    result = None
    async for msg in query(prompt=prompt, options=o):
        if isinstance(msg, AssistantMessage):
            for block in msg.content:
                if isinstance(block, TextBlock):
                    text.append(block.text)
        elif isinstance(msg, ResultMessage):
            result = msg
    assert result is not None, "no ResultMessage"
    return "".join(text), result


def image_prompt(text: str, path: Path):
    """Streaming-input form carrying an image block. THE SHAPE UNDER TEST."""
    b64 = base64.b64encode(path.read_bytes()).decode()
    async def gen():
        yield {
            "type": "user",
            "message": {
                "role": "user",
                "content": [
                    {"type": "text", "text": text},
                    {"type": "image", "source": {"type": "base64", "media_type": "image/jpeg", "data": b64}},
                ],
            },
        }
    return gen()


async def main() -> None:
    # 1. text round-trip
    txt, res = await drive("Reply with exactly: SPIKE-OK", opts())
    ok = "SPIKE-OK" in (txt + (res.result or ""))
    print(f"1 text round-trip: {'PASS' if ok else 'FAIL'} (session={res.session_id})")
    if not ok:
        sys.exit(1)
    first_session = res.session_id

    # 2. MCP tool call + builtin stripping
    txt, res = await drive(
        "Call the ping tool, then tell me what it returned. Also: what is in the file ./pyproject.toml? (If you cannot read files, say CANNOT-READ.)",
        opts(),
    )
    tool_ok = CALLED["ping"] and "PONG-MARKER-12345" in txt
    strip_ok = "CANNOT-READ" in txt or "cannot" in txt.lower()
    print(f"2 mcp tool called: {'PASS' if tool_ok else 'FAIL'}; builtins stripped: {'PASS' if strip_ok else 'FAIL'}")
    if not tool_ok:
        sys.exit(1)
    if not strip_ok:
        print("   WARN: builtin stripping unconfirmed — inspect output above; tighten disallowed_tools/tools")

    # 3. vision
    txt, res = await drive(image_prompt("Briefly: what product is shown on this label?", HERE / "milo.jpeg"), opts())
    vision_ok = "milo" in txt.lower() or "milo" in (res.result or "").lower()
    print(f"3 vision round-trip: {'PASS' if vision_ok else 'FAIL'}\n   model said: {txt[:200]}")
    if not vision_ok:
        sys.exit(1)

    # 4. resume continuity
    txt, res = await drive("Remember this codeword: ZANZIBAR. Confirm only.", opts())
    sid = res.session_id
    txt2, res2 = await drive("What was the codeword?", opts(resume=sid))
    resume_ok = "ZANZIBAR" in txt2.upper()
    print(f"4 resume continuity: {'PASS' if resume_ok else 'FAIL'} ({sid} -> {res2.session_id})")
    if not resume_ok:
        sys.exit(1)

    print("ALL SPIKE STAGES PASS")


if __name__ == "__main__":
    anyio.run(main)
```

- [ ] **Step 3: Run the spike**

Run: `cd examples/nutrition-label-claude && uv run python spike_vision.py`
Expected: all 4 stages PASS.

THIS IS THE DE-RISK GATE. If a stage fails, iterate HERE (not later):
- Stage 1 fails with model/auth errors → check `.env`, try `claude-opus-4-6-asia-southeast1` or other namespaced ids from the proxy's `/v1/models`.
- Stage 2 tool never called → check the SDK's actual tool registration API against installed source (`uv run python -c "import claude_agent_sdk, inspect; print(claude_agent_sdk.__file__)"` and read it); adjust `@tool` signature/options.
- Stage 3 fails → the input shape is wrong: read the installed SDK source for the streaming-input message format, correct `image_prompt`, re-run. If the PROXY rejects image blocks (LiteLLM passthrough gap), try fallback model ids; if all fail, STOP and report BLOCKED with exact errors — the spec's fallback ladder (temp-file MCP read tool / operator escalation) is a controller decision.
- Stage 4 fails → check `CLAUDE_CONFIG_DIR`+cwd pinning actually keeps transcripts (`ls claude-config/projects/`).

Record in your report: the locked claude-agent-sdk version, which stages needed shape corrections, and the FINAL working image-input shape (verbatim dict).

- [ ] **Step 4: Commit (no secrets!)**

```bash
cd /Users/sausheong/projects/runtime
git add examples/nutrition-label-claude/pyproject.toml examples/nutrition-label-claude/.gitignore \
        examples/nutrition-label-claude/.env.example examples/nutrition-label-claude/spike_vision.py \
        examples/nutrition-label-claude/milo.jpeg examples/nutrition-label-claude/sfa_additives.json \
        examples/nutrition-label-claude/uv.lock
git commit -m "feat(c1): claude-sdk example scaffold + proxy/vision/tool/resume spike"
```

Verify `git status` shows `.env` untracked (gitignored) before committing.

---

### Task 2: Port agent.py (domain logic, SDK-free)

**Files:**
- Create: `examples/nutrition-label-claude/agent.py`
- Test: `examples/nutrition-label-claude/tests/__init__.py`, `tests/test_agent.py`

agent.py is the openai example's agent.py MINUS everything SDK-specific: no `agents` imports, no `@function_tool` decorators (tool bodies become plain async functions in tools.py — Task 3), no `build_agent`. Keep VERBATIM: `_norm`, additive indexes, `COLLOQUIAL`, `CONSUMER_NOTES`, memory load/save, `_resolve_additive`, `_format_entry`, `_safe_json`, `Finding`, `NutritionVerdict`, `render_verdict`, `remember_verdict`. `INSTRUCTIONS` is kept with ONE addition (final paragraph): the submit_verdict mandate.

- [ ] **Step 1: Write the failing tests**

`tests/__init__.py` empty. `tests/test_agent.py`:

```python
"""Hermetic tests for the SDK-free domain module."""
import json
from pathlib import Path

import pytest

import agent
from agent import (
    NutritionVerdict,
    Finding,
    render_verdict,
    remember_verdict,
    _resolve_additive,
    _format_entry,
)


@pytest.fixture(autouse=True)
def isolate_memory(tmp_path, monkeypatch):
    """Point the memory file at a temp path so tests never touch the real one."""
    mem_path = tmp_path / "agent_memory.json"
    monkeypatch.setattr(agent, "_MEMORY_PATH", mem_path)
    fresh = {"learned_aliases": {}, "products": {}}
    monkeypatch.setattr(agent, "_MEMORY", fresh)
    yield fresh


def make_verdict() -> NutritionVerdict:
    return NutritionVerdict(
        reasoning="r", product_name="Milo", summary="s",
        green=[Finding(category="GREEN", finding="g", source="label")],
        amber=[], red=[],
        recommendation="ok",
    )


def test_resolve_by_e_number():
    assert _resolve_additive("E211")["name"].lower().startswith("sodium benzoate".split()[0])


def test_resolve_by_colloquial():
    assert _resolve_additive("MSG") is not None


def test_resolve_unknown_returns_none():
    assert _resolve_additive("unobtainium") is None


def test_format_entry_not_found_mentions_sfa():
    out = _format_entry(None, "mystery")
    assert "Not found" in out


def test_render_verdict_signature_block():
    text = render_verdict(make_verdict())
    assert "Product: Milo" in text
    assert "🟢 g  [label]" in text
    assert text.strip().endswith("Recommendation: ok")


def test_remember_then_resolve_alias():
    # learned alias: teach that 'zingo' is E211 via the memory dict directly
    agent._MEMORY["learned_aliases"]["zingo"] = "211"
    assert _resolve_additive("zingo") is not None


def test_remember_verdict_persists(tmp_path):
    remember_verdict(make_verdict())
    saved = json.loads(agent._MEMORY_PATH.read_text())
    assert saved["products"]["milo"]["summary"] == "s"


def test_instructions_mention_submit_verdict():
    assert "submit_verdict" in agent.INSTRUCTIONS
```

- [ ] **Step 2: Run to verify failure**

Run: `cd examples/nutrition-label-claude && uv run pytest tests/test_agent.py -v`
Expected: FAIL — `ModuleNotFoundError: agent`.

- [ ] **Step 3: Implement agent.py**

Port from `../nutrition-label-openai/agent.py` (READ IT FULLY first). Structure:

```python
"""SG Nutrition Investigator — shared domain logic, framework-free.

Third implementation of the investigator (after Go/harness and the OpenAI
Agents SDK). This module carries everything that is NOT the Claude Agent SDK:
the SFA additive table + resolution, the persistent learned-alias/product
memory, the typed NutritionVerdict, the system prompt, and the prose renderer.
tools.py wraps the lookups as in-process MCP tools; adapter.py drives the SDK.
"""
# ... imports: os/re/json/pathlib/httpx/pydantic (NO claude_agent_sdk, NO agents)
```

Copy verbatim from the openai agent.py: `HCS_RESOURCE_ID`, `_ADDITIVES` load, `_norm`, `_BY_E`/`_BY_ALIAS` build, `COLLOQUIAL`, `CONSUMER_NOTES`, `_MEMORY_PATH`/`_load_memory`/`_save_memory`/`_MEMORY`, `_resolve_additive`, `_format_entry`, `_safe_json`, `Finding`, `NutritionVerdict`, `render_verdict`, `remember_verdict`. Drop: `set_tracing_disabled`, all `agents` imports, `@function_tool` decorators (the four tool FUNCTION BODIES move to tools.py in Task 3 — do not leave them here), `build_agent`.

`INSTRUCTIONS`: copy the openai INSTRUCTIONS text, then REPLACE step 5 (which says "Return a NutritionVerdict") with:

```
5. When your investigation is complete, you MUST call the submit_verdict tool exactly once with the full verdict: reasoning (step-by-step working-out, written for a curious reader), product_name, summary, findings sorted into green/amber/red (each with category, finding, source — the tool that produced it, or "label"), and recommendation. Do not write the verdict as plain text; submit it through the tool. After submit_verdict succeeds, reply with a single short closing line.
```

- [ ] **Step 4: Run tests to verify pass**

Run: `uv run pytest tests/test_agent.py -v`
Expected: ALL PASS.

- [ ] **Step 5: Commit**

```bash
git add examples/nutrition-label-claude/agent.py examples/nutrition-label-claude/tests/
git commit -m "feat(c1): port nutrition domain logic SDK-free (agent.py)"
```

---

### Task 3: tools.py — 5 in-process MCP tools + verdict holder

**Files:**
- Create: `examples/nutrition-label-claude/tools.py`
- Test: `tests/test_tools.py`

- [ ] **Step 1: Write the failing tests**

`tests/test_tools.py`:

```python
"""Hermetic tests for the MCP tool handlers (no SDK subprocess, no network)."""
import json

import pytest

import agent
import tools
from tools import (
    check_sfa_additive_impl,
    recall_product_impl,
    calculate_nutri_grade_impl,
    submit_verdict_impl,
    VerdictHolder,
)


@pytest.fixture(autouse=True)
def isolate_memory(tmp_path, monkeypatch):
    monkeypatch.setattr(agent, "_MEMORY_PATH", tmp_path / "agent_memory.json")
    monkeypatch.setattr(agent, "_MEMORY", {"learned_aliases": {}, "products": {}})


async def test_check_additive_known():
    out = await check_sfa_additive_impl("E211", "")
    assert "Permitted by SFA" in out


async def test_check_additive_unknown_no_hint():
    out = await check_sfa_additive_impl("blorbium", "")
    assert "Not found" in out


async def test_check_additive_learns_alias_from_hint():
    out = await check_sfa_additive_impl("blorbium", "E211")
    assert "Permitted by SFA" in out
    # learned: resolves WITHOUT hint now
    out2 = await check_sfa_additive_impl("blorbium", "")
    assert "Permitted by SFA" in out2


async def test_recall_product_empty_then_present():
    out = await recall_product_impl("Milo")
    assert "No prior record" in out
    agent._MEMORY["products"]["milo"] = {
        "product_name": "Milo", "summary": "sugary", "recommendation": "sometimes",
    }
    out2 = await recall_product_impl("Milo")
    assert "Seen before" in out2 and "sugary" in out2


async def test_nutri_grade_bands():
    a = await calculate_nutri_grade_impl(0.5, 0.5)
    d = await calculate_nutri_grade_impl(20.0, 5.0)
    assert "Nutri-Grade: A" in a and "Nutri-Grade: D" in d


async def test_submit_verdict_valid_stashes():
    holder = VerdictHolder()
    payload = {
        "reasoning": "r", "product_name": "Milo", "summary": "s",
        "green": [{"category": "GREEN", "finding": "g", "source": "label"}],
        "amber": [], "red": [], "recommendation": "ok",
    }
    out = await submit_verdict_impl(payload, holder)
    assert "recorded" in out.lower()
    assert holder.verdict is not None and holder.verdict.product_name == "Milo"


async def test_submit_verdict_invalid_rejected():
    holder = VerdictHolder()
    out = await submit_verdict_impl({"product_name": "x"}, holder)  # missing fields
    assert holder.verdict is None
    assert "invalid" in out.lower()


def test_verdict_schema_matches_model():
    schema = tools.VERDICT_SCHEMA
    assert schema["type"] == "object"
    for field in ("reasoning", "product_name", "summary", "green", "amber", "red", "recommendation"):
        assert field in schema["properties"]


def test_server_has_five_tools():
    # build_nutrition_server wires the impls into SDK tools; tool count locked.
    holder = VerdictHolder()
    server = tools.build_nutrition_server(holder)
    assert server is not None
    assert len(tools.TOOL_NAMES) == 5
    assert "mcp__nutrition__submit_verdict" in tools.TOOL_NAMES
```

- [ ] **Step 2: Run to verify failure**

Run: `uv run pytest tests/test_tools.py -v`
Expected: FAIL — no module `tools`.

- [ ] **Step 3: Implement tools.py**

Design: pure `*_impl` functions (testable hermetically) + a builder that wraps them as SDK tools bound to a turn-scoped `VerdictHolder`. The check_sfa/check_hcs/recall/nutri-grade BODIES are ported from the openai agent.py's decorated functions (read them; identical logic, decorator gone).

```python
"""In-process MCP tools for the Claude Agent SDK nutrition agent.

Five tools: the four investigator tools (ported from the OpenAI example's
@function_tool bodies) plus submit_verdict — the typed-output channel that
replaces the OpenAI SDK's output_type. Pure *_impl functions carry the logic
(hermetically testable); build_nutrition_server() wraps them as SDK tools
bound to a turn-scoped VerdictHolder.
"""
from __future__ import annotations

import re
from dataclasses import dataclass, field

import httpx
from claude_agent_sdk import tool, create_sdk_mcp_server
from pydantic import ValidationError

import agent
from agent import NutritionVerdict, _resolve_additive, _format_entry, _norm, _safe_json, HCS_RESOURCE_ID


# JSON schema for submit_verdict, generated from the SAME pydantic model the
# OpenAI version enforced via output_type. $defs inlined by pydantic.
VERDICT_SCHEMA = NutritionVerdict.model_json_schema()


@dataclass
class VerdictHolder:
    """Turn-scoped slot the submit_verdict tool fills and the adapter reads."""
    verdict: NutritionVerdict | None = None


async def check_sfa_additive_impl(additive: str, e_number_hint: str) -> str:
    raw = additive.strip()
    entry = _resolve_additive(raw)
    if not entry and e_number_hint.strip():
        entry = _resolve_additive(e_number_hint)
        if entry:
            num = entry["e_number"] or entry.get("ins") or ""
            agent._MEMORY["learned_aliases"][_norm(raw)] = re.sub(r"\(.*\)", "", num).lower()
            agent._save_memory(agent._MEMORY)
    return _format_entry(entry, raw)


async def recall_product_impl(product_name: str) -> str:
    rec = agent._MEMORY["products"].get(product_name.strip().lower())
    if not rec:
        return f"No prior record of '{product_name}'. This is a first investigation."
    return (f"Seen before — prior verdict for '{rec['product_name']}': {rec['summary']} "
            f"Recommendation: {rec['recommendation']}")


async def check_hcs_impl(product_name: str) -> str:
    try:
        async with httpx.AsyncClient(timeout=10) as client:
            resp = await client.get(
                "https://data.gov.sg/api/action/datastore_search",
                params={"resource_id": HCS_RESOURCE_ID, "q": product_name, "limit": 5},
            )
            records = _safe_json(resp).get("result", {}).get("records", [])
    except httpx.HTTPError as e:
        return f"HCS check failed (network error): {e}"
    if records:
        matches = [r.get("brand_and_product_name", "?") for r in records]
        return f"HCS CERTIFIED. Matching products: {', '.join(matches)}"
    return (f"NOT FOUND in HCS database. '{product_name}' does not appear to carry "
            "the Healthier Choice Symbol (absence is not necessarily a concern).")


async def calculate_nutri_grade_impl(sugar_per_100ml: float, saturated_fat_per_100ml: float) -> str:
    sugar, sat_fat = sugar_per_100ml, saturated_fat_per_100ml
    if sugar <= 1 and sat_fat <= 0.7:
        grade, desc = "A", "Healthiest tier — very low sugar and saturated fat"
    elif sugar <= 5 and sat_fat <= 1.2:
        grade, desc = "B", "Acceptable — moderate sugar and saturated fat"
    elif sugar <= 10 and sat_fat <= 2.8:
        grade, desc = "C", "Less healthy — must display Nutri-Grade label"
    else:
        grade, desc = "D", "Least healthy — mandatory label; cannot advertise to children"
    return (f"Nutri-Grade: {grade} | Sugar {sugar}g/100ml | "
            f"Sat fat {sat_fat}g/100ml | {desc}")


async def submit_verdict_impl(payload: dict, holder: VerdictHolder) -> str:
    try:
        holder.verdict = NutritionVerdict.model_validate(payload)
    except ValidationError as e:
        return f"Invalid verdict, fix and resubmit: {e.errors()[:3]}"
    return "Verdict recorded. Reply with one short closing line."


TOOL_NAMES = [
    "mcp__nutrition__recall_product",
    "mcp__nutrition__check_sfa_additive",
    "mcp__nutrition__check_hcs",
    "mcp__nutrition__calculate_nutri_grade",
    "mcp__nutrition__submit_verdict",
]


def build_nutrition_server(holder: VerdictHolder):
    """Build the per-turn MCP server: tools close over the given holder."""

    @tool("recall_product",
          "Recall whether this product has been investigated before, and the prior verdict. Call this FIRST.",
          {"type": "object", "properties": {"product_name": {"type": "string"}}, "required": ["product_name"]})
    async def recall_product(args: dict) -> dict:
        return _text(await recall_product_impl(args["product_name"]))

    @tool("check_sfa_additive",
          "Check whether a food additive is permitted by the Singapore Food Agency. Accepts an E-number or a name; pass e_number_hint when you know the number for an unrecognised name (it will be remembered).",
          {"type": "object",
           "properties": {"additive": {"type": "string"}, "e_number_hint": {"type": "string"}},
           "required": ["additive"]})
    async def check_sfa_additive(args: dict) -> dict:
        return _text(await check_sfa_additive_impl(args["additive"], args.get("e_number_hint", "")))

    @tool("check_hcs",
          "Check if a product carries Singapore's Healthier Choice Symbol (HPB dataset on data.gov.sg).",
          {"type": "object", "properties": {"product_name": {"type": "string"}}, "required": ["product_name"]})
    async def check_hcs(args: dict) -> dict:
        return _text(await check_hcs_impl(args["product_name"]))

    @tool("calculate_nutri_grade",
          "Calculate the Singapore Nutri-Grade (A/B/C/D) for a BEVERAGE from sugar and saturated fat per 100ml.",
          {"type": "object",
           "properties": {"sugar_per_100ml": {"type": "number"}, "saturated_fat_per_100ml": {"type": "number"}},
           "required": ["sugar_per_100ml", "saturated_fat_per_100ml"]})
    async def calculate_nutri_grade(args: dict) -> dict:
        return _text(await calculate_nutri_grade_impl(args["sugar_per_100ml"], args["saturated_fat_per_100ml"]))

    @tool("submit_verdict",
          "Submit the final structured NutritionVerdict. You MUST call this exactly once when the investigation is complete.",
          VERDICT_SCHEMA)
    async def submit_verdict(args: dict) -> dict:
        return _text(await submit_verdict_impl(args, holder))

    return create_sdk_mcp_server(
        name="nutrition", version="0.1.0",
        tools=[recall_product, check_sfa_additive, check_hcs, calculate_nutri_grade, submit_verdict],
    )


def _text(s: str) -> dict:
    return {"content": [{"type": "text", "text": s}]}
```

NOTE: if the spike (Task 1) revealed a different `@tool`/handler/return convention in the installed SDK version, FOLLOW THE SPIKE's proven convention and note the deviation.

- [ ] **Step 4: Run tests**

Run: `uv run pytest tests/ -v`
Expected: ALL PASS (test_agent + test_tools).

- [ ] **Step 5: Commit**

```bash
git add examples/nutrition-label-claude/tools.py examples/nutrition-label-claude/tests/test_tools.py
git commit -m "feat(c1): five in-process MCP tools incl. submit_verdict typed output"
```

---

### Task 4: sessions.py — runtime↔SDK session map

**Files:**
- Create: `examples/nutrition-label-claude/sessions.py`
- Test: `tests/test_sessions.py`

- [ ] **Step 1: Failing tests**

`tests/test_sessions.py`:

```python
import pytest

from sessions import SessionMap


def test_lookup_absent_returns_none(tmp_path):
    m = SessionMap(str(tmp_path / "shim.db"))
    assert m.lookup("ses-1") is None


def test_store_then_lookup(tmp_path):
    m = SessionMap(str(tmp_path / "shim.db"))
    m.store("ses-1", "sdk-abc")
    assert m.lookup("ses-1") == "sdk-abc"


def test_store_upserts(tmp_path):
    m = SessionMap(str(tmp_path / "shim.db"))
    m.store("ses-1", "sdk-abc")
    m.store("ses-1", "sdk-def")
    assert m.lookup("ses-1") == "sdk-def"


def test_survives_reopen(tmp_path):
    p = str(tmp_path / "shim.db")
    SessionMap(p).store("ses-1", "sdk-abc")
    assert SessionMap(p).lookup("ses-1") == "sdk-abc"
```

- [ ] **Step 2: Verify failure**

Run: `uv run pytest tests/test_sessions.py -v` → FAIL (no module).

- [ ] **Step 3: Implement sessions.py**

```python
"""Runtime session_id → Claude SDK session_id map.

The SDK owns conversation state (JSONL transcripts under CLAUDE_CONFIG_DIR);
the platform owns the runtime session id. This one-table map ties them so a
turn can resume= the SDK session belonging to its runtime session. Lives in
the same SQLite file as the contract store (RUNTIME_SHIM_DB) — separate
table, no schema interference; the same co-location the OpenAI adapter used
for its SQLiteSession.
"""
from __future__ import annotations

import sqlite3


class SessionMap:
    def __init__(self, db_path: str):
        self._db = db_path
        with self._conn() as c:
            c.execute(
                "CREATE TABLE IF NOT EXISTS sdk_sessions ("
                "runtime_id TEXT PRIMARY KEY, sdk_id TEXT NOT NULL)"
            )

    def _conn(self) -> sqlite3.Connection:
        return sqlite3.connect(self._db)

    def lookup(self, runtime_id: str) -> str | None:
        with self._conn() as c:
            row = c.execute(
                "SELECT sdk_id FROM sdk_sessions WHERE runtime_id = ?", (runtime_id,)
            ).fetchone()
        return row[0] if row else None

    def store(self, runtime_id: str, sdk_id: str) -> None:
        with self._conn() as c:
            c.execute(
                "INSERT INTO sdk_sessions (runtime_id, sdk_id) VALUES (?, ?) "
                "ON CONFLICT(runtime_id) DO UPDATE SET sdk_id = excluded.sdk_id",
                (runtime_id, sdk_id),
            )
```

- [ ] **Step 4: Tests pass** — `uv run pytest tests/test_sessions.py -v`

- [ ] **Step 5: Commit**

```bash
git add examples/nutrition-label-claude/sessions.py examples/nutrition-label-claude/tests/test_sessions.py
git commit -m "feat(c1): runtime-to-SDK session id map"
```

---

### Task 5: adapter.py + serve.py (the exhibit)

**Files:**
- Create: `examples/nutrition-label-claude/adapter.py`, `serve.py`
- Test: `tests/test_adapter.py`

- [ ] **Step 1: Failing tests**

`tests/test_adapter.py` — fakes `query` so no subprocess/network:

```python
"""Adapter tests with a faked claude_agent_sdk.query (no subprocess, no network)."""
import dataclasses

import pytest

import adapter as adapter_mod
from adapter import NutritionClaudeAdapter
from agent import NutritionVerdict, Finding
from runtime_contract.events import ContractEvent, Image


VALID = dict(
    reasoning="r", product_name="Milo", summary="s",
    green=[], amber=[], red=[], recommendation="ok",
)


class FakeResult:
    def __init__(self, session_id="sdk-1", is_error=False, result="text", subtype="success"):
        self.session_id = session_id
        self.is_error = is_error
        self.result = result
        self.subtype = subtype


def fake_query(*, submit=True, session_id="sdk-1", is_error=False, raise_exc=None, capture=None):
    """Returns an async-gen factory mimicking query(prompt=..., options=...)."""
    async def _gen(prompt=None, options=None):
        if capture is not None:
            capture["prompt"] = prompt
            capture["options"] = options
        if raise_exc:
            raise raise_exc
        if submit and options is not None:
            # simulate the model calling submit_verdict: fill the holder the
            # adapter handed to build_nutrition_server via its turn state
            adapter_mod._test_last_holder.verdict = NutritionVerdict(**VALID)
        yield FakeResult(session_id=session_id, is_error=is_error)
    return _gen


@pytest.fixture
def adp(tmp_path, monkeypatch):
    monkeypatch.setenv("ANTHROPIC_MODEL", "test-model")
    a = NutritionClaudeAdapter(str(tmp_path / "shim.db"))
    return a


async def collect(agen):
    return [e async for e in agen]


async def test_text_turn_yields_rendered_verdict(adp, monkeypatch):
    monkeypatch.setattr(adapter_mod, "query", fake_query())
    events = await collect(adp.run("ses-1", "investigate Milo", [], []))
    assert len(events) == 1 and events[0].type == "text"
    assert "Product: Milo" in events[0].text


async def test_session_id_captured(adp, monkeypatch):
    monkeypatch.setattr(adapter_mod, "query", fake_query(session_id="sdk-77"))
    await collect(adp.run("ses-1", "go", [], []))
    assert adp._sessions.lookup("ses-1") == "sdk-77"


async def test_resume_passed_on_second_turn(adp, monkeypatch):
    cap = {}
    monkeypatch.setattr(adapter_mod, "query", fake_query(session_id="sdk-77"))
    await collect(adp.run("ses-1", "first", [], []))
    monkeypatch.setattr(adapter_mod, "query", fake_query(capture=cap))
    await collect(adp.run("ses-1", "second", [], []))
    assert cap["options"].resume == "sdk-77"


async def test_image_turn_builds_block_prompt(adp, monkeypatch):
    cap = {}
    monkeypatch.setattr(adapter_mod, "query", fake_query(capture=cap))
    img = Image(mime="image/jpeg", data=b"\xff\xd8fake")
    await collect(adp.run("ses-1", "what is this", [img], []))
    # streaming-input form: an async iterable, not a plain string
    assert not isinstance(cap["prompt"], str)


async def test_error_result_yields_error_event(adp, monkeypatch):
    monkeypatch.setattr(adapter_mod, "query", fake_query(submit=False, is_error=True))
    events = await collect(adp.run("ses-1", "go", [], []))
    assert events[-1].type == "error"


async def test_no_verdict_falls_back_to_text(adp, monkeypatch):
    monkeypatch.setattr(adapter_mod, "query", fake_query(submit=False))
    events = await collect(adp.run("ses-1", "go", [], []))
    assert events[0].type == "text"  # ResultMessage.result fallback


async def test_exception_becomes_single_error_event(adp, monkeypatch):
    monkeypatch.setattr(adapter_mod, "query", fake_query(raise_exc=RuntimeError("boom")))
    events = await collect(adp.run("ses-1", "go", [], []))
    assert len(events) == 1 and events[0].type == "error" and "boom" in events[0].error
```

NOTE the `adapter_mod._test_last_holder` seam: the adapter exposes its current turn's holder via a module-level test hook (set in run() right after creating it). Cheap, explicit, and keeps the fake simple. Alternative if you prefer: have fake_query introspect `options.mcp_servers` — but the holder hook is simpler; implement the hook.

- [ ] **Step 2: Verify failure** — `uv run pytest tests/test_adapter.py -v` → FAIL.

- [ ] **Step 3: Implement adapter.py**

```python
"""Adapter: SG Nutrition Investigator (Claude Agent SDK) -> runtime contract.

The C1-M2 exhibit: hosting a Claude Agent SDK agent required ONLY this file
(plus domain/tool/session modules) — runtime_contract is consumed unchanged.

Per turn: look up the SDK session for this runtime session, drive query()
with resume=, capture the submit_verdict payload via a turn-scoped holder,
persist the SDK session id, and yield ONE text event (rendered verdict) or
ONE error event. Never raises out of run(); the library appends the terminal
done/error lifecycle event.
"""
from __future__ import annotations

import base64
import os
from pathlib import Path
from typing import AsyncIterator, Sequence

from claude_agent_sdk import (
    query,
    ClaudeAgentOptions,
    AssistantMessage,
    TextBlock,
    ResultMessage,
)

from runtime_contract.events import ContractEvent, Image

import agent
from agent import render_verdict, remember_verdict
from sessions import SessionMap
from tools import VerdictHolder, build_nutrition_server, TOOL_NAMES

HERE = Path(__file__).resolve().parent

# Built-ins stripped from a network-facing agent; the spike (spike_vision.py)
# verified this combination leaves only the nutrition MCP tools callable.
BUILTINS_OFF = [
    "Bash", "Read", "Write", "Edit", "Glob", "Grep",
    "WebFetch", "WebSearch", "NotebookEdit", "TodoWrite", "Task",
]

# Test seam: tests fake query() and need the current turn's holder.
_test_last_holder: VerdictHolder | None = None


class NutritionClaudeAdapter:
    """AgentAdapter backed by the Claude Agent SDK nutrition agent."""

    def __init__(self, db_path: str):
        self._sessions = SessionMap(db_path)
        # Transcript home pinned NEXT TO the shim db: resume is keyed by
        # (CLAUDE_CONFIG_DIR, cwd), so both must be stable across restarts.
        self._config_dir = str(Path(db_path).resolve().parent / "claude-config")
        self._model = os.environ["ANTHROPIC_MODEL"]  # fail fast at startup

    def _options(self, holder: VerdictHolder, resume: str | None) -> ClaudeAgentOptions:
        return ClaudeAgentOptions(
            model=self._model,
            resume=resume,
            system_prompt=agent.INSTRUCTIONS,
            mcp_servers={"nutrition": build_nutrition_server(holder)},
            allowed_tools=list(TOOL_NAMES),
            disallowed_tools=list(BUILTINS_OFF),
            permission_mode="dontAsk",
            cwd=str(HERE),
            env={
                "CLAUDE_CONFIG_DIR": self._config_dir,
                "CLAUDE_CODE_DISABLE_AUTO_MEMORY": "1",
            },
            setting_sources=[],
            max_turns=25,
        )

    @staticmethod
    def _prompt(message: str, images: Sequence[Image]):
        if not images:
            return message or "Investigate this nutrition label."
        img = images[0]
        b64 = base64.b64encode(img.data).decode()

        async def gen():
            yield {
                "type": "user",
                "message": {
                    "role": "user",
                    "content": [
                        {"type": "text", "text": message or "Investigate this nutrition label."},
                        {"type": "image",
                         "source": {"type": "base64", "media_type": img.mime, "data": b64}},
                    ],
                },
            }

        return gen()

    async def run(
        self,
        session_id: str,
        message: str,
        images: Sequence[Image],
        history: Sequence[ContractEvent],
    ) -> AsyncIterator[ContractEvent]:
        # `history` unused: the SDK's own transcripts (resume=) own memory,
        # the same stance the OpenAI adapter took with SQLiteSession.
        global _test_last_holder
        holder = VerdictHolder()
        _test_last_holder = holder
        try:
            resume = self._sessions.lookup(session_id)
            opts = self._options(holder, resume)
            text_parts: list[str] = []
            result: ResultMessage | None = None
            async for msg in query(prompt=self._prompt(message, images), options=opts):
                if isinstance(msg, AssistantMessage):
                    for block in msg.content:
                        if isinstance(block, TextBlock):
                            text_parts.append(block.text)
                elif isinstance(msg, ResultMessage):
                    result = msg
            if result is not None and result.session_id:
                self._sessions.store(session_id, result.session_id)
            if holder.verdict is not None:
                remember_verdict(holder.verdict)
                yield ContractEvent(type="text", text=render_verdict(holder.verdict))
            elif result is not None and result.is_error:
                yield ContractEvent(type="error",
                                    error=f"agent run failed ({result.subtype}): {result.result or ''}")
            else:
                # Fidelity fallback: agent never called submit_verdict.
                fallback = (result.result if result and result.result else "".join(text_parts))
                yield ContractEvent(type="text", text=fallback or "(no output)")
        except Exception as e:  # never raise out of run()
            yield ContractEvent(type="error", error=str(e))
```

IMPORTANT for the implementer: the FakeResult in tests is not a real ResultMessage — `isinstance(msg, ResultMessage)` will be False for it. Make the adapter's loop robust to this OR (simpler, do this) have the test's FakeResult subclass nothing and instead detect the result by duck-typing in the adapter: `elif hasattr(msg, "session_id") and hasattr(msg, "is_error"): result = msg` — NO. Keep the adapter clean with isinstance; instead make the TEST register FakeResult as a virtual subclass: `ResultMessage.register(FakeResult)` won't work (it's a dataclass, not ABC). RESOLUTION: in tests, monkeypatch `adapter_mod.ResultMessage` to `FakeResult` alongside `adapter_mod.query` (one extra line in the fixture: `monkeypatch.setattr(adapter_mod, "ResultMessage", FakeResult)`). Add that line to the `adp` fixture. Same for AssistantMessage if a test ever fakes text blocks (none currently do).

`serve.py`:

```python
"""Entry point: serve the Claude-SDK nutrition investigator over the contract.

runtimed execs this (config command/workdir) as a supervised agent. Operator
parameters come from the injected RUNTIME_* env via runtime_contract.serve;
this file only builds the adapter. The factory form shares RUNTIME_SHIM_DB
between the contract store and the session map.
"""
from __future__ import annotations

import os

from dotenv import load_dotenv

load_dotenv()  # .env: ANTHROPIC_API_KEY / ANTHROPIC_BASE_URL / ANTHROPIC_MODEL

from runtime_contract import serve  # noqa: E402

from adapter import NutritionClaudeAdapter  # noqa: E402


def main() -> None:
    print(
        f"serving agent {os.environ.get('RUNTIME_AGENT_ID', 'nutrition-claude')} "
        f"with ANTHROPIC_MODEL={os.environ.get('ANTHROPIC_MODEL', '(unset!)')}",
        flush=True,
    )
    serve(NutritionClaudeAdapter)


if __name__ == "__main__":
    main()
```

- [ ] **Step 4: All unit tests pass**

Run: `uv run pytest tests/ -v`
Expected: ALL PASS (agent + tools + sessions + adapter).

- [ ] **Step 5: Shim regression**

Run: `cd ../../contrib/shims/python && uv run pytest -q && cd -`
Expected: shim suite passes UNCHANGED. If anything in the shim had to change to make this adapter work — STOP, report it loudly (milestone finding).

- [ ] **Step 6: Commit**

```bash
git add examples/nutrition-label-claude/adapter.py examples/nutrition-label-claude/serve.py examples/nutrition-label-claude/tests/test_adapter.py
git commit -m "feat(c1): Claude Agent SDK adapter + serve entry point"
```

---

### Task 6: Makefile, runtime yaml, README + hosted live proof

**Files:**
- Create: `examples/nutrition-label-claude/Makefile`, `runtime.nutrition-claude.yaml`, `README.md`

- [ ] **Step 1: runtime.nutrition-claude.yaml**

```yaml
# Host the Claude Agent SDK nutrition agent via the Python contract shim.
# Driven by this directory's Makefile (`make run`), which runs runtimed from
# the repo root, so `workdir` is repo-root-relative.
#
#   cp .env.example .env   # fill in your proxy key
#   make run               # builds binaries, uv sync, runs the control plane
agents:
  - id: nutrition-claude
    name: SG Nutrition Investigator (Claude Agent SDK)
    model: claude-sonnet-4-6
    listen_addr: 127.0.0.1:8303
    workdir: ./examples/nutrition-label-claude
    command: ["uv", "run", "python", "serve.py"]
```

(Port 8303: the openai example uses 8302 — verify by grep and pick free.)

- [ ] **Step 2: Makefile**

READ `../nutrition-label-openai/Makefile` FULLY and mirror it: same targets (`run`, `demo-text`, `demo-image`, `sessions`, `test`, `clean`), `-include .env` + export, ROOT resolution, binary builds, `RUNTIME_CONFIG=runtime.nutrition-claude.yaml`. Differences: env names are ANTHROPIC_*; demo-image posts milo.jpeg base64 to the nutrition-claude agent's session endpoint (copy the openai Makefile's curl pattern, change agent id + port).

- [ ] **Step 3: README.md**

Mirror the openai example's README structure: what this is (THIRD implementation of the investigator; the C1-M2 reuse proof), prerequisites (Postgres, proxy key, uv), quick start, how it works (per-turn query()+resume, the 5 MCP tools incl. submit_verdict typed output, built-ins stripped, transcripts under ./claude-config), the THREE-IMPLEMENTATIONS comparison table (Go/harness vs OpenAI SDK vs Claude SDK: memory mechanism, typed output mechanism, tool definition style, lines in adapter), limitations (subprocess-per-turn latency; warm-client upgrade noted for Level 2; pinned SDK version).

- [ ] **Step 4: Hosted live proof (needs Postgres + proxy)**

```bash
cd examples/nutrition-label-claude
cp -n .env.example .env 2>/dev/null; # ensure .env has the real key (Task 1 created it)
make run    # control plane + hosted agent
```

Second shell:
```bash
cd examples/nutrition-label-claude
# conformance against the hosted agent THROUGH the control plane:
$(git rev-parse --show-toplevel)/bin/runtimectl conformance --base http://localhost:8080/agents/nutrition-claude || \
  go run ./cmd/runtimectl conformance --base http://localhost:8080/agents/nutrition-claude
# text investigation:
make demo-text   # or the curl equivalent from the Makefile
# vision verdict (THE parity proof):
make demo-image IMAGE=milo.jpeg
```

Then Level-1 resume proof: note the session id from demo-image; Ctrl-C the control plane; `make run` again; POST a follow-up message to the SAME session asking "what did you conclude about that product earlier?" — the reply must reference the Milo verdict. Then alias learning: in a NEW session teach "check the additive 'blorbium', its number is E211"; in ANOTHER new session ask about blorbium WITHOUT the hint — must resolve.

Record: conformance PASS output, the vision verdict text, resume + alias proofs. Iterate on the adapter if any step fails (likely suspects: tool-permission combos, image shape drift from the spike, model id).

- [ ] **Step 5: Commit**

```bash
git add examples/nutrition-label-claude/Makefile examples/nutrition-label-claude/runtime.nutrition-claude.yaml examples/nutrition-label-claude/README.md
git commit -m "feat(c1): Makefile, runtime config, README + hosted live proof for claude adapter"
```

---

### Task 7: ROADMAP + final verification

**Files:**
- Modify: `ROADMAP.md`

- [ ] **Step 1: ROADMAP update**

Header: checkpoint date "2026-06-10 (C1 M2 — Claude Agent SDK adapter)"; Current state's C1 sentence extended (two foreign frameworks hosted via the one Python shim).

§C1: update "Remaining C1 work" (second adapter DONE; remaining: Level 2, TS shim, PydanticAI as a further candidate) and append a "**Second milestone DONE (merged to `master`, 2026-06-10):** Claude Agent SDK adapter." paragraph in house style covering: the reuse claim tested against a maximally-different architecture (spawned CLI subprocess + JSONL-on-disk state vs in-process client); shim consumed UNCHANGED (or the loud finding if not); the third nutrition implementation at full parity (5 MCP tools incl. tool-as-output submit_verdict replacing output_type, vision verdict live, Level-1 resume via SDK-native resume= with a runtime→SDK id map, learned aliases); built-ins stripped + dontAsk posture; proxy wiring (ANTHROPIC_BASE_URL→LiteLLM, namespaced models); the HONEST MEASUREMENTS (adapter.py line count vs the ~30-line claim, locked SDK version, any deviations); spec/plan paths.

- [ ] **Step 2: Final verification**

```bash
cd examples/nutrition-label-claude && uv run pytest tests/ -q
cd ../../contrib/shims/python && uv run pytest -q
cd ../../.. 2>/dev/null || cd $(git rev-parse --show-toplevel)
go vet ./... && go build ./... && go test ./... -count=1   # Go side untouched, sanity
git status   # clean except intended; .env NOT tracked
```

- [ ] **Step 3: Commit**

```bash
git add ROADMAP.md
git commit -m "docs: ROADMAP through C1 M2 (Claude Agent SDK adapter)"
```

---

## Completion

Final whole-branch review (focus: secrets hygiene — no keys in any committed file; shim untouched verification via `git diff master -- contrib/`; the honest-measurements clause satisfied), then **superpowers:finishing-a-development-branch** to merge `feat/claude-sdk-adapter` to `master`.
