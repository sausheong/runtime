package agentruntime

import (
	"context"
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

// TestIsTurnTimeout is the regression test for the harness v0.3.2 contract:
// RunTurn returns a nil error on every path and reports failures on
// TurnResult.StopReason ("aborted" on ctx cancellation, "error" on LLM stream
// errors), so turn-timeout classification MUST happen on the result, not on
// the (always-nil) returned error.
func TestIsTurnTimeout(t *testing.T) {
	cases := []struct {
		name       string
		stopReason string
		runCtxErr  error
		stepCtxErr error
		want       bool
	}{
		{"aborted on deadline", "aborted", context.DeadlineExceeded, nil, true},
		{"error on deadline", "error", context.DeadlineExceeded, nil, true},
		{"completed despite deadline", "completed", context.DeadlineExceeded, nil, false},
		{"aborted without deadline", "aborted", nil, nil, false},
		{"step cancelled (shutdown, not limit)", "aborted", context.DeadlineExceeded, context.Canceled, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTurnTimeout(tc.stopReason, tc.runCtxErr, tc.stepCtxErr); got != tc.want {
				t.Errorf("isTurnTimeout(%q, %v, %v) = %v, want %v",
					tc.stopReason, tc.runCtxErr, tc.stepCtxErr, got, tc.want)
			}
		})
	}
}

func TestBreachEventFormat(t *testing.T) {
	got := breachMsg("max_tokens", 150231, 100000)
	want := "limit exceeded: max_tokens (150231/100000)"
	if got != want {
		t.Errorf("breachMsg = %q, want %q", got, want)
	}
}
