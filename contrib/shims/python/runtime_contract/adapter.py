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
        """
        ...
