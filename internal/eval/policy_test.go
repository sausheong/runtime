package eval

import "testing"

func TestValidatePolicy(t *testing.T) {
	good := Policy{Tenant: "t1", AgentID: "a1", SampleRate: 50, Criteria: []Criterion{
		{Name: "polite", Scorer: ScorerJudge, Rubric: "must be polite"},
		{Name: "has-src", Scorer: ScorerContains, Pattern: "source:"},
		{Name: "fmt", Scorer: ScorerRegex, Pattern: "^\\d+$"},
	}}
	if err := ValidatePolicy(good); err != nil {
		t.Fatalf("valid policy rejected: %v", err)
	}
	bad := []struct {
		name string
		p    Policy
	}{
		{"rate-neg", Policy{AgentID: "a", SampleRate: -1, Criteria: good.Criteria}},
		{"rate-over", Policy{AgentID: "a", SampleRate: 101, Criteria: good.Criteria}},
		{"no-agent", Policy{AgentID: "", SampleRate: 10, Criteria: good.Criteria}},
		{"no-criteria", Policy{AgentID: "a", SampleRate: 10}},
		{"unknown-scorer", Policy{AgentID: "a", SampleRate: 10, Criteria: []Criterion{{Name: "x", Scorer: "bogus", Pattern: "y"}}}},
		{"exact-rejected", Policy{AgentID: "a", SampleRate: 10, Criteria: []Criterion{{Name: "x", Scorer: ScorerExact, Pattern: "y"}}}},
		{"judge-no-rubric", Policy{AgentID: "a", SampleRate: 10, Criteria: []Criterion{{Name: "x", Scorer: ScorerJudge}}}},
		{"regex-no-pattern", Policy{AgentID: "a", SampleRate: 10, Criteria: []Criterion{{Name: "x", Scorer: ScorerRegex}}}},
		{"regex-bad", Policy{AgentID: "a", SampleRate: 10, Criteria: []Criterion{{Name: "x", Scorer: ScorerRegex, Pattern: "("}}}},
		{"no-name", Policy{AgentID: "a", SampleRate: 10, Criteria: []Criterion{{Name: "", Scorer: ScorerContains, Pattern: "y"}}}},
	}
	for _, b := range bad {
		if err := ValidatePolicy(b.p); err == nil {
			t.Errorf("%s: expected rejection, got nil", b.name)
		}
	}
}

func TestCriterionToCase(t *testing.T) {
	j := Criterion{Name: "n", Scorer: ScorerJudge, Rubric: "be nice"}.toCase()
	if j.Scorer != ScorerJudge || j.Rubric != "be nice" {
		t.Errorf("judge mapping wrong: %+v", j)
	}
	r := Criterion{Name: "n", Scorer: ScorerContains, Pattern: "foo"}.toCase()
	if r.Scorer != ScorerContains || r.Expected != "foo" {
		t.Errorf("contains mapping wrong: %+v", r)
	}
}
