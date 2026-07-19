package config

import (
	"encoding/json"
	"fmt"
	"math"

	"github.com/sausheong/harness/llm"
)

// ModelPrice is the per-model price in dollars per MILLION tokens. Cache prices
// are optional in yaml: CacheWrite defaults to Input, CacheRead defaults to 0.
type ModelPrice struct {
	Input      float64 `yaml:"input"       json:"input"`
	Output     float64 `yaml:"output"      json:"output"`
	CacheWrite float64 `yaml:"cache_write" json:"cache_write"`
	CacheRead  float64 `yaml:"cache_read"  json:"cache_read"`
}

// Cost is the dollar cost of one turn's usage. INCLUDES cache tokens — this
// deliberately differs from the P1.2 max_tokens budget sum (input+output only,
// see agentruntime/turnstep.go sumTokens). Nil usage ⇒ 0.
func (mp ModelPrice) Cost(u *llm.Usage) float64 {
	if u == nil {
		return 0
	}
	return float64(u.InputTokens)*mp.Input/1e6 +
		float64(u.OutputTokens)*mp.Output/1e6 +
		float64(u.CacheCreationInputTokens)*mp.CacheWrite/1e6 +
		float64(u.CacheReadInputTokens)*mp.CacheRead/1e6
}

// JSON is the RUNTIME_AGENT_PRICING wire form for one model's price.
func (mp ModelPrice) JSON() string {
	b, _ := json.Marshal(mp)
	return string(b)
}

// ParseModelPrice decodes the RUNTIME_AGENT_PRICING wire form. "" ⇒ ok=false
// (the model is unpriced), never an error.
func ParseModelPrice(s string) (ModelPrice, bool, error) {
	if s == "" {
		return ModelPrice{}, false, nil
	}
	var mp ModelPrice
	if err := json.Unmarshal([]byte(s), &mp); err != nil {
		return ModelPrice{}, false, fmt.Errorf("config: RUNTIME_AGENT_PRICING: %w", err)
	}
	return mp, true, nil
}

// Pricing is the top-level pricing: block, keyed by the full provider/model
// string (exact match only).
type Pricing struct {
	Currency string                `yaml:"currency"`
	Models   map[string]ModelPrice `yaml:"models"`
}

// PriceFor returns the price for an exact provider/model key.
func (p Pricing) PriceFor(model string) (ModelPrice, bool) {
	mp, ok := p.Models[model]
	return mp, ok
}

// validate rejects malformed prices (negative, NaN, Inf) and defaults optional
// cache prices. An empty Pricing is valid (all models unpriced). Mutates in
// place to apply cache defaults.
func (p *Pricing) validate() error {
	for model, mp := range p.Models {
		for field, v := range map[string]float64{
			"input": mp.Input, "output": mp.Output,
			"cache_write": mp.CacheWrite, "cache_read": mp.CacheRead,
		} {
			if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
				return fmt.Errorf("config: pricing model %q field %q: invalid price %v", model, field, v)
			}
		}
		if mp.CacheWrite == 0 {
			mp.CacheWrite = mp.Input // default cache_write to input
		}
		p.Models[model] = mp
	}
	return nil
}
