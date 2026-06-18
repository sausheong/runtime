"""In-process MCP tools for the Food Label Advisor (Claude Agent SDK).

Three tools:
  extract_label    — OCR/vision extraction + normalisation for one product
  compare_products — rank the extracted products by user goal
  submit_advice    — typed output channel; fills AdvisorHolder for the adapter

Pure *_impl functions carry the logic (hermetically testable).
build_advisor_server() wraps them as SDK tools bound to a turn-scoped holder.
"""
from __future__ import annotations

from dataclasses import dataclass, field

from claude_agent_sdk import tool, create_sdk_mcp_server
from pydantic import ValidationError

from agent import (
    LabelData,
    NutritionPer100,
    ComparisonResult,
    AdvisorResult,
    GOAL,
    normalise_to_100,
    normalise_to_serving,
    build_comparison,
)


# ── Turn-scoped state ─────────────────────────────────────────────────────────

@dataclass
class AdvisorHolder:
    labels: dict[str, LabelData] = field(default_factory=dict)
    result: AdvisorResult | None = None


# ── Tool implementations (pure; no SDK dependency) ────────────────────────────

def extract_label_impl(args: dict, holder: AdvisorHolder) -> str:
    """Parse, validate and normalise label data for one product."""
    try:
        name = args["product_name"].strip()
        serving = args.get("serving_size_g_or_ml")
        values_are_per = args.get("values_are_per", "per_100")

        raw = NutritionPer100(
            calories_kcal=args.get("calories_kcal"),
            sugar_g=args.get("sugar_g"),
            fibre_g=args.get("fibre_g"),
            protein_g=args.get("protein_g"),
            sodium_mg=args.get("sodium_mg"),
            saturated_fat_g=args.get("saturated_fat_g"),
        )

        if values_are_per == "per_serving" and serving and serving > 0:
            per_100 = NutritionPer100(
                calories_kcal=normalise_to_100(raw.calories_kcal, serving) if raw.calories_kcal is not None else None,
                sugar_g=normalise_to_100(raw.sugar_g, serving) if raw.sugar_g is not None else None,
                fibre_g=normalise_to_100(raw.fibre_g, serving) if raw.fibre_g is not None else None,
                protein_g=normalise_to_100(raw.protein_g, serving) if raw.protein_g is not None else None,
                sodium_mg=normalise_to_100(raw.sodium_mg, serving) if raw.sodium_mg is not None else None,
                saturated_fat_g=normalise_to_100(raw.saturated_fat_g, serving) if raw.saturated_fat_g is not None else None,
            )
            per_serving = raw
        else:
            per_100 = raw
            per_serving = NutritionPer100(
                calories_kcal=normalise_to_serving(raw.calories_kcal, serving) if raw.calories_kcal is not None and serving else None,
                sugar_g=normalise_to_serving(raw.sugar_g, serving) if raw.sugar_g is not None and serving else None,
                fibre_g=normalise_to_serving(raw.fibre_g, serving) if raw.fibre_g is not None and serving else None,
                protein_g=normalise_to_serving(raw.protein_g, serving) if raw.protein_g is not None and serving else None,
                sodium_mg=normalise_to_serving(raw.sodium_mg, serving) if raw.sodium_mg is not None and serving else None,
                saturated_fat_g=normalise_to_serving(raw.saturated_fat_g, serving) if raw.saturated_fat_g is not None and serving else None,
            ) if serving else NutritionPer100()

        label = LabelData(
            product_name=name,
            brand=args.get("brand"),
            serving_size_g_or_ml=serving,
            per_100=per_100,
            per_serving=per_serving,
            additives=args.get("additives", []),
            allergens=args.get("allergens", []),
            claims=args.get("claims", []),
            is_beverage=args.get("is_beverage", False),
            extraction_notes=args.get("extraction_notes", ""),
        )
        holder.labels[name] = label

        unit = "ml" if label.is_beverage else "g"
        parts = [f"Extracted '{name}'"]
        p = label.per_100
        if p.sugar_g is not None:
            parts.append(f"sugar={p.sugar_g}g/100{unit}")
        if p.protein_g is not None:
            parts.append(f"protein={p.protein_g}g/100{unit}")
        if p.sodium_mg is not None:
            parts.append(f"sodium={p.sodium_mg}mg/100{unit}")
        if p.calories_kcal is not None:
            parts.append(f"calories={p.calories_kcal}kcal/100{unit}")
        parts.append(f"additives={len(label.additives)}")
        parts.append(f"allergens={len(label.allergens)}")
        return ". ".join(parts) + "."

    except Exception as e:
        return f"Extraction failed for '{args.get('product_name', '?')}': {e}"


