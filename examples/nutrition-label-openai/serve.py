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
