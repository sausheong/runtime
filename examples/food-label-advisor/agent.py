"""Food Label Advisor — domain logic, framework-free.

Defines the data model (LabelData, ComparisonResult), normalisation helpers
(per 100g/ml and per serving), goal-based ranking, and the system prompt.
tools.py wraps these as in-process MCP tools; adapter.py drives the SDK.
"""
from __future__ import annotations

import re
from typing import Literal

from pydantic import BaseModel, Field

# ── Goal vocabulary ───────────────────────────────────────────────────────────

GOAL = Literal[
    "healthiest_overall",
    "lowest_sugar",
    "highest_protein",
    "lowest_sodium",
    "least_processed",
    "best_for_children",
    "allergen_safe",
]

# ── Data models ───────────────────────────────────────────────────────────────


class NutritionPer100(BaseModel):
    """All values per 100 g or 100 ml (None = not stated on label)."""
    calories_kcal: float | None = None
    sugar_g: float | None = None
    fibre_g: float | None = None
    protein_g: float | None = None
    sodium_mg: float | None = None
    saturated_fat_g: float | None = None


class LabelData(BaseModel):
    """Extracted, normalised data from one product label."""
    product_name: str
    brand: str | None = None
    serving_size_g_or_ml: float | None = None
    per_100: NutritionPer100 = Field(default_factory=NutritionPer100)
    per_serving: NutritionPer100 = Field(default_factory=NutritionPer100)
    additives: list[str] = Field(default_factory=list)
    allergens: list[str] = Field(default_factory=list)
    claims: list[str] = Field(default_factory=list)   # e.g. "low fat", "organic"
    is_beverage: bool = False
    extraction_notes: str = ""   # caveats (values estimated, label unclear, etc.)


class ProductRank(BaseModel):
    rank: int
    product_name: str
    score_note: str   # one-line rationale for this rank
    trade_offs: list[str]   # things to watch for despite winning on the goal


class ComparisonResult(BaseModel):
    goal: str
    reasoning: str
    ranked: list[ProductRank]   # ordered best→worst for the stated goal
    winner: str                  # product_name of rank-1
    winner_explanation: str      # why it wins — 2-3 sentences
    trade_offs_for_winner: list[str]


# ── Normalisation helpers ─────────────────────────────────────────────────────

def normalise_to_100(value: float, per_g: float) -> float:
    """Scale a per-serving value to per-100g/ml given serving size in grams/ml."""
    if per_g <= 0:
        raise ValueError(f"serving size must be > 0, got {per_g}")
    return round(value * 100 / per_g, 2)


def normalise_to_serving(value: float, per_g: float) -> float:
    """Scale a per-100g/ml value to per-serving given serving size."""
    return round(value * per_g / 100, 2)


def additive_count(label: LabelData) -> int:
    return len(label.additives)


# ── Ranking engine ────────────────────────────────────────────────────────────

def _get(label: LabelData, attr: str) -> float:
    """Return per_100 attribute; treat None as a very large / very small sentinel."""
    return getattr(label.per_100, attr) or 0.0


def rank_products(labels: list[LabelData], goal: GOAL) -> list[LabelData]:
    """Return labels sorted best-first for the given goal.

    For min goals (lower is better) we sort ascending; for max goals descending.
    Ties broken by additive count ascending (less processed wins ties).
    """
    if goal == "healthiest_overall":
        # Composite: penalise sugar, sodium, saturated fat; reward protein, fibre.
        # Missing values are treated as 0 (unknown doesn't disqualify).
        def score(l: LabelData) -> float:
            p = l.per_100
            return (
                (p.sugar_g or 0) * 2
                + (p.sodium_mg or 0) / 100
                + (p.saturated_fat_g or 0) * 3
                - (p.protein_g or 0) * 2
                - (p.fibre_g or 0) * 1.5
                + additive_count(l) * 0.5
            )
        return sorted(labels, key=lambda l: (score(l), additive_count(l)))

    if goal == "lowest_sugar":
        return sorted(labels, key=lambda l: (_get(l, "sugar_g"), additive_count(l)))

    if goal == "highest_protein":
        return sorted(labels, key=lambda l: (-_get(l, "protein_g"), additive_count(l)))

    if goal == "lowest_sodium":
        return sorted(labels, key=lambda l: (_get(l, "sodium_mg"), additive_count(l)))

    if goal == "least_processed":
        return sorted(labels, key=lambda l: (additive_count(l), _get(l, "sugar_g")))

    if goal == "best_for_children":
        # Prioritise low sugar, low sodium, no artificial colours / numbers.
        CHILD_FLAGS = {"tartrazine", "e102", "sunset yellow", "e110", "e211", "sodium benzoate"}
        def child_score(l: LabelData) -> float:
            flag_penalty = sum(
                1 for a in l.additives
                if any(f in a.lower() for f in CHILD_FLAGS)
            ) * 10
            return (
                (_get(l, "sugar_g") * 2)
                + (_get(l, "sodium_mg") / 100)
                + flag_penalty
                + additive_count(l) * 0.3
            )
        return sorted(labels, key=child_score)

    if goal == "allergen_safe":
        # Fewer allergens declared wins; secondary sort by healthiest_overall score.
        def allergen_score(l: LabelData) -> float:
            p = l.per_100
            secondary = (
                (p.sugar_g or 0) * 2
                + (p.sodium_mg or 0) / 100
                + (p.saturated_fat_g or 0) * 3
                - (p.protein_g or 0) * 2
            )
            return (len(l.allergens), secondary)
        return sorted(labels, key=allergen_score)

    # Fallback
    return labels


