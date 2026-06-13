package controlplane

import (
	"context"
	"strconv"
	"testing"

	"github.com/sausheong/runtime/internal/config"
)

func TestRegistry_ReplicaSetExpansion(t *testing.T) {
	cfg := &config.Config{Agents: []config.AgentConfig{
		{ID: "support", Name: "S", Model: "m", ListenAddr: "127.0.0.1:8101", Replicas: 3, Tenant: "default"},
	}}
	r := NewRegistry(cfg, "/bin/agentd", "dsn")
	set, ok := r.Replicas("support")
	if !ok || len(set) != 3 {
		t.Fatalf("Replicas: ok=%v len=%d, want 3", ok, len(set))
	}
	for i, ap := range set {
		if ap.ReplicaIndex != i {
			t.Errorf("replica %d: ReplicaIndex=%d", i, ap.ReplicaIndex)
		}
		wantVMID := "support#" + strconv.Itoa(i)
		if ap.DBOSVMID != wantVMID {
			t.Errorf("replica %d: DBOSVMID=%q want %q", i, ap.DBOSVMID, wantVMID)
		}
		wantAddr := "127.0.0.1:" + strconv.Itoa(8101+i)
		if ap.Addr != wantAddr {
			t.Errorf("replica %d: Addr=%q want %q", i, ap.Addr, wantAddr)
		}
	}
}

func TestRegistry_NextReplicaRoundRobin(t *testing.T) {
	cfg := &config.Config{Agents: []config.AgentConfig{
		{ID: "a", Name: "A", Model: "m", ListenAddr: "127.0.0.1:8201", Replicas: 2, Tenant: "default"},
	}}
	r := NewRegistry(cfg, "/bin/agentd", "dsn")
	got := []int{r.NextReplica("a"), r.NextReplica("a"), r.NextReplica("a"), r.NextReplica("a")}
	want := []int{0, 1, 0, 1}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("NextReplica seq: got %v want %v", got, want)
		}
	}
}

func TestRegistry_RemoteSingleReplica(t *testing.T) {
	cfg := &config.Config{Agents: []config.AgentConfig{
		{ID: "rem", Name: "R", Model: "m", URL: "https://h:8443", Tenant: "default"},
	}}
	r := NewRegistry(cfg, "/bin/agentd", "dsn")
	set, ok := r.Replicas("rem")
	if !ok || len(set) != 1 {
		t.Fatalf("remote Replicas: ok=%v len=%d, want 1", ok, len(set))
	}
	if !set[0].Remote || set[0].DBOSVMID != "" || set[0].BaseURL != "https://h:8443" {
		t.Fatalf("remote replica fields wrong: %+v", set[0])
	}
}

func TestRegistry_RemotePoolExpansion(t *testing.T) {
	cfg := &config.Config{Agents: []config.AgentConfig{
		{ID: "support", Name: "S", Model: "m",
			URL: "http://support-{i}.support-hl.ns.svc:8080", Replicas: 3,
			AuthToken: "tok", Tenant: "default"},
	}}
	r := NewRegistry(cfg, "/bin/agentd", "dsn")
	set, ok := r.Replicas("support")
	if !ok || len(set) != 3 {
		t.Fatalf("Replicas: ok=%v len=%d, want 3", ok, len(set))
	}
	for i, ap := range set {
		if !ap.Remote {
			t.Errorf("replica %d: Remote=false, want true", i)
		}
		if ap.ReplicaIndex != i {
			t.Errorf("replica %d: ReplicaIndex=%d", i, ap.ReplicaIndex)
		}
		want := "http://support-" + strconv.Itoa(i) + ".support-hl.ns.svc:8080"
		if ap.BaseURL != want {
			t.Errorf("replica %d: BaseURL=%q want %q", i, ap.BaseURL, want)
		}
		if ap.DBOSVMID != "" {
			t.Errorf("replica %d: DBOSVMID=%q, want empty (remote owns its id)", i, ap.DBOSVMID)
		}
		if ap.AuthToken != "tok" {
			t.Errorf("replica %d: AuthToken=%q", i, ap.AuthToken)
		}
	}
}

func TestRegistry_ReplicaByIndex(t *testing.T) {
	cfg := &config.Config{Agents: []config.AgentConfig{
		{ID: "a", Name: "A", Model: "m", ListenAddr: "127.0.0.1:8301", Replicas: 2, Tenant: "default"},
	}}
	r := NewRegistry(cfg, "/bin/agentd", "dsn")
	ap, ok := r.Replica("a", 1)
	if !ok || ap.ReplicaIndex != 1 {
		t.Fatalf("Replica(a,1): ok=%v idx=%d", ok, ap.ReplicaIndex)
	}
	if _, ok := r.Replica("a", 2); ok {
		t.Fatal("Replica(a,2) should be out of range")
	}
	if _, ok := r.Replica("nope", 0); ok {
		t.Fatal("Replica(nope,0) should be unknown")
	}
}

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

func TestRegistryDelegatesAutoscaledAgent(t *testing.T) {
	cfg := &config.Config{Agents: []config.AgentConfig{
		{ID: "as", Name: "AS", Model: "m", ListenAddr: "127.0.0.1:9300",
			Autoscale: &config.AutoscaleConfig{Min: 1, Max: 3, TargetSessionsPerReplica: 2}},
		{ID: "st", Name: "ST", Model: "m", ListenAddr: "127.0.0.1:9400", Replicas: 2},
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(cfg, "/bin/true", "dsn")

	if reg.pools["as"] == nil {
		t.Fatal("expected PoolManager for autoscaled agent")
	}
	if reg.pools["st"] != nil {
		t.Fatal("static agent must not have a PoolManager")
	}
	st, ok := reg.Replicas("st")
	if !ok || len(st) != 2 {
		t.Fatalf("static replicas = %d, ok=%v; want 2", len(st), ok)
	}

	pm := reg.pools["as"]
	pm.startReplica = func(ctx context.Context, ap AgentProcess) (context.CancelFunc, error) {
		_, c := context.WithCancel(ctx)
		return c, nil
	}
	if err := pm.grow(context.Background()); err != nil {
		t.Fatal(err)
	}
	reps, ok := reg.Replicas("as")
	if !ok || len(reps) != 1 || reps[0].DBOSVMID != "as#0" {
		t.Fatalf("delegated Replicas wrong: %+v ok=%v", reps, ok)
	}
}

func TestRegistrySetBrokerStampsPool(t *testing.T) {
	cfg := &config.Config{Agents: []config.AgentConfig{
		{ID: "as", Name: "AS", Model: "m", ListenAddr: "127.0.0.1:9300",
			Autoscale: &config.AutoscaleConfig{Min: 1, Max: 2, TargetSessionsPerReplica: 2}},
	}}
	_ = cfg.Validate()
	reg := NewRegistry(cfg, "/bin/true", "dsn")
	reg.SetBroker(stubBroker{})
	pm := reg.pools["as"]
	if pm.base.broker == nil {
		t.Fatal("SetBroker did not stamp the PoolManager base")
	}
}

type stubBroker struct{}

func (stubBroker) SecretsFor(context.Context, string) (map[string]string, error) { return nil, nil }

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
