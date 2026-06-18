"""Adapter: Food Label Advisor (Claude Agent SDK) -> runtime contract.

Per turn: look up the SDK session for this runtime session, build a fresh
AdvisorHolder, drive query() with all label images and the user message,
capture the AdvisorResult via the holder, persist the SDK session id, and
yield ONE text event (rendered comparison) or ONE error event.
Never raises out of run(); the contract library appends the terminal
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
from agent import render_comparison
from sessions import SessionMap
from tools import AdvisorHolder, build_advisor_server, TOOL_NAMES

HERE = Path(__file__).resolve().parent

# Network-facing agent: disable all CLI built-ins.
BUILTINS_OFF = [
    "Bash", "Read", "Write", "Edit", "Glob", "Grep",
    "WebFetch", "WebSearch", "NotebookEdit", "TodoWrite", "Task",
]


class FoodLabelAdvisorAdapter:
    """AgentAdapter backed by the Claude Agent SDK food label advisor."""

    def __init__(self, db_path: str):
        self._sessions = SessionMap(db_path)
        self._config_dir = str(Path(db_path).resolve().parent / "claude-config")
        self._model = os.environ["ANTHROPIC_MODEL"]   # fail fast at startup

    def _options(self, holder: AdvisorHolder, resume: str | None) -> ClaudeAgentOptions:
        return ClaudeAgentOptions(
            model=self._model,
            resume=resume,
            system_prompt=agent.INSTRUCTIONS,
            mcp_servers={"advisor": build_advisor_server(holder)},
            tools=[],                       # disables ALL CLI built-ins
            allowed_tools=list(TOOL_NAMES),
            disallowed_tools=list(BUILTINS_OFF),
            permission_mode="dontAsk",
            cwd=str(HERE),
            env={
                "CLAUDE_CONFIG_DIR": self._config_dir,
                "CLAUDE_CODE_DISABLE_AUTO_MEMORY": "1",
            },
            setting_sources=[],
            max_turns=30,
        )

    @staticmethod
    def _prompt(message: str, images: Sequence[Image]):
        """Build a multi-image user message for the SDK."""
        if not images:
            return message or "Please analyse these food labels."

        content: list[dict] = [
            {"type": "text", "text": message or "Please analyse these food labels."}
        ]
        for img in images:
            b64 = base64.b64encode(img.data).decode()
            content.append({
                "type": "image",
                "source": {"type": "base64", "media_type": img.mime, "data": b64},
            })

        async def gen():
            yield {
                "type": "user",
                "message": {"role": "user", "content": content},
            }

        return gen()

    async def run(
        self,
        session_id: str,
        message: str,
        images: Sequence[Image],
        history: Sequence[ContractEvent],
    ) -> AsyncIterator[ContractEvent]:
        holder = AdvisorHolder()
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

            usage_ev = _usage_event(result)
            if usage_ev is not None:
                yield usage_ev

            # Prefer the structured comparison result if the agent completed it.
            if holder.result is not None and holder.result.comparison is not None:
                yield ContractEvent(type="text", text=render_comparison(holder.result.comparison))
                return

            if result is not None and result.is_error:
                yield ContractEvent(
                    type="error",
                    error=f"agent run failed ({result.subtype}): {result.result or ''}",
                )
                return

            fallback = (result.result if result and result.result else "".join(text_parts))
            yield ContractEvent(type="text", text=fallback or "(no output)")

        except Exception as e:   # never raise out of run()
            yield ContractEvent(type="error", error=str(e))


def _usage_event(result) -> ContractEvent | None:
    """Best-effort token-usage telemetry from a ResultMessage.usage dict.
    Returns None on any mismatch — telemetry never breaks a turn."""
    try:
        u = getattr(result, "usage", None)
        if not u:
            return None
        cache_read = int(u.get("cache_read_input_tokens", 0) or 0)
        cache_creation = int(u.get("cache_creation_input_tokens", 0) or 0)
        return ContractEvent(type="usage", usage={
            # Roll cache_read into input so the console shows total tokens the
            # model actually read, not just the tiny non-cached fraction.
            "input":          int(u.get("input_tokens", 0) or 0) + cache_read,
            "output":         int(u.get("output_tokens", 0) or 0),
            "cache_creation": cache_creation,
            "cache_read":     cache_read,
        })
    except Exception:
        return None
