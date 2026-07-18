package agentruntime

import (
	"testing"

	"github.com/sausheong/harness/llm"
	"github.com/sausheong/runtime/internal/config"
)

func TestSumTokens(t *testing.T) {
	outs := []turnOutput{
		{Usage: &llm.Usage{InputTokens: 100, OutputTokens: 50}},
		{Usage: nil}, // old checkpoint / no usage reported ⇒ counts 0
		{Usage: &llm.Usage{InputTokens: 10, OutputTokens: 5, CacheReadInputTokens: 999}},
	}
	total := 0
	for _, o := range outs {
		total += sumTokens(o.Usage)
	}
	if total != 165 { // cache tokens excluded by design
		t.Errorf("total = %d, want 165", total)
	}
}

func TestParseLimitsEnv(t *testing.T) {
	l, err := parseLimits(`{"turn_timeout_ms":120000,"max_tokens":200000}`)
	if err != nil {
		t.Fatal(err)
	}
	if l.TurnTimeoutMS != 120000 || l.MaxTokens != 200000 || l.MaxTurns != 0 {
		t.Errorf("parsed = %+v", l)
	}
	if l2, err := parseLimits(""); err != nil || !l2.Empty() {
		t.Errorf("empty env: %+v err=%v, want empty limits nil err", l2, err)
	}
	if _, err := parseLimits("{bad"); err == nil {
		t.Error("malformed JSON: want error")
	}
}

func TestEffectiveMaxTurns(t *testing.T) {
	if got := effectiveMaxTurns(config.Limits{}, 0); got != 25 {
		t.Errorf("no limit, no spec: %d want 25 (legacy fallback)", got)
	}
	if got := effectiveMaxTurns(config.Limits{}, 40); got != 40 {
		t.Errorf("spec only: %d want 40", got)
	}
	if got := effectiveMaxTurns(config.Limits{MaxTurns: 3}, 40); got != 3 {
		t.Errorf("limit wins: %d want 3", got)
	}
}

func TestBreachEventFormat(t *testing.T) {
	got := breachMsg("max_tokens", 150231, 100000)
	want := "limit exceeded: max_tokens (150231/100000)"
	if got != want {
		t.Errorf("breachMsg = %q, want %q", got, want)
	}
}
