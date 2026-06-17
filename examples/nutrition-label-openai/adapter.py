"""Adapter: the SG Nutrition Investigator agent -> runtime contract events.

Implements the runtime_contract AgentAdapter protocol. Drives the shared agent
(agent.py) for one invocation and yields a single text event carrying the
verdict rendered as prose (the same block main.py prints). The validated typed
NutritionVerdict is produced and checked by the SDK; we serialize it to prose
for transport over the text-based contract. Never raises — failures become one
error event; the library appends the terminal done/error.
"""
from __future__ import annotations

import base64
from typing import AsyncIterator, Sequence

from agents import Runner, SQLiteSession

from runtime_contract.events import ContractEvent, Image
from agent import build_agent, render_verdict, remember_verdict, NutritionVerdict


class NutritionAdapter:
    """AgentAdapter backed by the OpenAI Agents SDK nutrition agent."""

    def __init__(self, db_path: str):
        # build_agent() reads OPENAI_API_KEY here, so constructing the adapter
        # (done once at serve.py startup) fails fast if the key is missing.
        self._db = db_path
        self._agent = build_agent()

    async def run(
        self,
        session_id: str,
        message: str,
        images: Sequence[Image],
        history: Sequence[ContractEvent],
    ) -> AsyncIterator[ContractEvent]:
        # `history` is unused: conversation memory is owned by the SQLiteSession
        # below (keyed on session_id), not replayed from the contract event log.
        # Build the SDK input: text, or a content-list with the image data URL
        # (same shape as main.py's investigate()).
        if images:
            img = images[0]
            data_url = f"data:{img.mime};base64,{base64.b64encode(img.data).decode()}"
            user_input = [
                {
                    "role": "user",
                    "content": [
                        {"type": "input_text", "text": message or "Investigate this nutrition label."},
                        {"type": "input_image", "image_url": data_url},
                    ],
                }
            ]
        else:
            user_input = message

        # SQLiteSession keyed on the runtime session id gives Level-1
        # conversation memory in the same shim db.
        session = SQLiteSession(session_id, self._db)
        try:
            # Non-streamed: with output_type=NutritionVerdict the SDK returns the
            # validated structured object at the end (not output_text deltas).
            result = await Runner.run(self._agent, input=user_input, session=session)
            # Telemetry (metrics only; never reaches the client stream). Extraction
            # is best-effort: a change in the SDK result shape must never break the
            # turn, so it's wrapped and yielded before the text result.
            for ev in _telemetry_events(result):
                yield ev
            v: NutritionVerdict = result.final_output
            remember_verdict(v)  # learn across sessions, like the CLI across runs
            yield ContractEvent(type="text", text=render_verdict(v))
        except Exception as e:  # never raise out of run(); surface as one error
            yield ContractEvent(type="error", error=str(e))


def _telemetry_events(result) -> list[ContractEvent]:
    """Best-effort tool_call + usage telemetry from a RunResult. Returns [] on any
    shape mismatch — telemetry must never break a turn."""
    out: list[ContractEvent] = []
    try:
        for item in getattr(result, "new_items", []) or []:
            # ToolCallItem exposes .tool_name; other run items are skipped.
            name = getattr(item, "tool_name", None)
            if name:
                out.append(ContractEvent(type="tool_call", tool=name))
    except Exception:
        pass
    try:
        usage = getattr(getattr(result, "context_wrapper", None), "usage", None)
        if usage is not None:
            out.append(ContractEvent(type="usage", usage={
                "input": int(getattr(usage, "input_tokens", 0) or 0),
                "output": int(getattr(usage, "output_tokens", 0) or 0),
            }))
    except Exception:
        pass
    return out
