package eval

import (
	"context"
	"errors"
	"regexp"
	"strings"
)

var ErrJudgeUnavailable = errors.New("eval: judge unavailable")

// Score returns pass/fail + a short detail for one case. Deterministic scorers
// never error. Judge cases delegate to j; a judge transport error or a nil
// judge FAILS THE CASE (never propagates) so a run always completes.
func Score(ctx context.Context, j Judge, c Case, output string) (bool, string) {
	passed, detail, err := ScoreChecked(ctx, j, c, output)
	if err != nil {
		return false, detail
	}
	return passed, detail
}

// ScoreChecked preserves the distinction between a real failed criterion and
// scoring infrastructure failure. Offline runs intentionally use Score's
// fail-the-case compatibility contract; online classification uses this form
// so a missing or failed judge cannot be mislabeled as a quality failure.
func ScoreChecked(ctx context.Context, j Judge, c Case, output string) (bool, string, error) {
	switch c.Scorer {
	case ScorerExact:
		return output == c.Expected, "", nil
	case ScorerContains:
		return strings.Contains(output, c.Expected), "", nil
	case ScorerRegex:
		re, err := regexp.Compile(c.Expected)
		if err != nil {
			detail := "invalid regex: " + err.Error()
			return false, detail, errors.New(detail) // unreachable post-ValidateSet
		}
		return re.MatchString(output), "", nil
	case ScorerJudge:
		if j == nil {
			return false, "judge unavailable: RUNTIME_EVAL_JUDGE_MODEL not set",
				ErrJudgeUnavailable
		}
		target := c.Rubric
		if target == "" {
			target = c.Expected
		}
		pass, reason, err := j.Grade(ctx, c.Input, target, output)
		if err != nil {
			return false, "judge error: " + err.Error(), err
		}
		return pass, reason, nil
	default:
		detail := "unknown scorer: " + string(c.Scorer)
		return false, detail, errors.New(detail) // unreachable post-ValidateSet
	}
}
