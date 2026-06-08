"""Singapore Nutrition Label Investigator — OpenAI Agents SDK (image input).

Idiomatic OpenAI Agents SDK demo. The agent is given a PHOTO of a nutrition
label; the model's own vision reads the product name, additives, sugar and
saturated fat per 100ml, and whether the product is a beverage. There is no
Open Food Facts lookup — the image is the sole source of truth.

Tools are plain async functions decorated with @function_tool (schema inferred
from type hints + docstrings). The final result is a *typed* Pydantic object
via `output_type=NutritionVerdict`.

The model is served by a LiteLLM proxy: an AsyncOpenAI client wrapped in
OpenAIChatCompletionsModel. Trace export is disabled because it would otherwise
try to phone home to OpenAI with a key the proxy issued.

Usage:
    cp .env.example .env   # then fill in your proxy key
    uv run python main.py            # defaults to the bundled milo.jpeg
    uv run python main.py path/to/label.jpeg
"""

import os
import re
import sys
import json
import base64
import asyncio
import mimetypes
import httpx
from pathlib import Path
from dotenv import load_dotenv
from pydantic import BaseModel
from agents import (
    Agent,
    Runner,
    function_tool,
    AsyncOpenAI,
    OpenAIChatCompletionsModel,
    set_tracing_disabled,
)

# Load credentials from a local .env (gitignored; copy from .env.example).
# override=True so the local .env wins over any stray OPENAI_* in the shell
# (e.g. a personal OpenAI key) that would otherwise misroute to api.openai.com.
load_dotenv(Path(__file__).resolve().parent / ".env", override=True)

API_KEY = os.environ["OPENAI_API_KEY"]
BASE_URL = os.environ["OPENAI_BASE_URL"]
OPENAI_MODEL = os.environ["OPENAI_MODEL"]

DEFAULT_IMAGE = str(Path(__file__).resolve().parent / "milo.jpeg")
HCS_RESOURCE_ID = "d_6725eed000bf5b3c5d310eb08de0851f"

set_tracing_disabled(True)  # proxy key won't authenticate against OpenAI's trace ingest

# Full SFA permitted-additives table, parsed from the official PDF by
# build_additives.py. ~540 entries; ~270 carry E-numbers.
_ADDITIVES = json.loads((Path(__file__).resolve().parent / "sfa_additives.json").read_text())


def _norm(s: str) -> str:
    """Normalise an additive name for matching: drop parentheticals, stereo
    markers (L-, DL-, L(+)-), punctuation and extra spaces. So the label's
    'monosodium glutamate' matches the PDF's formal 'Monosodium L- glutamate'."""
    s = s.lower().replace("en:", "")
    s = re.sub(r"\([^)]*\)", "", s)               # drop parentheticals, e.g. (L-)
    s = re.sub(r"\b[dl]l?\s*\(?\+?\)?-", "", s)    # drop stereo markers L- DL- L(+)-
    s = s.replace("-", " ")
    s = re.sub(r"[^a-z0-9 ]", "", s)
    return re.sub(r"\s+", " ", s).strip()


# E-number index: keyed both as-is ('500(ii)') and by base number ('500'), so a
# label that prints '500' still resolves to the sub-numbered entry.
_BY_E = {}
# Alias index: every name / name_in_regs (and '/'-separated alternates), normalised.
_BY_ALIAS = {}
for _e in _ADDITIVES:
    # Index by both E-number and INS number. Many entries (e.g. the phosphates)
    # carry their number only in the INS column with the E-column blank, so
    # indexing INS too makes ~110 more additives findable by number.
    for _num in (_e["e_number"], _e.get("ins")):
        if _num:
            _BY_E.setdefault(_num.lower(), _e)
            _BY_E.setdefault(re.sub(r"\(.*\)", "", _num).lower(), _e)  # base, e.g. '500'
    for _n in (_e["name"], _e.get("name_in_regs")):
        if _n:
            for _part in _n.split("/"):
                _BY_ALIAS.setdefault(_norm(_part), _e)