def compare_products_impl(product_names: list[str], goal: str, reasoning: str, holder: AdvisorHolder) -> str:
    missing = [n for n in product_names if n not in holder.labels]
    if missing:
        return f"Cannot compare: labels not yet extracted for: {missing}. Call extract_label first."
    if len(product_names) < 2:
        return "Need at least 2 products to compare."

    labels = [holder.labels[n] for n in product_names]
    try:
        result = build_comparison(labels, goal, reasoning)  # type: ignore[arg-type]
    except Exception as e:
        return f"Comparison failed: {e}"

    lines = [f"Comparison for goal '{goal}':"]
    for r in result.ranked:
        lines.append(f"  #{r.rank} {r.product_name} — {r.score_note}")
    lines.append(f"Winner: {result.winner}")
    # Store for submit_advice
    holder.result = AdvisorResult(labels=labels, comparison=result)
    return "\n".join(lines)


def submit_advice_impl(holder: AdvisorHolder) -> str:
    if holder.result is None or holder.result.comparison is None:
        return "No comparison result available. Call compare_products first."
    return "Advice recorded. Reply with a one- or two-sentence plain-language summary."


# ── Tool name list (for allowed_tools in adapter) ─────────────────────────────

TOOL_NAMES = [
    "mcp__advisor__extract_label",
    "mcp__advisor__compare_products",
    "mcp__advisor__submit_advice",
]


# ── JSON schemas ──────────────────────────────────────────────────────────────

_NUTRITION_FIELDS = {
    "calories_kcal": {"type": "number", "description": "Calories in kcal"},
    "sugar_g":       {"type": "number", "description": "Total sugar in grams"},
    "fibre_g":       {"type": "number", "description": "Dietary fibre in grams"},
    "protein_g":     {"type": "number", "description": "Protein in grams"},
    "sodium_mg":     {"type": "number", "description": "Sodium in milligrams"},
    "saturated_fat_g": {"type": "number", "description": "Saturated fat in grams"},
}

EXTRACT_LABEL_SCHEMA = {
    "type": "object",
    "properties": {
        "product_name":        {"type": "string", "description": "Product name as printed on label"},
        "brand":               {"type": "string", "description": "Brand name"},
        "serving_size_g_or_ml": {"type": "number", "description": "Serving size in g or ml"},
        "values_are_per": {
            "type": "string",
            "enum": ["per_100", "per_serving"],
            "description": "Whether the supplied nutritional values are per 100g/ml or per serving",
        },
        **_NUTRITION_FIELDS,
        "additives":   {"type": "array", "items": {"type": "string"}, "description": "Additives/E-numbers listed"},
        "allergens":   {"type": "array", "items": {"type": "string"}, "description": "Allergens declared on label"},
        "claims":      {"type": "array", "items": {"type": "string"}, "description": "Marketing claims (e.g. 'low fat')"},
        "is_beverage": {"type": "boolean", "description": "True if this is a drink/beverage"},
        "extraction_notes": {"type": "string", "description": "Caveats about unclear or estimated values"},
    },
    "required": ["product_name"],
}

COMPARE_PRODUCTS_SCHEMA = {
    "type": "object",
    "properties": {
        "product_names": {
            "type": "array",
            "items": {"type": "string"},
            "description": "Names of products to compare (must match extract_label product_name values)",
        },
        "goal": {
            "type": "string",
            "enum": [
                "healthiest_overall", "lowest_sugar", "highest_protein",
                "lowest_sodium", "least_processed", "best_for_children", "allergen_safe",
            ],
            "description": "The user's ranking goal",
        },
        "reasoning": {
            "type": "string",
            "description": "Step-by-step reasoning for the comparison — written for a curious reader",
        },
    },
    "required": ["product_names", "goal", "reasoning"],
}

SUBMIT_ADVICE_SCHEMA = {
    "type": "object",
    "properties": {},
    "required": [],
    "description": "Finalise and record the comparison result. Call after compare_products.",
}


# ── MCP server factory ────────────────────────────────────────────────────────

def build_advisor_server(holder: AdvisorHolder):
    """Build the per-turn MCP server: all tools close over the given holder."""

    @tool("extract_label",
          "Extract and normalise nutritional data from one product label image. Call once per product.",
          EXTRACT_LABEL_SCHEMA)
    async def extract_label(args: dict) -> dict:
        return _text(extract_label_impl(args, holder))

    @tool("compare_products",
          "Rank all extracted products by the stated goal. Call after all labels are extracted.",
          COMPARE_PRODUCTS_SCHEMA)
    async def compare_products(args: dict) -> dict:
        return _text(compare_products_impl(
            args["product_names"], args["goal"], args.get("reasoning", ""), holder
        ))

    @tool("submit_advice",
          "Record the final comparison result. Call once when investigation is complete.",
          SUBMIT_ADVICE_SCHEMA)
    async def submit_advice(args: dict) -> dict:
        return _text(submit_advice_impl(holder))

    return create_sdk_mcp_server(
        name="advisor",
        version="0.1.0",
        tools=[extract_label, compare_products, submit_advice],
    )


def _text(s: str) -> dict:
    return {"content": [{"type": "text", "text": s}]}
