"""Adapter tests with a faked claude_agent_sdk.query (no subprocess, no network)."""
import pytest

import adapter as adapter_mod
from adapter import NutritionClaudeAdapter
from agent import NutritionVerdict
from runtime_contract.events import Image


VALID = dict(
    reasoning="r", product_name="Milo", summary="s",
    green=[], amber=[], red=[], recommendation="ok",
)


class FakeResult:
    def __init__(self, session_id="sdk-1", is_error=False, result="fallback text", subtype="success"):
        self.session_id = session_id
        self.is_error = is_error
        self.result = result
        self.subtype = subtype


def fake_query(*, submit=True, session_id="sdk-1", is_error=False, raise_exc=None, capture=None):
    """Async-gen factory mimicking query(prompt=..., options=...)."""
    async def _gen(prompt=None, options=None):
        if capture is not None:
            capture["prompt"] = prompt
            capture["options"] = options
        if raise_exc:
            raise raise_exc
        if submit:
            adapter_mod._test_last_holder.verdict = NutritionVerdict(**VALID)
        yield FakeResult(session_id=session_id, is_error=is_error)
    return _gen


@pytest.fixture
def adp(tmp_path, monkeypatch):
    monkeypatch.setenv("ANTHROPIC_MODEL", "test-model")
    # FakeResult is not a real ResultMessage; point the adapter's isinstance at it.
    monkeypatch.setattr(adapter_mod, "ResultMessage", FakeResult)
    return NutritionClaudeAdapter(str(tmp_path / "shim.db"))


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
    assert not isinstance(cap["prompt"], str)  # streaming-input form, not a plain string


async def test_error_result_yields_error_event(adp, monkeypatch):
    monkeypatch.setattr(adapter_mod, "query", fake_query(submit=False, is_error=True))
    events = await collect(adp.run("ses-1", "go", [], []))
    assert events[-1].type == "error"


async def test_no_verdict_falls_back_to_text(adp, monkeypatch):
    monkeypatch.setattr(adapter_mod, "query", fake_query(submit=False))
    events = await collect(adp.run("ses-1", "go", [], []))
    assert events[0].type == "text"
    assert "fallback text" in events[0].text


async def test_exception_becomes_single_error_event(adp, monkeypatch):
    monkeypatch.setattr(adapter_mod, "query", fake_query(raise_exc=RuntimeError("boom")))
    events = await collect(adp.run("ses-1", "go", [], []))
    assert len(events) == 1 and events[0].type == "error" and "boom" in events[0].error


class FakeTextBlock:
    def __init__(self, text):
        self.text = text


class FakeAssistantMessage:
    def __init__(self, *texts):
        self.content = [FakeTextBlock(t) for t in texts]


async def test_accumulated_text_fallback(adp, monkeypatch):
    # No verdict, no ResultMessage.result: fallback = accumulated assistant text.
    def fq():
        async def _gen(prompt=None, options=None):
            yield FakeAssistantMessage("part one. ", "part two.")
            yield FakeResult(result=None)
        return _gen
    monkeypatch.setattr(adapter_mod, "query", fq())
    monkeypatch.setattr(adapter_mod, "AssistantMessage", FakeAssistantMessage)
    monkeypatch.setattr(adapter_mod, "TextBlock", FakeTextBlock)
    events = await collect(adp.run("ses-1", "go", [], []))
    assert events[0].type == "text" and "part one. part two." in events[0].text


async def test_image_prompt_block_shape(adp, monkeypatch):
    cap = {}
    monkeypatch.setattr(adapter_mod, "query", fake_query(capture=cap))
    img = Image(mime="image/png", data=b"fakepng")
    await collect(adp.run("ses-1", "look", [img], []))
    msgs = [m async for m in cap["prompt"]]
    assert len(msgs) == 1
    content = msgs[0]["message"]["content"]
    assert content[0]["type"] == "text"
    assert content[1]["type"] == "image"
    assert content[1]["source"]["media_type"] == "image/png"
    assert content[1]["source"]["type"] == "base64"
