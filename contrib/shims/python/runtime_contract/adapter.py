"""The AgentAdapter seam: each framework implements run() to yield ContractEvents."""
from __future__ import annotations
from typing import Protocol, AsyncIterator, Sequence, runtime_checkable
from .events import ContractEvent, Image


@runtime_checkable
class AgentAdapter(Protocol):
    async def run(
        self,
        session_id: str,
        message: str,
        images: Sequence[Image],
        history: Sequence[ContractEvent],
    ) -> AsyncIterator[ContractEvent]:
        """Drive the underlying framework for one invocation, yielding events.

        The library frames/persists/fans-out the events; the adapter must never
        raise out of run() — surface failures as a ContractEvent(type="error").
        Do NOT emit the terminal 'done'/'error' lifecycle event; the library
        appends it based on whether run() completed or yielded an error event.

        Telemetry (optional): an adapter MAY additionally yield internal telemetry
        events that feed Prometheus metrics but are NEVER sent to the client or
        persisted:
          - ContractEvent(type="tool_call", tool="<name>") — one per tool call
            the framework made; recorded as agent_tool_calls_total{tool=...}.
          - ContractEvent(type="usage", usage={"input": N, "output": N,
            "cache_creation": N, "cache_read": N}) — token counts for the turn;
            recorded as agent_tokens_total{direction=...}. Yield at most one per
            turn (the last one wins).
        If an adapter yields neither, metrics are simply sparser — turn count and
        duration are still recorded by the library.
        """
        ...
