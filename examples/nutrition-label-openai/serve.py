"""Entry point: serve the SG Nutrition Investigator over the runtime contract.

runtimed execs this (via the config's command/workdir) as a supervised agent.
Operator parameters (listen address, the Level-1 SQLite path, the agent id) are
read from the injected RUNTIME_* env by runtime_contract.serve — this file only
has to build the agent. NutritionAdapter is passed as a factory: serve() calls
NutritionAdapter(db_path) so the adapter keys its SQLiteSession on the same db
the contract store uses.
"""
from __future__ import annotations

import os

from runtime_contract import serve

from adapter import NutritionAdapter


def main() -> None:
    # The control plane's /agents listing shows the config's `model:` string,
    # but the SDK actually runs OPENAI_MODEL (default gpt-4o). Print the resolved
    # model at startup so the two are never silently out of step. (Plain print —
    # uvicorn's loggers aren't configured until serve() runs uvicorn.)
    print(
        f"serving agent {os.environ.get('RUNTIME_AGENT_ID', 'nutrition-openai')} "
        f"with OPENAI_MODEL={os.environ.get('OPENAI_MODEL', 'gpt-4o')}",
        flush=True,
    )

    # NutritionAdapter(db_path) builds the agent (fails fast if OPENAI_API_KEY is
    # missing). serve() resolves db_path from RUNTIME_SHIM_DB and binds
    # RUNTIME_LISTEN_ADDR.
    serve(NutritionAdapter)


if __name__ == "__main__":
    main()
