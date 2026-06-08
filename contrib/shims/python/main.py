"""Entry point: serve the OpenAI Agents SDK agent over the runtime contract."""
from __future__ import annotations

import os

import uvicorn

from runtime_contract.app import create_app
from runtime_contract.store import Store
from adapters.openai_agents import OpenAIAgentsAdapter


def main() -> None:
    addr = os.environ.get("RUNTIME_LISTEN_ADDR", "127.0.0.1:8301")
    host, _, port = addr.partition(":")
    agent_id = os.environ.get("RUNTIME_AGENT_ID", "openai")
    db = os.environ.get("RUNTIME_SHIM_DB", "./shim.db")

    store = Store(db)
    adapter = OpenAIAgentsAdapter(db_path=db)
    app = create_app(adapter, store, agent_id)
    uvicorn.run(
        app,
        host=host or "127.0.0.1",
        port=int(port or "8301"),
        log_level="info",
    )


if __name__ == "__main__":
    main()
