package controlplane

import "github.com/sausheong/runtime/internal/config"

// AgentInfo is the public description of a registered agent.
type AgentInfo struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Model  string `json:"model"`
	Tenant string `json:"tenant"`
}

// Registry holds the agents the control plane hosts, built from config.
// Read-only after construction except for the optional secret broker.
type Registry struct {
	order  []string
	agents map[string]AgentProcess
	infos  map[string]AgentInfo
	broker SecretBroker // optional; injected into each AgentProcess on Get.
}

// NewRegistry builds a Registry from parsed config. binPath is the agentd
// binary all agents run; dsn is the shared Postgres DSN.
func NewRegistry(cfg *config.Config, binPath, dsn string) *Registry {
	r := &Registry{agents: map[string]AgentProcess{}, infos: map[string]AgentInfo{}}
	for _, a := range cfg.Agents {
		r.order = append(r.order, a.ID)
		r.agents[a.ID] = AgentProcess{
			AgentID: a.ID, Addr: a.ListenAddr, BinPath: binPath, PGDSN: dsn,
			Kind: a.Kind, Command: a.Command, WorkDir: a.WorkDir, Tenant: a.Tenant,
			Memory: a.Memory, GatewayOn: a.Gateway,
		}
		r.infos[a.ID] = AgentInfo{ID: a.ID, Name: a.Name, Model: a.Model, Tenant: a.Tenant}
	}
	return r
}

// SetBroker installs the secret broker injected into every AgentProcess returned
// by Get. It is NOT safe to call concurrently with Get: SetBroker must complete
// (happen-before) the HTTP server and the supervisor goroutines start. In
// practice it is called once during startup, before either begins. nil ⇒ no
// brokering.
func (r *Registry) SetBroker(b SecretBroker) { r.broker = b }

// SetGateway records the gateway endpoint URL and per-tenant agent keys,
// stamped onto every gateway-enabled AgentProcess. Like SetBroker, it must
// complete before the HTTP server and supervisor goroutines start.
func (r *Registry) SetGateway(url string, keys map[string]string) {
	for id, ap := range r.agents {
		if !ap.GatewayOn {
			continue
		}
		ap.GatewayURL = url
		ap.GatewayKey = keys[ap.Tenant]
		r.agents[id] = ap
	}
}

// AgentTenants returns agentID→tenantID for all registered agents.
func (r *Registry) AgentTenants() map[string]string {
	m := make(map[string]string, len(r.order))
	for _, id := range r.order {
		m[id] = r.agents[id].Tenant
	}
	return m
}

// Get returns the AgentProcess for id, with the registry's secret broker
// attached so its SpawnFunc brokers secrets.
func (r *Registry) Get(id string) (AgentProcess, bool) {
	ap, ok := r.agents[id]
	if ok {
		// ap is a copy (agents is a value-typed map): mutate the copy so the
		// broker rides along on what callers get, never the stored entry. If
		// this map ever becomes map[string]*AgentProcess, this would mutate the
		// shared entry and must be revisited.
		ap.broker = r.broker
	}
	return ap, ok
}

// List returns agent infos in config order.
func (r *Registry) List() []AgentInfo {
	out := make([]AgentInfo, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.infos[id])
	}
	return out
}
