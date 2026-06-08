"""serve() reads operator env and resolves the adapter (instance or factory)."""
from typing import AsyncIterator

import pytest
import uvicorn

from runtime_contract import serve
from runtime_contract.events import ContractEvent


class StubAdapter:
    def __init__(self, db_path: str):
        self.db_path = db_path

    async def run(self, session_id, message, images, history) -> AsyncIterator[ContractEvent]:
        yield ContractEvent(type="text", text="ok")


def _capture_uvicorn(monkeypatch):
    """Replace uvicorn.run so serve() returns instead of blocking; capture args."""
    captured = {}

    def fake_run(app, host, port, log_level):  # noqa: ANN001
        captured["app"] = app
        captured["host"] = host
        captured["port"] = port

    monkeypatch.setattr(uvicorn, "run", fake_run)
    return captured


def test_requires_listen_addr(monkeypatch):
    monkeypatch.delenv("RUNTIME_LISTEN_ADDR", raising=False)
    with pytest.raises(RuntimeError, match="RUNTIME_LISTEN_ADDR"):
        serve(StubAdapter)


def test_factory_gets_db_path(monkeypatch, tmp_path):
    cap = _capture_uvicorn(monkeypatch)
    db = str(tmp_path / "shim.db")
    monkeypatch.setenv("RUNTIME_LISTEN_ADDR", "127.0.0.1:9311")
    monkeypatch.setenv("RUNTIME_SHIM_DB", db)
    monkeypatch.setenv("RUNTIME_AGENT_ID", "stub")

    built = {}
    orig_init = StubAdapter.__init__

    def spy_init(self, db_path):
        built["db_path"] = db_path
        orig_init(self, db_path)

    monkeypatch.setattr(StubAdapter, "__init__", spy_init)
    serve(StubAdapter)

    assert built["db_path"] == db
    assert cap["host"] == "127.0.0.1"
    assert cap["port"] == 9311


def test_accepts_ready_instance(monkeypatch, tmp_path):
    cap = _capture_uvicorn(monkeypatch)
    monkeypatch.setenv("RUNTIME_LISTEN_ADDR", "0.0.0.0:8080")
    monkeypatch.setenv("RUNTIME_SHIM_DB", str(tmp_path / "shim.db"))
    instance = StubAdapter(str(tmp_path / "shim.db"))
    # Passing an already-built adapter must NOT try to call it as a factory.
    serve(instance)
    assert cap["host"] == "0.0.0.0"
    assert cap["port"] == 8080