# A handful of true colloquialisms that appear nowhere in the PDF, mapped to the
# additive's formal name (which the alias index resolves). Everything that the
# PDF already names — ascorbic acid, sodium bicarbonate, etc. — is handled by
# the alias index above and needs no entry here.
COLLOQUIAL = {
    "msg": "monosodium glutamate",
    "soy lecithin": "lecithin", "soya lecithin": "lecithin",
    "vitamin c": "ascorbic acid",
    "baking soda": "sodium bicarbonate",
    "cream of tartar": "potassium acid tartrate",
    "mono and diglycerides": "mono and diglycerides of fatty acids",
}

# Consumer-relevant warnings the PDF's terse "Notes" column (GMP / *) lacks.
# Overlaid on top of the parsed SFA data, keyed by E-number.
CONSUMER_NOTES = {
    "102": "Linked to hyperactivity in children in some studies; specific labelling required.",
    "110": "EU requires a hyperactivity warning; SFA permits with disclosure.",
    "211": "Can form benzene when combined with ascorbic acid (Vitamin C) — flag if both present.",
    "621": "MSG — safe at normal dietary intake; some report sensitivity.",
    "951": "Aspartame — must carry a phenylalanine warning for PKU sufferers.",
    "407": "Carrageenan — some contested studies on gut inflammation.",
    "471": "May contain trans fats not separately listed on the label.",
}


# ── Persistent memory (a simple JSON file the agent reads and writes) ─────────
# Holds two things that let the agent learn and remember ACROSS runs:
#   learned_aliases — label name -> SFA number, discovered when the model
#                     supplies an E-number hint for a name the table didn't know
#   products        — past verdicts, keyed by product name, for recall
# Each SDK also ships a native session layer (here: agents.SQLiteSession) for
# production memory; this file keeps the demo portable and inspectable.
_MEMORY_PATH = Path(__file__).resolve().parent / "agent_memory.json"


def _load_memory() -> dict:
    try:
        return json.loads(_MEMORY_PATH.read_text())
    except (FileNotFoundError, ValueError):
        return {"learned_aliases": {}, "products": {}}


def _save_memory(mem: dict) -> None:
    _MEMORY_PATH.write_text(json.dumps(mem, indent=2, ensure_ascii=False))


_MEMORY = _load_memory()


def _resolve_additive(additive: str):
    """Resolve an E-number or additive name to a table entry, or None.

    Consults learned aliases first, so a name taught in a previous run resolves
    deterministically without needing the model to supply a number again."""
    norm = _norm(additive)
    compact = norm.replace(" ", "")
    e_key = compact[1:] if compact.startswith("e") and compact[1:2].isdigit() else compact
    return (
        _BY_E.get(_MEMORY["learned_aliases"].get(norm, ""))
        or _BY_E.get(e_key)
        or _BY_ALIAS.get(norm)
        or _BY_ALIAS.get(_norm(COLLOQUIAL.get(norm, "")))
    )


def _format_entry(entry, raw: str) -> str:
    if not entry:
        return (f"{raw}: Not found in the SFA permitted-additives list. It may be "
                "an unpermitted additive, a vitamin/nutrient, or a non-specific "
                "label term — worth noting rather than assuming it is permitted.")
    label = f"E{entry['e_number']}" if entry["e_number"] else entry["name"]
    parts = [f"{label} ({entry['name']}): Permitted by SFA"]
    if entry.get("schedule"):
        parts.append(f"under {entry['schedule']}")
    note = CONSUMER_NOTES.get(entry["e_number"] or "")
    tail = f". {note}" if note else "."
    return " ".join(parts) + tail


# ── Tools (schema inferred from hints + docstrings) ──────────────────────────

