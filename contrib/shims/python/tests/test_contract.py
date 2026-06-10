import base64
from typing import AsyncIterator
from fastapi.testclient import TestClient

from runtime_contract.app import create_app
from runtime_contract.events import ContractEvent
from runtime_contract.store import Store


class FakeAdapter:
    async def run(self, session_id, message, images, history) -> AsyncIterator[ContractEvent]:
        yield ContractEvent(type="text", text="hello " + message)
        yield ContractEvent(type="tool_result", text="ran a tool")


def make_client(tmp_path):
    store = Store(str(tmp_path / "shim.db"))
    app = create_app(FakeAdapter(), store, "fake")
    return TestClient(app), store


def test_healthz_and_meta(tmp_path):
    c, _ = make_client(tmp_path)
    assert c.get("/healthz").text == "ok"
    m = c.get("/meta").json()
    assert m["agent_id"] == "fake" and m["contract_version"] == "v1"


def test_session_stream_to_done(tmp_path):
    c, _ = make_client(tmp_path)
    sid = c.post("/sessions", json={"message": "world"}).json()["session_id"]
    assert sid
    body = c.get(f"/sessions/{sid}/stream?since=0").text
    # Compact JSON (no space after colon) — must match the Go WireEvent wire
    # format, which the conformance suite substring-matches as `"type":"done"`.
    assert '"type":"text"' in body and "hello world" in body
    assert '"type":"tool_result"' in body
    assert '"type":"done"' in body
    assert "id: 1" in body
    row = c.get(f"/sessions/{sid}").json()
    assert row["status"] == "completed"
    rows = c.get("/sessions").json()
    assert any(r["id"] == sid for r in rows)


def test_follow_up_message_same_session(tmp_path):
    c, _ = make_client(tmp_path)
    sid = c.post("/sessions", json={"message": "one"}).json()["session_id"]
    first = c.get(f"/sessions/{sid}/stream?since=0").text
    assert "hello one" in first and '"type":"done"' in first
    # done seq for turn 1 = 3 (text, tool_result, done)
    r = c.post(f"/sessions/{sid}/messages", json={"message": "two"})
    assert r.status_code == 200 and r.json()["session_id"] == sid
    second = c.get(f"/sessions/{sid}/stream?since=3").text
    assert "hello two" in second and '"type":"done"' in second
    assert "hello one" not in second
    row = c.get(f"/sessions/{sid}").json()
    assert row["status"] == "completed"
    assert row["turn_count"] == 2


def test_follow_up_unknown_session_404(tmp_path):
    c, _ = make_client(tmp_path)
    r = c.post("/sessions/ses-nope/messages", json={"message": "x"})
    assert r.status_code == 404


def test_replay_since(tmp_path):
    c, _ = make_client(tmp_path)
    sid = c.post("/sessions", json={"message": "x"}).json()["session_id"]
    full = c.get(f"/sessions/{sid}/stream?since=0").text
    tail = c.get(f"/sessions/{sid}/stream?since=1").text
    assert "hello x" not in tail
    assert '"type":"done"' in tail


def test_image_decoded(tmp_path):
    captured = {}

    class ImgAdapter:
        async def run(self, session_id, message, images, history):
            captured["n"] = len(images)
            if images:
                captured["mime"] = images[0].mime
            yield ContractEvent(type="text", text="ok")

    store = Store(str(tmp_path / "db.sqlite"))
    c = TestClient(create_app(ImgAdapter(), store, "img"))
    b64 = base64.b64encode(b"\xff\xd8fake").decode()
    sid = c.post("/sessions", json={"message": "m", "image_b64": b64, "image_mime": "image/png"}).json()["session_id"]
    c.get(f"/sessions/{sid}/stream?since=0").text
    assert captured.get("n") == 1
    assert captured.get("mime") == "image/png"
