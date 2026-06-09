package agentkind

import "testing"

func TestGetKnownKinds(t *testing.T) {
	for _, k := range []string{"", "testagent", "nutrition"} {
		if _, ok := Get(k); !ok {
			t.Errorf("kind %q: expected a builder", k)
		}
	}
	if _, ok := Get("does-not-exist"); ok {
		t.Error("unknown kind should not resolve")
	}
}

func TestBuildTestAgent_NoMemoryToolByDefault(t *testing.T) {
	build, _ := Get("testagent")
	cfg, err := build(Deps{AgentID: "a1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Tools.Get("memory"); ok {
		t.Fatal("memory tool must be absent when Deps.Memory is false")
	}
}

func TestBuildTestAgent_MemoryEnabledRequiresDB(t *testing.T) {
	build, _ := Get("testagent")
	_, err := build(Deps{AgentID: "a1", Memory: true, Tenant: "alpha", DB: nil})
	if err == nil {
		t.Fatal("memory enabled with nil DB must error (fail fast)")
	}
}
