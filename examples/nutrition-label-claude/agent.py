"""SG Nutrition Investigator — shared domain logic, framework-free.

Third implementation of the investigator (after Go/harness and the OpenAI
Agents SDK). This module carries everything that is NOT the Claude Agent SDK:
the SFA additive table + resolution, the persistent learned-alias/product
memory, the typed NutritionVerdict, the system prompt, and the prose renderer.
tools.py wraps the lookups as in-process MCP tools; adapter.py drives the SDK.
"""

import re
import json
import httpx
from pathlib import Path
from pydantic import BaseModel

HCS_RESOURCE_ID = "d_6725eed000bf5b3c5d310eb08de0851f"

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
# Each SDK also ships a native session layer for production memory; this file
# keeps the demo portable and inspectable.
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


def _safe_json(resp: httpx.Response) -> dict:
    """Return parsed JSON, or {} if the response isn't 200 / isn't JSON."""
    if resp.status_code != 200:
        return {}
    try:
        return resp.json()
    except ValueError:
        return {}


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
5. When your investigation is complete, you MUST call the submit_verdict tool exactly once with the full verdict: reasoning (step-by-step working-out, written for a curious reader), product_name, summary, findings sorted into green/amber/red (each with category, finding, source — the tool that produced it, or "label"), and recommendation. Do not write the verdict as plain text; submit it through the tool. After submit_verdict succeeds, reply with a single short closing line.

Be specific and plain-spoken. If the image is unreadable or not a nutrition label, say so in the reasoning and summary."""


def render_verdict(v: NutritionVerdict) -> str:
    """Render a validated verdict to the CLI's signature prose block.

    Pure: identical input → identical output, no I/O. Used by both the CLI
    (main.py) and the contract adapter (adapter.py) so the hosted agent's
    streamed text matches what `uv run python main.py` prints.
    """
    lines = [
        f"Product: {v.product_name}",
        "",
        f"Reasoning: {v.reasoning}",
        "",
        f"Summary: {v.summary}",
        "",
    ]
    for f in v.green:
        lines.append(f"🟢 {f.finding}  [{f.source}]")
    for f in v.amber:
        lines.append(f"🟡 {f.finding}  [{f.source}]")
    for f in v.red:
        lines.append(f"🔴 {f.finding}  [{f.source}]")
    lines.append("")
    lines.append(f"Recommendation: {v.recommendation}")
    return "\n".join(lines)


def remember_verdict(v: NutritionVerdict) -> None:
    """Persist a verdict to agent_memory.json so a future run can recall it."""
    _MEMORY["products"][v.product_name.strip().lower()] = {
        "product_name": v.product_name,
        "summary": v.summary,
        "recommendation": v.recommendation,
    }
    _save_memory(_MEMORY)
