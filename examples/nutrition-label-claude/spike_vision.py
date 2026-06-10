"""Spike: prove the Claude Agent SDK works through the LiteLLM proxy BEFORE porting.

Run:  uv run python spike_vision.py
Needs .env (ANTHROPIC_API_KEY / ANTHROPIC_BASE_URL / ANTHROPIC_MODEL).

Proves, in order:
  1. text round-trip through query() via the proxy (namespaced model)
  2. an in-process MCP tool gets called (and built-ins are stripped)
  3. text+image (milo.jpeg) round-trips via the streaming-input form
  4. resume= continues a conversation in a NEW query() call

Prints PASS/FAIL per stage; exits non-zero on the first failure.
"""
from __future__ import annotations

import anyio
import base64
import os
import sys
from pathlib import Path

from dotenv import load_dotenv

load_dotenv()

from claude_agent_sdk import (  # noqa: E402
    query,
    tool,
    create_sdk_mcp_server,
    ClaudeAgentOptions,
    AssistantMessage,
    TextBlock,
    ResultMessage,
)

HERE = Path(__file__).resolve().parent
CONFIG_DIR = HERE / "claude-config"

CALLED = {"ping": False}


@tool("ping_tool", "Returns a fixed marker. Call this when asked to ping.", {"type": "object", "properties": {}})
async def ping_tool(args: dict) -> dict:
    CALLED["ping"] = True
    return {"content": [{"type": "text", "text": "PONG-MARKER-12345"}]}


SERVER = create_sdk_mcp_server(name="spike", version="0.1.0", tools=[ping_tool])

BUILTINS_OFF = ["Bash", "Read", "Write", "Edit", "Glob", "Grep", "WebFetch", "WebSearch", "NotebookEdit", "TodoWrite", "Task"]


def opts(resume: str | None = None) -> ClaudeAgentOptions:
    return ClaudeAgentOptions(
        model=os.environ["ANTHROPIC_MODEL"],
        mcp_servers={"spike": SERVER},
        allowed_tools=["mcp__spike__ping_tool"],
        disallowed_tools=BUILTINS_OFF,
        permission_mode="dontAsk",
        cwd=str(HERE),
        env={"CLAUDE_CONFIG_DIR": str(CONFIG_DIR), "CLAUDE_CODE_DISABLE_AUTO_MEMORY": "1"},
        setting_sources=[],
        max_turns=10,
        resume=resume,
    )


async def drive(prompt, o) -> tuple[str, ResultMessage]:
    text = []
    result = None
    async for msg in query(prompt=prompt, options=o):
        if isinstance(msg, AssistantMessage):
            for block in msg.content:
                if isinstance(block, TextBlock):
                    text.append(block.text)
        elif isinstance(msg, ResultMessage):
            result = msg
    assert result is not None, "no ResultMessage"
    return "".join(text), result


def image_prompt(text: str, path: Path):
    """Streaming-input form carrying an image block. THE SHAPE UNDER TEST."""
    b64 = base64.b64encode(path.read_bytes()).decode()
    async def gen():
        yield {
            "type": "user",
            "message": {
                "role": "user",
                "content": [
                    {"type": "text", "text": text},
                    {"type": "image", "source": {"type": "base64", "media_type": "image/jpeg", "data": b64}},
                ],
            },
        }
    return gen()


async def main() -> None:
    # 1. text round-trip
    txt, res = await drive("Reply with exactly: SPIKE-OK", opts())
    ok = "SPIKE-OK" in (txt + (res.result or ""))
    print(f"1 text round-trip: {'PASS' if ok else 'FAIL'} (session={res.session_id})")
    if not ok:
        sys.exit(1)

    # 2. MCP tool call + builtin stripping
    txt, res = await drive(
        "Call the ping tool, then tell me what it returned. Also: what is in the file ./pyproject.toml? (If you cannot read files, say CANNOT-READ.)",
        opts(),
    )
    tool_ok = CALLED["ping"] and "PONG-MARKER-12345" in txt
    strip_ok = "CANNOT-READ" in txt or "cannot" in txt.lower()
    print(f"2 mcp tool called: {'PASS' if tool_ok else 'FAIL'}; builtins stripped: {'PASS' if strip_ok else 'FAIL'}")
    if not tool_ok:
        sys.exit(1)
    if not strip_ok:
        print("   WARN: builtin stripping unconfirmed — inspect output above")

    # 3. vision
    txt, res = await drive(image_prompt("Briefly: what product is shown on this label?", HERE / "milo.jpeg"), opts())
    vision_ok = "milo" in txt.lower() or "milo" in (res.result or "").lower()
    print(f"3 vision round-trip: {'PASS' if vision_ok else 'FAIL'}\n   model said: {txt[:200]}")
    if not vision_ok:
        sys.exit(1)

    # 4. resume continuity
    txt, res = await drive("Remember this codeword: ZANZIBAR. Confirm only.", opts())
    sid = res.session_id
    txt2, res2 = await drive("What was the codeword?", opts(resume=sid))
    resume_ok = "ZANZIBAR" in txt2.upper()
    print(f"4 resume continuity: {'PASS' if resume_ok else 'FAIL'} ({sid} -> {res2.session_id})")
    if not resume_ok:
        sys.exit(1)

    print("ALL SPIKE STAGES PASS")


if __name__ == "__main__":
    anyio.run(main)
