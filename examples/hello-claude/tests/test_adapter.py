"""Adapter tests with a faked claude_agent_sdk.query (no subprocess, no network)."""
import pytest

import adapter as adapter_mod
from adapter import HelloClaudeAdapter


class FakeText:
    """Stands in for a TextBlock (the adapter isinstance-checks against TextBlock)."""

    def __init__(self, text):
        self.text = text


class FakeAssistant:
    def __init__(self, *blocks):
        self.content = list(blocks)


class FakeResult:
    def __init__(self, session_id="sdk-1", is_error=False, result="", subtype="success"):
        self.session_id = session_id
        self.is_error = is_error
        self.result = result
        self.subtype = subtype


def fake_query(*, text="hi there", session_id="sdk-1", is_error=False,
               raise_exc=None, capture=None):
    """Async-gen factory mimicking query(prompt=..., options=...)."""
    async def _gen(prompt=None, options=None):
        if capture is not None:
            capture["prompt"] = prompt
            capture["options"] = options
        if raise_exc:
            raise raise_exc
        if text is not None:
            yield FakeAssistant(FakeText(text))
        yield FakeResult(session_id=session_id, is_error=is_error)
    return _gen


@pytest.fixture
def adp(tmp_path, monkeypatch):
    monkeypatch.setenv("ANTHROPIC_MODEL", "test-model")
    # Point the adapter's isinstance checks at the fakes.
    monkeypatch.setattr(adapter_mod, "AssistantMessage", FakeAssistant)
    monkeypatch.setattr(adapter_mod, "TextBlock", FakeText)
    monkeypatch.setattr(adapter_mod, "ResultMessage", FakeResult)
    return HelloClaudeAdapter(str(tmp_path / "shim.db"))


async def collect(agen):
    return [e async for e in agen]


async def test_text_turn_yields_one_text_event(adp, monkeypatch):
    monkeypatch.setattr(adapter_mod, "query", fake_query(text="hello, world"))
    events = await collect(adp.run("ses-1", "say hi", [], []))
    assert len(events) == 1 and events[0].type == "text"
    assert events[0].text == "hello, world"


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


async def test_error_result_yields_error_event(adp, monkeypatch):
    monkeypatch.setattr(adapter_mod, "query", fake_query(text=None, is_error=True))
    events = await collect(adp.run("ses-1", "go", [], []))
    assert len(events) == 1 and events[0].type == "error"


async def test_never_raises_on_exception(adp, monkeypatch):
    monkeypatch.setattr(adapter_mod, "query", fake_query(raise_exc=RuntimeError("boom")))
    events = await collect(adp.run("ses-1", "go", [], []))
    assert len(events) == 1 and events[0].type == "error"
    assert "boom" in events[0].error
