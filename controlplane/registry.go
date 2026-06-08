package controlplane

import "github.com/sausheong/runtime/internal/config"

// AgentInfo is the public description of a registered agent.
type AgentInfo struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Model string `json:"model"`
}

// Registry holds the agents the control plane hosts, built from config.
// Read-only after construction in M2 (config-driven).
type Registry struct {
	order  []string
	agents map[string]AgentProcess
	infos  map[string]AgentInfo
}

// NewRegistry builds a Registry from parsed config. binPath is the agentd
// binary all agents run; dsn is the shared Postgres DSN.
func NewRegistry(cfg *config.Config, binPath, dsn string) *Registry {
	r := &Registry{agents: map[string]AgentProcess{}, infos: map[string]AgentInfo{}}
	for _, a := range cfg.Agents {
		r.order = append(r.order, a.ID)
		r.agents[a.ID] = AgentProcess{AgentID: a.ID, Addr: a.ListenAddr, BinPath: binPath, PGDSN: dsn, Kind: a.Kind}
		r.infos[a.ID] = AgentInfo{ID: a.ID, Name: a.Name, Model: a.Model}
	}
	return r
}

// Get returns the AgentProcess for id.
func (r *Registry) Get(id string) (AgentProcess, bool) {
	ap, ok := r.agents[id]
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
