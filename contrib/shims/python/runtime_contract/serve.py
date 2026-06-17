"""serve(): the operator-glue entry point for a runtime-hosted Python agent.

This mirrors the Go `agentruntime.Serve`: operator parameters are read from the
environment the control plane injects, NOT handed in by the adapter author. A
consumer entrypoint shrinks to "build adapter -> serve(adapter)".

Operator environment (set by runtimed on the supervised subprocess):

  RUNTIME_LISTEN_ADDR   host:port to bind (required; runtimed always injects it)
  RUNTIME_AGENT_ID      this agent's id, surfaced on /meta (default "agent")
  RUNTIME_SHIM_DB       SQLite path for the Level-1 store (optional; the control
                        plane does NOT inject it, so it defaults to ./shim.db
                        under the agent's workdir)

The adapter author never reads these. The only seam the author owns is the
adapter itself (the AgentAdapter protocol).
"""
from __future__ import annotations

import os
from typing import Callable, Union

import uvicorn

from .adapter import AgentAdapter
from .app import create_app
from .metrics import Metrics
from .store import Store

# An adapter may be supplied directly, or as a factory taking the resolved
# SQLite path (so the adapter can key its own per-session store on the same db
# the library uses for Level-1 durability — e.g. an SDK's SQLiteSession).
AdapterOrFactory = Union[AgentAdapter, Callable[[str], AgentAdapter]]


def serve(adapter: AdapterOrFactory) -> None:
    """Read operator env, build the Store + app, and run uvicorn until stopped.

    `adapter` is either a ready AgentAdapter, or a callable `make(db_path)` that
    returns one. The factory form lets the adapter share the resolved db path
    (RUNTIME_SHIM_DB) with the contract store. A plain class works as a factory
    when its constructor takes the db path positionally (e.g.
    `NutritionAdapter(db_path)`), so `serve(NutritionAdapter)` is the common case.
    """
    addr = os.environ.get("RUNTIME_LISTEN_ADDR")
    if not addr:
        raise RuntimeError("runtime_contract.serve: RUNTIME_LISTEN_ADDR is not set")
    host, _, port = addr.partition(":")
    agent_id = os.environ.get("RUNTIME_AGENT_ID", "agent")
    db = os.environ.get("RUNTIME_SHIM_DB", "./shim.db")

    # Resolve the adapter. A factory (incl. a class taking db_path) is called
    # with the db path; an already-constructed adapter is used as-is.
    if callable(adapter) and not _is_adapter_instance(adapter):
        resolved = adapter(db)
    else:
        resolved = adapter  # type: ignore[assignment]

    store = Store(db)
    # Always-on Prometheus metrics: in-process and cheap, served at /metrics on
    # the agent's own port and scraped by the control plane over the VPC — same
    # trust model as the Go agentruntime emitter.
    metrics = Metrics(agent_id)
    app = create_app(resolved, store, agent_id, metrics=metrics)
    uvicorn.run(app, host=host or "127.0.0.1", port=int(port or "8000"), log_level="info")


def _is_adapter_instance(obj: object) -> bool:
    """True if obj is already an adapter instance (has an async-capable run()),
    rather than a class/factory to be called with the db path. A class also has
    `run` (unbound), so we additionally require it not be a type."""
    return not isinstance(obj, type) and hasattr(obj, "run")
