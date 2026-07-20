package eval

import "testing"

func TestValidateSet(t *testing.T) {
	good := []Case{
		{Input: "hi", Scorer: ScorerExact, Expected: "hello"},
		{Input: "q", Scorer: ScorerContains, Expected: "sub"},
		{Input: "q", Scorer: ScorerRegex, Expected: "^ab.*z$"},
		{Input: "q", Scorer: ScorerJudge, Rubric: "must be polite"},
		{Input: "q", Scorer: ScorerJudge, Expected: "42"},
	}
	if err := ValidateSet("myset", good); err != nil {
		t.Fatalf("valid set rejected: %v", err)
	}
	bad := []struct {
		name  string
		cases []Case
	}{
		{"", good},
		{"empty", nil},
		{"unknown-scorer", []Case{{Input: "x", Scorer: "bogus", Expected: "y"}}},
		{"bad-regex", []Case{{Input: "x", Scorer: ScorerRegex, Expected: "("}}},
		{"exact-no-expected", []Case{{Input: "x", Scorer: ScorerExact}}},
		{"judge-no-target", []Case{{Input: "x", Scorer: ScorerJudge}}},
	}
	for _, b := range bad {
		if err := ValidateSet(b.name, b.cases); err == nil {
			t.Errorf("%s: expected rejection, got nil", b.name)
		}
	}
}
