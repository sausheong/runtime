"""In-process MCP tools for the Claude Agent SDK nutrition agent.

Five tools: the four investigator tools (ported from the OpenAI example's
@function_tool bodies) plus submit_verdict — the typed-output channel that
replaces the OpenAI SDK's output_type. Pure *_impl functions carry the logic
(hermetically testable); build_nutrition_server() wraps them as SDK tools
bound to a turn-scoped VerdictHolder.
"""
from __future__ import annotations

import re
from dataclasses import dataclass

import httpx
from claude_agent_sdk import tool, create_sdk_mcp_server
from pydantic import ValidationError

import agent
from agent import NutritionVerdict, _resolve_additive, _format_entry, _norm, _safe_json, HCS_RESOURCE_ID


# JSON schema for submit_verdict, generated from the SAME pydantic model the
# OpenAI version enforced via output_type.
VERDICT_SCHEMA = NutritionVerdict.model_json_schema()


@dataclass
class VerdictHolder:
    """Turn-scoped slot the submit_verdict tool fills and the adapter reads."""
    verdict: NutritionVerdict | None = None


async def check_sfa_additive_impl(additive: str, e_number_hint: str) -> str:
    raw = additive.strip()
    entry = _resolve_additive(raw)
    if not entry and e_number_hint.strip():
        entry = _resolve_additive(e_number_hint)
        if entry:
            num = entry["e_number"] or entry.get("ins") or ""
            agent._MEMORY["learned_aliases"][_norm(raw)] = re.sub(r"\(.*\)", "", num).lower()
            agent._save_memory(agent._MEMORY)
    return _format_entry(entry, raw)


async def recall_product_impl(product_name: str) -> str:
    rec = agent._MEMORY["products"].get(product_name.strip().lower())
    if not rec:
        return f"No prior record of '{product_name}'. This is a first investigation."
    return (f"Seen before — prior verdict for '{rec['product_name']}': {rec['summary']} "
            f"Recommendation: {rec['recommendation']}")


async def check_hcs_impl(product_name: str) -> str:
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


async def calculate_nutri_grade_impl(sugar_per_100ml: float, saturated_fat_per_100ml: float) -> str:
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


async def submit_verdict_impl(payload: dict, holder: VerdictHolder) -> str:
    try:
        holder.verdict = NutritionVerdict.model_validate(payload)
    except ValidationError as e:
        return f"Invalid verdict, fix and resubmit: {e.errors()[:3]}"
    return "Verdict recorded. Reply with one short closing line."


TOOL_NAMES = [
    "mcp__nutrition__recall_product",
    "mcp__nutrition__check_sfa_additive",
    "mcp__nutrition__check_hcs",
    "mcp__nutrition__calculate_nutri_grade",
    "mcp__nutrition__submit_verdict",
]


def build_nutrition_server(holder: VerdictHolder):
    """Build the per-turn MCP server: tools close over the given holder."""

    @tool("recall_product",
          "Recall whether this product has been investigated before, and the prior verdict. Call this FIRST.",
          {"type": "object", "properties": {"product_name": {"type": "string"}}, "required": ["product_name"]})
    async def recall_product(args: dict) -> dict:
        return _text(await recall_product_impl(args["product_name"]))

    @tool("check_sfa_additive",
          "Check whether a food additive is permitted by the Singapore Food Agency. Accepts an E-number or a name; pass e_number_hint when you know the number for an unrecognised name (it will be remembered).",
          {"type": "object",
           "properties": {"additive": {"type": "string"}, "e_number_hint": {"type": "string"}},
           "required": ["additive"]})
    async def check_sfa_additive(args: dict) -> dict:
        return _text(await check_sfa_additive_impl(args["additive"], args.get("e_number_hint", "")))

    @tool("check_hcs",
          "Check if a product carries Singapore's Healthier Choice Symbol (HPB dataset on data.gov.sg).",
          {"type": "object", "properties": {"product_name": {"type": "string"}}, "required": ["product_name"]})
    async def check_hcs(args: dict) -> dict:
        return _text(await check_hcs_impl(args["product_name"]))

    @tool("calculate_nutri_grade",
          "Calculate the Singapore Nutri-Grade (A/B/C/D) for a BEVERAGE from sugar and saturated fat per 100ml.",
          {"type": "object",
           "properties": {"sugar_per_100ml": {"type": "number"}, "saturated_fat_per_100ml": {"type": "number"}},
           "required": ["sugar_per_100ml", "saturated_fat_per_100ml"]})
    async def calculate_nutri_grade(args: dict) -> dict:
        return _text(await calculate_nutri_grade_impl(args["sugar_per_100ml"], args["saturated_fat_per_100ml"]))

    @tool("submit_verdict",
          "Submit the final structured NutritionVerdict. You MUST call this exactly once when the investigation is complete.",
          VERDICT_SCHEMA)
    async def submit_verdict(args: dict) -> dict:
        return _text(await submit_verdict_impl(args, holder))

    return create_sdk_mcp_server(
        name="nutrition", version="0.1.0",
        tools=[recall_product, check_sfa_additive, check_hcs, calculate_nutri_grade, submit_verdict],
    )


def _text(s: str) -> dict:
    return {"content": [{"type": "text", "text": s}]}
