"""Hermetic tests for the SDK-free domain module."""
import json

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
    entry = _resolve_additive("E211")
    assert entry is not None and "benzoate" in entry["name"].lower()


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
    agent._MEMORY["learned_aliases"]["zingo"] = "211"
    assert _resolve_additive("zingo") is not None


def test_remember_verdict_persists():
    remember_verdict(make_verdict())
    saved = json.loads(agent._MEMORY_PATH.read_text())
    assert saved["products"]["milo"]["summary"] == "s"


def test_instructions_mention_submit_verdict():
    assert "submit_verdict" in agent.INSTRUCTIONS
