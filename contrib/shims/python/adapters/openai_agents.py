"""Adapter: OpenAI Agents SDK -> runtime contract events.

Drives the OpenAI Agents SDK behind runtime's HTTP/SSE contract. The agent is
built from environment variables at construction time, run streamed with an
``SQLiteSession`` keyed by the runtime session id (Level-1 conversation memory),
and the SDK's stream is mapped to ``ContractEvent`` instances.

Streaming choice: the SDK's streamed run yields ``RawResponsesStreamEvent``
objects whose ``.data`` is a raw OpenAI Responses streaming event. Assistant
text arrives as ``response.output_text.delta`` events with a string ``.delta``.
We forward exactly those as incremental ``text`` events. We deliberately guard
on the inner event ``type`` so that other delta-bearing events (e.g. tool-call
argument deltas, which also have a ``.delta`` string) do NOT leak into the
assistant text. If the stream produced no text deltas (e.g. a model that does
not stream output_text), we fall back to emitting ``final_output`` once so the
verdict is always surfaced exactly once.
"""
from __future__ import annotations

import base64
import os
from typing import AsyncIterator, Sequence

from agents import (
    Agent,
    Runner,
    AsyncOpenAI,
    OpenAIChatCompletionsModel,
    set_tracing_disabled,
    SQLiteSession,
)

from runtime_contract.events import ContractEvent, Image

# The proxy/base-url key won't authenticate against OpenAI's trace ingest, and
# the shim must never phone home. Disable tracing at import time.
set_tracing_disabled(True)

INSTRUCTIONS = (
    "You are a Singapore food label investigator. Given a label (as text or an "
    "image), read the product, additives, and sugar/saturated-fat values, and "
    "return a plain-prose verdict: reasoning, a one-line summary, findings grouped "
    "GREEN/AMBER/RED, then a recommendation. If you cannot read it, say so."
)


def _build_agent() -> Agent:
    """Construct the Agent from environment.

    Reads ``OPENAI_API_KEY`` (required), ``OPENAI_BASE_URL`` (optional), and
    ``OPENAI_MODEL`` (default ``gpt-4o``). Building the AsyncOpenAI client and
    the Agent makes no network call; the model is only contacted on run().
    """
    key = os.environ["OPENAI_API_KEY"]
    base = os.environ.get("OPENAI_BASE_URL") or None
    model = os.environ.get("OPENAI_MODEL", "gpt-4o")
    client = AsyncOpenAI(base_url=base, api_key=key)
    return Agent(
        name="OpenAI SDK Nutrition Agent",
        model=OpenAIChatCompletionsModel(model=model, openai_client=client),
        instructions=INSTRUCTIONS,
    )


class OpenAIAgentsAdapter:
    """AgentAdapter implementation backed by the OpenAI Agents SDK."""

    def __init__(self, db_path: str):
        self._db = db_path
        # _build_agent() reads OPENAI_API_KEY here, so importing this module does
        # not require the key — only constructing the adapter does.
        self._agent = _build_agent()

    async def run(
        self,
        session_id: str,
        message: str,
        images: Sequence[Image],
        history: Sequence[ContractEvent],
    ) -> AsyncIterator[ContractEvent]:
        # Build the user input. For vision, mirror the original demo's content
        # shape: an input_text part plus an input_image data URL.
        if images:
            img = images[0]
            data_url = (
                f"data:{img.mime};base64,"
                f"{base64.b64encode(img.data).decode()}"
            )
            user_input = [
                {
                    "role": "user",
                    "content": [
                        {
                            "type": "input_text",
                            "text": message or "Investigate this label.",
                        },
                        {"type": "input_image", "image_url": data_url},
                    ],
                }
            ]
        else:
            user_input = message

        # SQLiteSession gives Level-1 conversation memory keyed by the runtime
        # session id, persisted in the shim's SQLite db.
        session = SQLiteSession(session_id, self._db)
        try:
            result = Runner.run_streamed(
                self._agent, input=user_input, session=session
            )
            emitted_any = False
            async for ev in result.stream_events():
                # Only raw response events carry token-level model output.
                if getattr(ev, "type", None) != "raw_response_event":
                    continue
                data = getattr(ev, "data", None)
                # Guard on the inner Responses event type so that only assistant
                # text deltas are forwarded (not tool-call argument deltas etc.).
                if getattr(data, "type", None) != "response.output_text.delta":
                    continue
                delta = getattr(data, "delta", None)
                if isinstance(delta, str) and delta:
                    emitted_any = True
                    yield ContractEvent(type="text", text=delta)

            # Fallback: if no text deltas streamed, surface the final output once
            # so the verdict is always emitted exactly once.
            if not emitted_any:
                final = getattr(result, "final_output", None)
                if final is not None:
                    yield ContractEvent(type="text", text=str(final))
        except Exception as e:  # never raise out of run(); surface as one error
            yield ContractEvent(type="error", error=str(e))