@function_tool
async def check_sfa_additive(additive: str, e_number_hint: str = "") -> str:
    """Check whether a food additive is permitted by the Singapore Food Agency.

    Looks the additive up in the full SFA permitted-additives list (parsed from
    the official SFA PDF). Accepts either an E-number or a plain-English name as
    printed on a Singapore label.

    Args:
        additive: An E-number ('E211', 'e211', 'en:e211') OR an additive name
            ('Sodium Benzoate', 'Soy Lecithin', 'MSG').
        e_number_hint: Optional. If you pass a name and also know its E/INS
            number, supply it here. If the name isn't recognised but the number
            is, the mapping is REMEMBERED so the name resolves directly next time.
    """
    raw = additive.strip()
    entry = _resolve_additive(raw)
    if not entry and e_number_hint.strip():
        entry = _resolve_additive(e_number_hint)
        if entry:  # learn: this label name -> this number, persisted to disk
            num = entry["e_number"] or entry.get("ins") or ""
            _MEMORY["learned_aliases"][_norm(raw)] = re.sub(r"\(.*\)", "", num).lower()
            _save_memory(_MEMORY)
    return _format_entry(entry, raw)


@function_tool
async def recall_product(product_name: str) -> str:
    """Recall whether this product has been investigated before, and the prior verdict.

    Call this FIRST. If a prior record exists, use it to inform (not replace)
    your fresh investigation.

    Args:
        product_name: The product/brand name read off the label.
    """
    rec = _MEMORY["products"].get(product_name.strip().lower())
    if not rec:
        return f"No prior record of '{product_name}'. This is a first investigation."
    return (f"Seen before — prior verdict for '{rec['product_name']}': {rec['summary']} "
            f"Recommendation: {rec['recommendation']}")


@function_tool
async def check_hcs(product_name: str) -> str:
    """Check if a product carries Singapore's Healthier Choice Symbol (HCS).

    Queries the HPB dataset on data.gov.sg.

    Args:
        product_name: Brand and product name to search for.
    """
    try:
        async with httpx.AsyncClient(timeout=10) as client:
            resp = await client.get(
                "https://data.gov.sg/api/action/datastore_search",
                params={"resource_id": HCS_RESOURCE_ID, "q": product_name, "limit": 5},
            )
            records = _safe_json(resp).get("result", {}).get("records", [])
    except httpx.HTTPError as e:
        return f"HCS check failed (network error): {e}"
    if records:
        matches = [r.get("brand_and_product_name", "?") for r in records]
        return f"HCS CERTIFIED. Matching products: {', '.join(matches)}"
    return (f"NOT FOUND in HCS database. '{product_name}' does not appear to carry "
            "the Healthier Choice Symbol (absence is not necessarily a concern).")


@function_tool
async def calculate_nutri_grade(sugar_per_100ml: float, saturated_fat_per_100ml: float) -> str:
    """Calculate the Singapore Nutri-Grade (A/B/C/D) for a beverage.

    Only call this for beverages, using values read from the nutrition panel.

    Args:
        sugar_per_100ml: Sugar content in grams per 100ml.
        saturated_fat_per_100ml: Saturated fat content in grams per 100ml.
    """
    sugar, sat_fat = sugar_per_100ml, saturated_fat_per_100ml
    if sugar <= 1 and sat_fat <= 0.7:
        grade, desc = "A", "Healthiest tier — very low sugar and saturated fat"
    elif sugar <= 5 and sat_fat <= 1.2:
        grade, desc = "B", "Acceptable — moderate sugar and saturated fat"
    elif sugar <= 10 and sat_fat <= 2.8:
        grade, desc = "C", "Less healthy — must display Nutri-Grade label"
    else:
        grade, desc = "D", "Least healthy — mandatory label; cannot advertise to children"
    return (f"Nutri-Grade: {grade} | Sugar {sugar}g/100ml | "
            f"Sat fat {sat_fat}g/100ml | {desc}")


def _safe_json(resp: httpx.Response) -> dict:
    """Return parsed JSON, or {} if the response isn't 200 / isn't JSON."""
    if resp.status_code != 200:
        return {}
    try:
        return resp.json()
    except ValueError:
        return {}


def _data_url(image_path: str) -> str:
    """Read an image file into a base64 data URL for the vision input."""
    mime = mimetypes.guess_type(image_path)[0] or "image/jpeg"
    b64 = base64.standard_b64encode(Path(image_path).read_bytes()).decode()
    return f"data:{mime};base64,{b64}"


