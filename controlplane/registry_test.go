package controlplane

import (
	"context"
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

func TestRegistryThreadsGateway(t *testing.T) {
	cfg := &config.Config{
		Agents: []config.AgentConfig{
			{ID: "g", Name: "G", Model: "m", ListenAddr: "127.0.0.1:1", Tenant: "acme", Gateway: config.GatewayFull},
			{ID: "p", Name: "P", Model: "m", ListenAddr: "127.0.0.1:2"},
		},
		Gateway: config.GatewayConfig{
			Servers:   []config.GatewayServer{{Name: "fs", Command: "x"}},
			AgentKeys: map[string]string{"acme": "svk-acme"},
			SelfURL:   "http://127.0.0.1:9999",
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	r := NewRegistry(cfg, "bin", "dsn")
	r.SetGateway("http://127.0.0.1:9999/gateway/mcp", cfg.Gateway.AgentKeys)

	g, _ := r.Get("g")
	if !g.GatewayOn || g.GatewayURL != "http://127.0.0.1:9999/gateway/mcp" || g.GatewayKey != "svk-acme" {
		t.Fatalf("gateway agent not wired: %+v", g)
	}
	p, _ := r.Get("p")
	if p.GatewayOn || p.GatewayURL != "" || p.GatewayKey != "" {
		t.Fatalf("non-gateway agent leaked gateway env: %+v", p)
	}
}

func TestRegistry_GetInjectsBroker(t *testing.T) {
	cfg := &config.Config{Agents: []config.AgentConfig{
		{ID: "a1", ListenAddr: "127.0.0.1:9001", Tenant: "alpha"},
	}}
	reg := NewRegistry(cfg, "./agentd", "dsn")

	// Before SetBroker: the AgentProcess has no broker.
	ap, ok := reg.Get("a1")
	if !ok {
		t.Fatal("agent a1 missing")
	}
	if ap.broker != nil {
		t.Fatal("broker should be nil before SetBroker")
	}

	br := fakeBroker{secrets: map[string]map[string]string{"alpha": {"K": "v"}}}
	reg.SetBroker(br)
	ap2, _ := reg.Get("a1")
	if ap2.broker == nil {
		t.Fatal("Get must inject the registry broker into the AgentProcess")
	}
	env, err := ap2.buildEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if lastIndexWithPrefix(env, "K=v") < 0 {
		t.Fatalf("brokered secret not in env: %v", env)
	}
}

func TestRegistryThreadsGatewaySearch(t *testing.T) {
	cfg := &config.Config{
		Agents: []config.AgentConfig{
			{ID: "s", Name: "S", Model: "m", ListenAddr: "127.0.0.1:1", Gateway: config.GatewaySearch},
			{ID: "f", Name: "F", Model: "m", ListenAddr: "127.0.0.1:2", Gateway: config.GatewayFull},
		},
		Gateway: config.GatewayConfig{Servers: []config.GatewayServer{{Name: "fs", Command: "x"}}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	r := NewRegistry(cfg, "bin", "dsn")
	s, _ := r.Get("s")
	if !s.GatewayOn || !s.GatewaySearch {
		t.Fatalf("search agent not threaded: %+v", s)
	}
	f, _ := r.Get("f")
	if !f.GatewayOn || f.GatewaySearch {
		t.Fatalf("full agent wrong: %+v", f)
	}
}

func TestRegistry_RemoteAgentDialIdentity(t *testing.T) {
	cfg := &config.Config{Agents: []config.AgentConfig{
		{ID: "local", Name: "L", Model: "m", ListenAddr: "127.0.0.1:8101"},
		{ID: "remote", Name: "R", Model: "m", URL: "https://h:8443", AuthToken: "tok"},
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(cfg, "/bin/agentd", "dsn")

	l, _ := reg.Get("local")
	if l.Remote {
		t.Fatal("local agent marked Remote")
	}
	if l.baseURL() != "http://127.0.0.1:8101" || l.Addr != "127.0.0.1:8101" {
		t.Fatalf("local dial wrong: base=%q addr=%q", l.baseURL(), l.Addr)
	}

	r, _ := reg.Get("remote")
	if !r.Remote {
		t.Fatal("remote agent not marked Remote")
	}
	if r.baseURL() != "https://h:8443" {
		t.Fatalf("remote baseURL = %q", r.baseURL())
	}
	if r.AuthToken != "tok" {
		t.Fatalf("remote AuthToken = %q", r.AuthToken)
	}
	if r.Addr != "" {
		t.Fatalf("remote Addr should be empty, got %q", r.Addr)
	}
}
