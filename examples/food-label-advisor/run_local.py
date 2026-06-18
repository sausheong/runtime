"""Local runner: invoke the Food Label Advisor directly (no HTTP server).

Reads all images from food_label_images/, passes them to the adapter's run()
as the shim would, and prints the resulting events to stdout.

Usage:
    uv run python run_local.py [goal]

goal defaults to "healthiest_overall".
"""
from __future__ import annotations

import asyncio
import mimetypes
import sys
from pathlib import Path

from dotenv import load_dotenv

load_dotenv()

from runtime_contract.events import ContractEvent, Image  # noqa: E402
from adapter import FoodLabelAdvisorAdapter                # noqa: E402

IMAGES_DIR = Path(__file__).resolve().parent.parent.parent / "food_label_images"
DB_PATH = str(Path(__file__).resolve().parent / "run_local.db")


def load_images(directory: Path) -> list[Image]:
    images = []
    for path in sorted(directory.iterdir()):
        if path.suffix.lower() not in {".jpg", ".jpeg", ".png", ".webp", ".gif"}:
            continue
        mime, _ = mimetypes.guess_type(str(path))
        mime = mime or "image/jpeg"
        data = path.read_bytes()
        images.append(Image(data=data, mime=mime))
        print(f"  loaded {path.name} ({len(data)//1024} KB, {mime})")
    return images


async def main(goal: str) -> None:
    print(f"=== Food Label Advisor — local run ===")
    print(f"Images dir : {IMAGES_DIR}")
    print(f"Goal       : {goal}")
    print()

    if not IMAGES_DIR.exists():
        print(f"ERROR: images directory not found: {IMAGES_DIR}")
        sys.exit(1)

    images = load_images(IMAGES_DIR)
    if not images:
        print("ERROR: no images found in", IMAGES_DIR)
        sys.exit(1)

    print(f"\nLoaded {len(images)} image(s). Starting agent...\n")

    adapter = FoodLabelAdvisorAdapter(DB_PATH)
    session_id = "local-test-session"
    message = f"Please compare these food products. Goal: {goal}."

    async for event in adapter.run(session_id, message, images, []):
        if event.type == "text":
            print("─" * 60)
            print(event.text)
            print("─" * 60)
        elif event.type == "error":
            print(f"\nERROR: {event.error}")
        else:
            print(f"[{event.type}] {event}")


if __name__ == "__main__":
    goal = sys.argv[1] if len(sys.argv) > 1 else "healthiest_overall"
    asyncio.run(main(goal))
