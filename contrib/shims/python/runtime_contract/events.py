"""Contract event vocabulary — mirrors the Go agentruntime.WireEvent JSON.

Two additional event types — "usage" and "tool_call" — are INTERNAL TELEMETRY,
not part of the client-facing wire contract. Adapters MAY yield them so the shim
can record token/tool metrics; app.py consumes them into Prometheus and does NOT
publish them to the SSE stream or persist them. Clients still see only
text/tool_result/done/error.
"""
from __future__ import annotations
from dataclasses import dataclass
from typing import Optional
import json

# Client-facing: "text" | "tool_result" | "done" | "error"
# Internal telemetry (never published): "usage" | "tool_call"
EventType = str


@dataclass
class ContractEvent:
    type: EventType
    text: str = ""
    error: str = ""
    # Telemetry-only fields (used by type=="tool_call" / type=="usage"). Ignored
    # for client-facing events and never reach the SSE stream.
    tool: str = ""
    usage: Optional[dict] = None

    def to_dict(self) -> dict:
        d: dict = {"type": self.type}
        if self.text:
            d["text"] = self.text
        if self.error:
            d["error"] = self.error
        if self.tool:
            d["tool"] = self.tool
        if self.usage:
            d["usage"] = self.usage
        return d

    def to_json(self) -> str:
        # Compact separators (no spaces) to match the Go agentruntime.WireEvent
        # wire format exactly — the conformance suite substring-matches
        # `"type":"done"`, and clients may do likewise.
        return json.dumps(self.to_dict(), separators=(",", ":"))


@dataclass
class Image:
    mime: str
    data: bytes
