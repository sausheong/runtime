"""Hermetic tests for the MCP tool handlers (no SDK subprocess, no network)."""
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
    out = await submit_verdict_impl({"product_name": "x"}, holder)
    assert holder.verdict is None
    assert "invalid" in out.lower()


def test_verdict_schema_matches_model():
    schema = tools.VERDICT_SCHEMA
    assert schema["type"] == "object"
    for field in ("reasoning", "product_name", "summary", "green", "amber", "red", "recommendation"):
        assert field in schema["properties"]


def test_server_has_five_tools():
    holder = VerdictHolder()
    server = tools.build_nutrition_server(holder)
    assert server is not None
    assert len(tools.TOOL_NAMES) == 5
    assert "mcp__nutrition__submit_verdict" in tools.TOOL_NAMES
