package controlplane

import (
	"testing"

	"github.com/sausheong/runtime/internal/config"
)

func TestRegistry_FromConfig(t *testing.T) {
	cfg := &config.Config{Agents: []config.AgentConfig{
		{ID: "a", Name: "A", Model: "m", ListenAddr: "127.0.0.1:8101"},
		{ID: "b", Name: "B", Model: "m", ListenAddr: "127.0.0.1:8102"},
	}}
	reg := NewRegistry(cfg, "/bin/agentd", "dsn")
	if len(reg.List()) != 2 {
		t.Fatalf("List = %d, want 2", len(reg.List()))
	}
	ap, ok := reg.Get("a")
	if !ok || ap.Addr != "127.0.0.1:8101" || ap.AgentID != "a" {
		t.Fatalf("Get(a) = %+v ok=%v", ap, ok)
	}
	if _, ok := reg.Get("nope"); ok {
		t.Fatal("Get(nope) should be !ok")
	}
}
