"""Adapter: a minimal Claude Agent SDK assistant -> runtime contract.

The smallest useful Claude SDK agent: a plain conversational assistant with no
tools and no domain logic. It exists to show the shortest path from "a Claude
Agent SDK script" to "an agent hosted on runtime".

Per turn: look up the SDK session for this runtime session, drive query() with
resume= so follow-ups continue the conversation, collect the assistant's text,
persist the SDK session id, and yield ONE text event (or ONE error event).
Never raises out of run(); the contract library appends the terminal
done/error lifecycle event.
"""
from __future__ import annotations

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

from sessions import SessionMap

HERE = Path(__file__).resolve().parent

SYSTEM_PROMPT = "You are a friendly, concise assistant. Answer in a sentence or two."

# This is a network-facing agent with no need for the CLI's built-in tools, so
# disable them all. tools=[] is the primary control (empty list disables ALL
# built-ins); the deny-list is a belt-and-braces backup.
BUILTINS_OFF = [
    "Bash", "Read", "Write", "Edit", "Glob", "Grep",
    "WebFetch", "WebSearch", "NotebookEdit", "TodoWrite", "Task",
]


class HelloClaudeAdapter:
    """AgentAdapter backed by the Claude Agent SDK (no tools)."""

    def __init__(self, db_path: str):
        self._sessions = SessionMap(db_path)
        # Transcript home pinned NEXT TO the shim db: resume is keyed by
        # (CLAUDE_CONFIG_DIR, cwd), so both must be stable across restarts.
        self._config_dir = str(Path(db_path).resolve().parent / "claude-config")
        self._model = os.environ["ANTHROPIC_MODEL"]  # fail fast at startup

    def _options(self, resume: str | None) -> ClaudeAgentOptions:
        return ClaudeAgentOptions(
            model=self._model,
            resume=resume,
            system_prompt=SYSTEM_PROMPT,
            tools=[],  # primary control: [] disables ALL built-ins
            disallowed_tools=list(BUILTINS_OFF),  # backup deny-list
            permission_mode="dontAsk",
            cwd=str(HERE),
            env={
                "CLAUDE_CONFIG_DIR": self._config_dir,
                "CLAUDE_CODE_DISABLE_AUTO_MEMORY": "1",
            },
            setting_sources=[],
            max_turns=8,
        )

    async def run(
        self,
        session_id: str,
        message: str,
        images: Sequence[Image],
        history: Sequence[ContractEvent],
    ) -> AsyncIterator[ContractEvent]:
        # `history` unused: the SDK's own transcripts (resume=) own memory.
        # `images` unused: this minimal agent is text-only.
        try:
            resume = self._sessions.lookup(session_id)
            opts = self._options(resume)
            text_parts: list[str] = []
            result = None
            async for msg in query(
                prompt=message or "Hello!", options=opts
            ):
                if isinstance(msg, AssistantMessage):
                    for block in msg.content:
                        if isinstance(block, TextBlock):
                            text_parts.append(block.text)
                elif isinstance(msg, ResultMessage):
                    result = msg
            if result is not None and result.session_id:
                self._sessions.store(session_id, result.session_id)
            if result is not None and result.is_error:
                yield ContractEvent(
                    type="error",
                    error=f"agent run failed ({result.subtype}): {result.result or ''}",
                )
                return
            # Token telemetry (metrics only; never reaches the client stream).
            # Best-effort: a usage-shape change must never break the turn. No
            # tool_call events — this agent runs with tools=[].
            usage_ev = _usage_event(result)
            if usage_ev is not None:
                yield usage_ev
            text = "".join(text_parts) or (result.result if result and result.result else "")
            yield ContractEvent(type="text", text=text or "(no output)")
        except Exception as e:  # never raise out of run()
            yield ContractEvent(type="error", error=str(e))


def _usage_event(result) -> ContractEvent | None:
    """Best-effort token-usage telemetry from a ResultMessage.usage dict (standard
    Anthropic shape). Returns None on any mismatch — telemetry never breaks a turn."""
    try:
        u = getattr(result, "usage", None)
        if not u:
            return None
        return ContractEvent(type="usage", usage={
            "input": int(u.get("input_tokens", 0) or 0),
            "output": int(u.get("output_tokens", 0) or 0),
            "cache_creation": int(u.get("cache_creation_input_tokens", 0) or 0),
            "cache_read": int(u.get("cache_read_input_tokens", 0) or 0),
        })
    except Exception:
        return None
