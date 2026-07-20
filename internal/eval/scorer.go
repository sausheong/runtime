package eval

import (
	"context"
	"regexp"
	"strings"
)

// Score returns pass/fail + a short detail for one case. Deterministic scorers
// never error. Judge cases delegate to j; a judge transport error or a nil
// judge FAILS THE CASE (never propagates) so a run always completes.
func Score(ctx context.Context, j Judge, c Case, output string) (bool, string) {
	switch c.Scorer {
	case ScorerExact:
		return output == c.Expected, ""
	case ScorerContains:
		return strings.Contains(output, c.Expected), ""
	case ScorerRegex:
		re, err := regexp.Compile(c.Expected)
		if err != nil {
			return false, "invalid regex: " + err.Error() // unreachable post-ValidateSet
		}
		return re.MatchString(output), ""
	case ScorerJudge:
		if j == nil {
			return false, "judge unavailable: RUNTIME_EVAL_JUDGE_MODEL not set"
		}
		target := c.Rubric
		if target == "" {
			target = c.Expected
		}
		pass, reason, err := j.Grade(ctx, c.Input, target, output)
		if err != nil {
			return false, "judge error: " + err.Error()
		}
		return pass, reason
	default:
		return false, "unknown scorer: " + string(c.Scorer) // unreachable post-ValidateSet
	}
}
