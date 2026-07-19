package config

import (
	"math"
	"testing"

	"github.com/sausheong/harness/llm"
)

func TestModelPriceCost(t *testing.T) {
	mp := ModelPrice{Input: 15, Output: 75, CacheWrite: 18.75, CacheRead: 1.5}
	u := &llm.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000,
		CacheCreationInputTokens: 1_000_000, CacheReadInputTokens: 1_000_000}
	// 15 + 75 + 18.75 + 1.5 = 110.25
	if got := mp.Cost(u); math.Abs(got-110.25) > 1e-9 {
		t.Fatalf("Cost = %v, want 110.25", got)
	}
	if got := mp.Cost(nil); got != 0 {
		t.Fatalf("Cost(nil) = %v, want 0", got)
	}
}

// Cost includes cache tokens; the P1.2 budget (input+output) does not. Prove
// they diverge so nobody reconciles them.
func TestCostDivergesFromBudgetSum(t *testing.T) {
	mp := ModelPrice{Input: 1, Output: 1, CacheWrite: 1, CacheRead: 1}
	u := &llm.Usage{InputTokens: 1_000_000, OutputTokens: 0,
		CacheCreationInputTokens: 1_000_000, CacheReadInputTokens: 1_000_000}
	// cost counts all four directions = 3.0; budget sum would count only 1e6.
	if got := mp.Cost(u); math.Abs(got-3.0) > 1e-9 {
		t.Fatalf("Cost = %v, want 3.0 (cache included)", got)
	}
}

func TestPricingPriceFor(t *testing.T) {
	p := Pricing{Models: map[string]ModelPrice{"anthropic/claude-opus-4-8": {Input: 15, Output: 75}}}
	if _, ok := p.PriceFor("anthropic/claude-opus-4-8"); !ok {
		t.Fatal("known model must resolve")
	}
	if _, ok := p.PriceFor("openai/gpt-4o"); ok {
		t.Fatal("unknown model must not resolve")
	}
}

func TestModelPriceJSONRoundTrip(t *testing.T) {
	mp := ModelPrice{Input: 2.5, Output: 10, CacheWrite: 2.5, CacheRead: 0.25}
	got, ok, err := ParseModelPrice(mp.JSON())
	if err != nil || !ok || got != mp {
		t.Fatalf("round-trip: got=%+v ok=%v err=%v", got, ok, err)
	}
	if _, ok, _ := ParseModelPrice(""); ok {
		t.Fatal("empty string must parse to ok=false (unpriced)")
	}
}

func TestPricingValidateRejectsNegative(t *testing.T) {
	p := Pricing{Models: map[string]ModelPrice{"m": {Input: -1, Output: 1}}}
	if err := p.validate(); err == nil {
		t.Fatal("negative price must be rejected")
	}
}

func TestPricingValidateDefaultsCacheWrite(t *testing.T) {
	p := Pricing{Models: map[string]ModelPrice{"m": {Input: 10, Output: 20}}}
	if err := p.validate(); err != nil {
		t.Fatal(err)
	}
	if got := p.Models["m"].CacheWrite; got != 10 {
		t.Fatalf("cache_write default = %v, want 10 (=input)", got)
	}
}

func TestPricingEmptyIsValid(t *testing.T) {
	var p Pricing
	if err := p.validate(); err != nil {
		t.Fatalf("empty pricing must be valid: %v", err)
	}
	if _, ok := p.PriceFor("anything"); ok {
		t.Fatal("empty pricing resolves nothing")
	}
}
