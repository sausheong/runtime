"""SG Nutrition Label Investigator — standalone CLI (OpenAI Agents SDK).

Thin front-end over agent.py: loads local .env, builds the agent, runs it on a
label photo, and prints the verdict. The agent definition (tools, SFA data,
memory, typed output) lives in agent.py and is shared with the runtime contract
adapter (adapter.py).

Usage:
    cp .env.example .env   # then fill in your proxy key
    uv run python main.py            # defaults to the bundled milo.jpeg
    uv run python main.py path/to/label.jpeg
"""
import sys
import asyncio
from pathlib import Path

from dotenv import load_dotenv

# Load credentials from a local .env (gitignored) BEFORE importing agent, so the
# agent's lazy env reads see them. override=True so the local .env wins over any
# stray OPENAI_* in the shell (which would misroute to api.openai.com).
load_dotenv(Path(__file__).resolve().parent / ".env", override=True)

from agent import build_agent, render_verdict, remember_verdict, NutritionVerdict
from agents import Runner

DEFAULT_IMAGE = str(Path(__file__).resolve().parent / "milo.jpeg")


def _data_url(image_path: str) -> str:
    import base64, mimetypes
    mime = mimetypes.guess_type(image_path)[0] or "image/jpeg"
    b64 = base64.standard_b64encode(Path(image_path).read_bytes()).decode()
    return f"data:{mime};base64,{b64}"


async def investigate(image_path: str) -> None:
    print(f"\nInvestigating image: {image_path}\n{'─' * 60}")
    user_input = [{
        "role": "user",
        "content": [
            {"type": "input_text", "text": "Investigate this nutrition label."},
            {"type": "input_image", "image_url": _data_url(image_path)},
        ],
    }]
    result = await Runner.run(build_agent(), input=user_input)
    v: NutritionVerdict = result.final_output
    print(render_verdict(v))
    remember_verdict(v)


def main() -> None:
    image_path = sys.argv[1] if len(sys.argv) > 1 else DEFAULT_IMAGE
    if not Path(image_path).is_file():
        sys.exit(f"Image not found: {image_path}")
    asyncio.run(investigate(image_path))


if __name__ == "__main__":
    main()
