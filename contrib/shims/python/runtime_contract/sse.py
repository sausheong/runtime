"""SSE framing matching the Go agentruntime: 'id: <seq>\ndata: <json>\n\n'."""
from __future__ import annotations
from .events import ContractEvent


def frame(seq: int, ev: ContractEvent) -> str:
    return f"id: {seq}\ndata: {ev.to_json()}\n\n"