# ── Typed structured output ──────────────────────────────────────────────────

class Finding(BaseModel):
    category: str   # "GREEN", "AMBER" or "RED"
    finding: str    # plain-English description
    source: str     # which tool call (or "label") produced this


class NutritionVerdict(BaseModel):
    # `reasoning` is declared first so the model thinks through what it read and
    # how it weighed each finding *before* committing to the structured verdict —
    # a built-in scratchpad that also surfaces its rationale to the reader.
    reasoning: str
    product_name: str
    summary: str
    green: list[Finding]
    amber: list[Finding]
    red: list[Finding]
    recommendation: str


INSTRUCTIONS = """You are a Singapore food label investigator. You are given a PHOTO of a packaged food or drink label. Work only from what you can read in the image — there is no product database.

0. First read the product name off the label and call recall_product with it. If this product was investigated before, note the prior verdict in your reasoning and let it inform (but not replace) this investigation.
1. Read the label carefully and extract: the product name, the full ingredient/additive list, the sugar and saturated fat content per 100ml (for beverages) or per 100g, and whether the product is a beverage/drink.
2. For EACH additive you can identify (whether printed as an E-number like 'E471' or as a name like 'Soy Lecithin' / 'Permitted Stabiliser'), call check_sfa_additive individually — one call per additive. Do not skip any. When the label gives a name, ALSO pass its E/INS number as `e_number_hint` if you know it (e.g. soy lecithin → E322, MSG → E621) — this is most reliable AND teaches the tool the name for next time. If the label only says a generic phrase like "permitted stabiliser" with no specific name, note that you could not identify it rather than calling the tool.
3. Call check_hcs with the product name to check for the Healthier Choice Symbol.
4. ONLY if the product is a beverage, call calculate_nutri_grade with sugar and saturated fat per 100ml read from the label. Skip it for solid foods/powders. If values are given per serving or per 100g, convert or note the assumption.
5. Return a NutritionVerdict. In the `reasoning` field, explain step by step what you read off the label, how you classified each additive, and why each finding landed in GREEN/AMBER/RED — this is your working-out, written for a curious reader. Then fill in the findings sorted into GREEN/AMBER/RED, citing the tool that produced each finding, or "label" for facts you read directly off the image.

Be specific and plain-spoken. If the image is unreadable or not a nutrition label, say so in the reasoning and summary."""


def build_agent() -> Agent:
    client = AsyncOpenAI(base_url=BASE_URL, api_key=API_KEY)
    model = OpenAIChatCompletionsModel(model=OPENAI_MODEL, openai_client=client)
    return Agent(
        name="SG Nutrition Investigator",
        model=model,
        instructions=INSTRUCTIONS,
        tools=[recall_product, check_sfa_additive, check_hcs, calculate_nutri_grade],
        output_type=NutritionVerdict,
    )


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

    print(f"Product: {v.product_name}\n")
    print(f"Reasoning: {v.reasoning}\n")
    print(f"Summary: {v.summary}\n")
    for f in v.green:
        print(f"🟢 {f.finding}  [{f.source}]")
    for f in v.amber:
        print(f"🟡 {f.finding}  [{f.source}]")
    for f in v.red:
        print(f"🔴 {f.finding}  [{f.source}]")
    print(f"\nRecommendation: {v.recommendation}")

    # Remember this verdict so a future run of the same product can recall it.
    _MEMORY["products"][v.product_name.strip().lower()] = {
        "product_name": v.product_name,
        "summary": v.summary,
        "recommendation": v.recommendation,
    }
    _save_memory(_MEMORY)


def main() -> None:
    image_path = sys.argv[1] if len(sys.argv) > 1 else DEFAULT_IMAGE
    if not Path(image_path).is_file():
        sys.exit(f"Image not found: {image_path}")
    asyncio.run(investigate(image_path))


if __name__ == "__main__":
    main()
