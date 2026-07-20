package eval

import (
	"errors"
	"fmt"
	"regexp"
	"time"
)

// Criterion is one online-scoring rule in a per-agent eval policy. Scorer is
// judge (LLM-as-judge against Rubric) or contains/regex (deterministic on the
// output via Pattern). exact is NOT allowed online (a live output rarely equals
// a fixed string).
type Criterion struct {
	Name    string     `json:"name"`
	Scorer  ScorerKind `json:"scorer"`
	Rubric  string     `json:"rubric,omitempty"`  // judge
	Pattern string     `json:"pattern,omitempty"` // contains/regex
}

// Policy is an agent's standing online-sampling config: sample SampleRate% of
// finished sessions and score each against every Criterion.
type Policy struct {
	Tenant     string      `json:"tenant"`
	AgentID    string      `json:"agent_id"`
	SampleRate int         `json:"sample_rate"` // 0..100 (percent)
	Criteria   []Criterion `json:"criteria"`
	CreatedAt  time.Time   `json:"created_at"`
}

// toCase maps a Criterion onto the M1 Case scoring vocabulary so eval.Score can
// evaluate it: judge carries Rubric, contains/regex carry Pattern as Expected.
func (c Criterion) toCase() Case {
	switch c.Scorer {
	case ScorerJudge:
		return Case{Scorer: ScorerJudge, Rubric: c.Rubric}
	default: // contains / regex
		return Case{Scorer: c.Scorer, Expected: c.Pattern}
	}
}

// ValidatePolicy rejects malformed policies at write time (never at scoring
// time): rate 0..100, >=1 criterion, each criterion a known online scorer
// (contains/regex/judge — NOT exact) with its field present, regex compiles.
func ValidatePolicy(p Policy) error {
	if p.AgentID == "" {
		return errors.New("eval: policy agent_id required")
	}
	if p.SampleRate < 0 || p.SampleRate > 100 {
		return fmt.Errorf("eval: sample_rate must be 0..100, got %d", p.SampleRate)
	}
	if len(p.Criteria) == 0 {
		return errors.New("eval: policy must have at least one criterion")
	}
	for i, c := range p.Criteria {
		if c.Name == "" {
			return fmt.Errorf("eval: criterion %d requires a name", i)
		}
		switch c.Scorer {
		case ScorerContains:
			if c.Pattern == "" {
				return fmt.Errorf("eval: criterion %q (contains) requires pattern", c.Name)
			}
		case ScorerRegex:
			if c.Pattern == "" {
				return fmt.Errorf("eval: criterion %q (regex) requires pattern", c.Name)
			}
			if _, err := regexp.Compile(c.Pattern); err != nil {
				return fmt.Errorf("eval: criterion %q bad regex: %w", c.Name, err)
			}
		case ScorerJudge:
			if c.Rubric == "" {
				return fmt.Errorf("eval: criterion %q (judge) requires rubric", c.Name)
			}
		case ScorerExact:
			return fmt.Errorf("eval: criterion %q uses exact, not allowed online (use contains/regex/judge)", c.Name)
		default:
			return fmt.Errorf("eval: criterion %q unknown scorer %q", c.Name, c.Scorer)
		}
	}
	return nil
}
