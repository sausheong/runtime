package obs

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestLimitHitObserved(t *testing.T) {
	m := NewAgentMetrics("a1")
	m.LimitHitObserved("max_tokens")
	m.LimitHitObserved("max_tokens")
	m.LimitHitObserved("turn_timeout")
	want := `
# HELP runtime_session_limit_hits_total Sessions terminated by a lifecycle limit, by limit name.
# TYPE runtime_session_limit_hits_total counter
runtime_session_limit_hits_total{agent="a1",limit="max_tokens"} 2
runtime_session_limit_hits_total{agent="a1",limit="turn_timeout"} 1
`
	if err := testutil.GatherAndCompare(m.reg, strings.NewReader(want), "runtime_session_limit_hits_total"); err != nil {
		t.Error(err)
	}
}

func TestLimitHitObservedNilSafe(t *testing.T) {
	var m *AgentMetrics
	m.LimitHitObserved("max_turns") // must not panic
}
