// Package eval is the golden-set batch evaluator: tenant-scoped sets of cases
// (input + a per-case scorer) run against a target agent, scored, and persisted
// as runs + per-case results. Measurement only — never a gate.
package eval

import (
	"errors"
	"fmt"
	"regexp"
	"time"
)

// ScorerKind discriminates how a case's output is scored.
type ScorerKind string

const (
	ScorerExact    ScorerKind = "exact"
	ScorerContains ScorerKind = "contains"
	ScorerRegex    ScorerKind = "regex"
	ScorerJudge    ScorerKind = "judge"
)

// Case is one golden-set entry: an input, how to score the agent's output, and
// the target (Expected literal/pattern, or Rubric for judge grading).
type Case struct {
	Input    string     `json:"input"`
	Scorer   ScorerKind `json:"scorer"`
	Expected string     `json:"expected,omitempty"`
	Rubric   string     `json:"rubric,omitempty"`
}

// Set is a named, tenant-scoped collection of cases.
type Set struct {
	Tenant    string    `json:"tenant"`
	Name      string    `json:"name"`
	Cases     []Case    `json:"cases"`
	CreatedAt time.Time `json:"created_at"`
}

// Run is one execution of a set against an agent.
type Run struct {
	RunID      string     `json:"run_id"`
	Tenant     string     `json:"tenant"`
	SetName    string     `json:"set_name"`
	AgentID    string     `json:"agent_id"`
	Status     string     `json:"status"` // pending|running|completed|error
	Total      int        `json:"total"`
	Passed     int        `json:"passed"`
	Failed     int        `json:"failed"`
	Score      float64    `json:"score"`
	Error      string     `json:"error,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// Result is one case's outcome within a run.
type Result struct {
	CaseIndex int    `json:"case_index"`
	Input     string `json:"input"`
	Output    string `json:"output"`
	Scorer    string `json:"scorer"`
	Passed    bool   `json:"passed"`
	Detail    string `json:"detail,omitempty"`
}

// Run status constants.
const (
	StatusPending   = "pending"
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusError     = "error"
)

// ValidateSet rejects malformed sets at write time (never at run time): a
// non-empty name, at least one case, every case a known scorer with its target
// present, and regex patterns that compile.
func ValidateSet(name string, cases []Case) error {
	if name == "" {
		return errors.New("eval: set name required")
	}
	if len(cases) == 0 {
		return errors.New("eval: set must have at least one case")
	}
	for i, c := range cases {
		switch c.Scorer {
		case ScorerExact, ScorerContains:
			if c.Expected == "" {
				return fmt.Errorf("eval: case %d (%s) requires expected", i, c.Scorer)
			}
		case ScorerRegex:
			if c.Expected == "" {
				return fmt.Errorf("eval: case %d (regex) requires expected", i)
			}
			if _, err := regexp.Compile(c.Expected); err != nil {
				return fmt.Errorf("eval: case %d bad regex: %w", i, err)
			}
		case ScorerJudge:
			if c.Expected == "" && c.Rubric == "" {
				return fmt.Errorf("eval: case %d (judge) requires expected or rubric", i)
			}
		default:
			return fmt.Errorf("eval: case %d unknown scorer %q", i, c.Scorer)
		}
	}
	return nil
}
