"""Contract event vocabulary — mirrors the Go agentruntime.WireEvent JSON."""
from __future__ import annotations
from dataclasses import dataclass
import json

EventType = str  # "text" | "tool_result" | "done" | "error"


@dataclass
class ContractEvent:
    type: EventType
    text: str = ""
    error: str = ""

    def to_dict(self) -> dict:
        d: dict = {"type": self.type}
        if self.text:
            d["text"] = self.text
        if self.error:
            d["error"] = self.error
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
