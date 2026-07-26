package store

import "testing"

func TestAgentDBOSSchemaIsStableAndAgentScoped(t *testing.T) {
	a1 := AgentDBOSSchema("alpha", "agent-a")
	a1Again := AgentDBOSSchema("alpha", "agent-a")
	a2 := AgentDBOSSchema("alpha", "agent-b")
	otherTenant := AgentDBOSSchema("beta", "agent-a")
	if a1 != a1Again {
		t.Fatalf("schema changed across restart: %q != %q", a1, a1Again)
	}
	if a1 == a2 || a1 == otherTenant {
		t.Fatalf("schema collision: a1=%q a2=%q otherTenant=%q", a1, a2, otherTenant)
	}
	if len(a1) > 63 {
		t.Fatalf("schema exceeds PostgreSQL identifier limit: %q", a1)
	}
}
