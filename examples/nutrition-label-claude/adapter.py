"""Adapter: SG Nutrition Investigator (Claude Agent SDK) -> runtime contract.

The C1-M2 exhibit: hosting a Claude Agent SDK agent required ONLY this file
(plus domain/tool/session modules) — runtime_contract is consumed unchanged.

Per turn: look up the SDK session for this runtime session, drive query()
with resume=, capture the submit_verdict payload via a turn-scoped holder,
persist the SDK session id, and yield ONE text event (rendered verdict) or
ONE error event. Never raises out of run(); the library appends the terminal
done/error lifecycle event.
"""
from __future__ import annotations

import base64
import os
from pathlib import Path
from typing import AsyncIterator, Sequence

from claude_agent_sdk import (
    query,
    ClaudeAgentOptions,
    AssistantMessage,
    TextBlock,
    ResultMessage,
)

from runtime_contract.events import ContractEvent, Image

import agent
from agent import render_verdict, remember_verdict
from sessions import SessionMap
from tools import VerdictHolder, build_nutrition_server, TOOL_NAMES

HERE = Path(__file__).resolve().parent

# Built-ins stripped from a network-facing agent; the spike (spike_vision.py)
# verified this combination leaves only the nutrition MCP tools callable.
BUILTINS_OFF = [
    "Bash", "Read", "Write", "Edit", "Glob", "Grep",
    "WebFetch", "WebSearch", "NotebookEdit", "TodoWrite", "Task",
]

# Test seam: tests fake query() and need the current turn's holder.
_test_last_holder: VerdictHolder | None = None


class NutritionClaudeAdapter:
    """AgentAdapter backed by the Claude Agent SDK nutrition agent."""

    def __init__(self, db_path: str):
        self._sessions = SessionMap(db_path)
        # Transcript home pinned NEXT TO the shim db: resume is keyed by
        # (CLAUDE_CONFIG_DIR, cwd), so both must be stable across restarts.
        self._config_dir = str(Path(db_path).resolve().parent / "claude-config")
        self._model = os.environ["ANTHROPIC_MODEL"]  # fail fast at startup

    def _options(self, holder: VerdictHolder, resume: str | None) -> ClaudeAgentOptions:
        return ClaudeAgentOptions(
            model=self._model,
            resume=resume,
            system_prompt=agent.INSTRUCTIONS,
            mcp_servers={"nutrition": build_nutrition_server(holder)},
            allowed_tools=list(TOOL_NAMES),
            disallowed_tools=list(BUILTINS_OFF),
            permission_mode="dontAsk",
            cwd=str(HERE),
            env={
                "CLAUDE_CONFIG_DIR": self._config_dir,
                "CLAUDE_CODE_DISABLE_AUTO_MEMORY": "1",
            },
            setting_sources=[],
            max_turns=25,
        )

    @staticmethod
    def _prompt(message: str, images: Sequence[Image]):
        if not images:
            return message or "Investigate this nutrition label."
        img = images[0]
        b64 = base64.b64encode(img.data).decode()

        async def gen():
            yield {
                "type": "user",
                "message": {
                    "role": "user",
                    "content": [
                        {"type": "text", "text": message or "Investigate this nutrition label."},
                        {"type": "image",
                         "source": {"type": "base64", "media_type": img.mime, "data": b64}},
                    ],
                },
            }

        return gen()

    async def run(
        self,
        session_id: str,
        message: str,
        images: Sequence[Image],
        history: Sequence[ContractEvent],
    ) -> AsyncIterator[ContractEvent]:
        # `history` unused: the SDK's own transcripts (resume=) own memory,
        # the same stance the OpenAI adapter took with SQLiteSession.
        global _test_last_holder
        holder = VerdictHolder()
        _test_last_holder = holder
        try:
            resume = self._sessions.lookup(session_id)
            opts = self._options(holder, resume)
            text_parts: list[str] = []
            result = None
            async for msg in query(prompt=self._prompt(message, images), options=opts):
                if isinstance(msg, AssistantMessage):
                    for block in msg.content:
                        if isinstance(block, TextBlock):
                            text_parts.append(block.text)
                elif isinstance(msg, ResultMessage):
                    result = msg
            if result is not None and result.session_id:
                self._sessions.store(session_id, result.session_id)
            if holder.verdict is not None:
                remember_verdict(holder.verdict)
                yield ContractEvent(type="text", text=render_verdict(holder.verdict))
            elif result is not None and result.is_error:
                yield ContractEvent(type="error",
                                    error=f"agent run failed ({result.subtype}): {result.result or ''}")
            else:
                fallback = (result.result if result and result.result else "".join(text_parts))
                yield ContractEvent(type="text", text=fallback or "(no output)")
        except Exception as e:  # never raise out of run()
            yield ContractEvent(type="error", error=str(e))
