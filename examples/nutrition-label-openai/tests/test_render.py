"""Hermetic unit test for render_verdict — no API key or network."""
from agent import NutritionVerdict, Finding, render_verdict


def _verdict() -> NutritionVerdict:
    return NutritionVerdict(
        reasoning="Read the label; classified each additive.",
        product_name="Milo UHT",
        summary="A chocolate malt beverage, Nutri-Grade C.",
        green=[Finding(category="GREEN", finding="Soy lecithin (E322) permitted", source="check_sfa_additive")],
        amber=[Finding(category="AMBER", finding="Moderate sugar", source="label")],
        red=[Finding(category="RED", finding="High saturated fat", source="label")],
        recommendation="Okay in moderation.",
    )


def test_render_includes_all_sections():
    text = render_verdict(_verdict())
    assert "Product: Milo UHT" in text
    assert "Reasoning: Read the label" in text
    assert "Summary: A chocolate malt beverage" in text
    assert "🟢 Soy lecithin (E322) permitted  [check_sfa_additive]" in text
    assert "🟡 Moderate sugar  [label]" in text
    assert "🔴 High saturated fat  [label]" in text
    assert "Recommendation: Okay in moderation." in text