def build_comparison(labels: list[LabelData], goal: GOAL, reasoning: str) -> ComparisonResult:
    ranked = rank_products(labels, goal)
    winner = ranked[0]

    product_ranks = []
    for i, l in enumerate(ranked):
        p = l.per_100
        notes = []
        if goal == "lowest_sugar":
            notes.append(f"{p.sugar_g}g sugar/100{'ml' if l.is_beverage else 'g'}")
        elif goal == "highest_protein":
            notes.append(f"{p.protein_g}g protein/100{'ml' if l.is_beverage else 'g'}")
        elif goal == "lowest_sodium":
            notes.append(f"{p.sodium_mg}mg sodium/100{'ml' if l.is_beverage else 'g'}")
        elif goal == "least_processed":
            notes.append(f"{additive_count(l)} additive(s)")
        elif goal == "allergen_safe":
            notes.append(f"{len(l.allergens)} allergen(s) declared")
        else:
            parts = [f"{p.sugar_g}g sugar", f"{p.protein_g}g protein",
                     f"{p.sodium_mg}mg sodium", f"{additive_count(l)} additives"]
            notes = [" | ".join(str(x) for x in parts)]

        trade_offs: list[str] = []
        if l.per_100.sodium_mg and l.per_100.sodium_mg > 600:
            trade_offs.append(f"High sodium ({l.per_100.sodium_mg} mg/100g) — watch for heart-health goals.")
        if l.per_100.sugar_g and l.per_100.sugar_g > 10:
            trade_offs.append(f"Elevated sugar ({l.per_100.sugar_g} g/100g).")
        if additive_count(l) > 5:
            trade_offs.append(f"Contains {additive_count(l)} additives — less suitable for 'least processed'.")
        if l.allergens:
            trade_offs.append(f"Allergens declared: {', '.join(l.allergens)}.")
        if l.extraction_notes:
            trade_offs.append(f"Label caveats: {l.extraction_notes}")

        product_ranks.append(ProductRank(
            rank=i + 1,
            product_name=l.product_name,
            score_note=" | ".join(notes),
            trade_offs=trade_offs,
        ))

    w = product_ranks[0]
    return ComparisonResult(
        goal=goal,
        reasoning=reasoning,
        ranked=product_ranks,
        winner=winner.product_name,
        winner_explanation=(
            f"{winner.product_name} ranks first for '{goal}'. "
            f"{w.score_note}. "
            + (f"Trade-offs: {w.trade_offs[0]}" if w.trade_offs else "No significant trade-offs identified.")
        ),
        trade_offs_for_winner=w.trade_offs,
    )


# ── Verdict holder ─────────────────────────────────────────────────────────────

class AdvisorResult(BaseModel):
    labels: list[LabelData]
    comparison: ComparisonResult | None = None


# ── Rendering ─────────────────────────────────────────────────────────────────

def render_comparison(result: ComparisonResult) -> str:
    lines = [
        f"Goal: {result.goal}",
        "",
        f"Reasoning: {result.reasoning}",
        "",
        f"Winner: {result.winner}",
        f"{result.winner_explanation}",
        "",
        "Ranking:",
    ]
    for r in result.ranked:
        lines.append(f"  #{r.rank} {r.product_name} — {r.score_note}")
        for t in r.trade_offs:
            lines.append(f"      ⚠ {t}")
    if result.trade_offs_for_winner:
        lines.append("")
        lines.append("Watch out for:")
        for t in result.trade_offs_for_winner:
            lines.append(f"  • {t}")
    return "\n".join(lines)


# ── System prompt ─────────────────────────────────────────────────────────────

INSTRUCTIONS = """\
You are a Food Label Advisor. The user will send you photos of nutrition and ingredient labels for two or more similar food or drink products, along with a goal such as "healthiest overall", "lowest sugar", "highest protein", "lowest sodium", "least processed", "best for children", or "allergen safe". Your job is to extract, normalise, and compare the labels so the user can make an informed choice.

Work through these steps, using the provided tools exactly as described:

1. **Detect goal.** Read the user's message. Identify the goal keyword. If no goal is stated, assume "healthiest_overall". Confirm the goal in your reasoning.

2. **Extract each label.** For EACH product image provided, call `extract_label` once. Pass:
   - `product_name` and `brand` as read from the label.
   - Raw values exactly as printed (calories, sugar, fibre, protein, sodium, saturated fat, serving size).
   - Whether the values on the label are per 100 g/ml or per serving — specify this via `values_are_per` ("per_100" or "per_serving").
   - The full `additives` list (E-numbers or names), `allergens` declared on the label, and any marketing `claims`.
   - Whether it is a beverage (`is_beverage`).
   - Any `extraction_notes` for values you had to estimate or that were unclear.

3. **Compare products.** Once all labels are extracted, call `compare_products` with the list of product names (in the order you extracted them) and the detected `goal`. The tool normalises the data and ranks the products.

4. **Submit final answer.** Call `submit_advice` with the `ComparisonResult` returned by `compare_products`. Then write one or two closing sentences summarising the recommendation in plain language.

Rules:
- Do not skip any additive or allergen listed on the label.
- If a value is missing from the label, omit it (leave as null) — do not guess.
- If images are unclear or not food labels, say so and return an error.
- Never raise out of a tool — surface problems in `extraction_notes`.
- Work through ALL images before calling `compare_products`.
"""
