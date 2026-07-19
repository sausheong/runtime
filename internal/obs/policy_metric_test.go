package obs

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestPolicyDecision(t *testing.T) {
	c := NewControlMetrics()
	c.PolicyDecision("acme", "allow")
	c.PolicyDecision("acme", "deny")
	c.PolicyDecision("acme", "deny")
	c.PolicyDecision("globex", "error")
	want := `
# HELP runtime_gateway_policy_decisions_total Gateway policy decisions, by tenant and decision (allow/deny/error).
# TYPE runtime_gateway_policy_decisions_total counter
runtime_gateway_policy_decisions_total{decision="allow",tenant="acme"} 1
runtime_gateway_policy_decisions_total{decision="deny",tenant="acme"} 2
runtime_gateway_policy_decisions_total{decision="error",tenant="globex"} 1
`
	if err := testutil.GatherAndCompare(c.reg, strings.NewReader(want), "runtime_gateway_policy_decisions_total"); err != nil {
		t.Error(err)
	}
}

func TestPolicyDecisionNilSafe(t *testing.T) {
	var c *ControlMetrics
	c.PolicyDecision("acme", "deny") // must not panic
}
