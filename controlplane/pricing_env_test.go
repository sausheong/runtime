package controlplane

import (
	"context"
	"strings"
	"testing"
)

func TestEnvDeltaCarriesPricing(t *testing.T) {
	ap := AgentProcess{
		AgentID: "a1", PGDSN: "postgres://x", Addr: "127.0.0.1:9000", Tenant: "acme",
		PricingJSON: `{"input":15,"output":75,"cache_write":15,"cache_read":0}`,
	}
	env, err := ap.envDelta(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var got string
	for _, e := range env {
		if strings.HasPrefix(e, "RUNTIME_AGENT_PRICING=") {
			got = e
		}
	}
	if got != `RUNTIME_AGENT_PRICING={"input":15,"output":75,"cache_write":15,"cache_read":0}` {
		t.Fatalf("pricing env = %q", got)
	}
}

func TestEnvDeltaEmptyPricingWhenUnpriced(t *testing.T) {
	ap := AgentProcess{AgentID: "a1", PGDSN: "x", Addr: "y", Tenant: "acme"} // PricingJSON ""
	env, _ := ap.envDelta(context.Background())
	found := false
	for _, e := range env {
		if e == "RUNTIME_AGENT_PRICING=" {
			found = true
		}
	}
	if !found {
		t.Fatal("unpriced agent must still emit explicit empty RUNTIME_AGENT_PRICING= (no inherited smuggling)")
	}
}
