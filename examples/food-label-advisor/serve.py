"""Entry point: serve the Food Label Advisor over the runtime contract.

runtimed execs this (config command/workdir) as a supervised agent. Operator
parameters come from the injected RUNTIME_* env via runtime_contract.serve;
this file only builds the adapter. The factory form shares RUNTIME_SHIM_DB
between the contract store and the SDK session map.
"""
from __future__ import annotations

import os

from dotenv import load_dotenv

load_dotenv()  # .env: ANTHROPIC_API_KEY / ANTHROPIC_BASE_URL / ANTHROPIC_MODEL

from runtime_contract import serve  # noqa: E402

from adapter import FoodLabelAdvisorAdapter  # noqa: E402


def main() -> None:
    print(
        f"serving agent {os.environ.get('RUNTIME_AGENT_ID', 'food-label-advisor')} "
        f"with ANTHROPIC_MODEL={os.environ.get('ANTHROPIC_MODEL', '(unset!)')}",
        flush=True,
    )
    serve(FoodLabelAdvisorAdapter)


if __name__ == "__main__":
    main()
