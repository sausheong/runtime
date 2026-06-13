package controlplane

import (
	"strconv"
	"sync/atomic"

	"github.com/sausheong/runtime/internal/config"
)

// AgentInfo is the public description of a registered agent.
type AgentInfo struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Model  string `json:"model"`
	Tenant string `json:"tenant"`
}

// Registry holds the agents the control plane hosts, built from config. Each
// agent maps to an ordered replica set (len 1 for single-replica and remote
// agents). Read-only after construction except the optional secret broker and
// the gateway stamp (SetGateway); both must complete before serving starts.
type Registry struct {
	order  []string
	sets   map[string][]AgentProcess // id -> ordered replica set
	infos  map[string]AgentInfo
	rr     map[string]*atomic.Uint64 // id -> round-robin counter (new-session routing)
	broker SecretBroker              // optional; injected into each AgentProcess on read.
}

// NewRegistry builds a Registry from parsed config. binPath is the agentd
// binary all local agents run; dsn is the shared Postgres DSN. A local agent
// with replicas: N expands to N AgentProcess entries on derived ports; a remote
// agent (url:) is a single attach-only entry.
func NewRegistry(cfg *config.Config, binPath, dsn string) *Registry {
	r := &Registry{
		sets:  map[string][]AgentProcess{},
		infos: map[string]AgentInfo{},
		rr:    map[string]*atomic.Uint64{},
	}
	for _, a := range cfg.Agents {
		r.order = append(r.order, a.ID)
		r.infos[a.ID] = AgentInfo{ID: a.ID, Name: a.Name, Model: a.Model, Tenant: a.Tenant}
		r.rr[a.ID] = &atomic.Uint64{}

		base := AgentProcess{
			AgentID: a.ID, BinPath: binPath, PGDSN: dsn,
			Kind: a.Kind, Command: a.Command, WorkDir: a.WorkDir, Tenant: a.Tenant,
			Memory: a.Memory, GatewayOn: a.Gateway.Enabled(),
			GatewaySearch: a.Gateway == config.GatewaySearch,
		}
		if a.URL != "" {
			rem := base
			rem.Remote = true
			rem.BaseURL = a.URL
			rem.AuthToken = a.AuthToken
			rem.ReplicaIndex = 0
			r.sets[a.ID] = []AgentProcess{rem}
			continue
		}
		// Local: expand to the derived replica addresses. Validate() has already
		// proven these parse and don't collide, so the error is unreachable; we
		// fall back to the single base addr defensively if it ever fires.
		addrs, err := a.ReplicaAddrs()
		if err != nil {
			addrs = []string{a.ListenAddr}
		}
		set := make([]AgentProcess, len(addrs))
		for i, addr := range addrs {
			ap := base
			ap.ReplicaIndex = i
			ap.Addr = addr
			ap.BaseURL = "http://" + addr
			ap.DBOSVMID = a.ID + "#" + strconv.Itoa(i)
			set[i] = ap
		}
		r.sets[a.ID] = set
	}
	return r
}

// SetBroker installs the secret broker injected into every AgentProcess returned
// by Get/Replicas/Replica. NOT safe to call concurrently with reads: it must
// happen-before the HTTP server and supervisor goroutines start. nil ⇒ no
// brokering.
func (r *Registry) SetBroker(b SecretBroker) { r.broker = b }

// SetGateway records the gateway endpoint URL and per-tenant agent keys, stamped
// onto every gateway-enabled replica. Like SetBroker, must complete before the
// server and supervisor goroutines start.
func (r *Registry) SetGateway(url string, keys map[string]string) {
	for id, set := range r.sets {
		for i := range set {
			if !set[i].GatewayOn {
				continue
			}
			set[i].GatewayURL = url
			set[i].GatewayKey = keys[set[i].Tenant]
		}
		r.sets[id] = set
	}
}

// AgentTenants returns agentID→tenantID for all registered agents.
func (r *Registry) AgentTenants() map[string]string {
	m := make(map[string]string, len(r.order))
	for _, id := range r.order {
		m[id] = r.infos[id].Tenant
	}
	return m
}

// withBroker returns a copy of ap with the registry's broker attached, so the
// broker rides along on what callers get without mutating the stored entry.
func (r *Registry) withBroker(ap AgentProcess) AgentProcess {
	ap.broker = r.broker
	return ap
}

// Get returns replica 0 of id (agent-level info: tenant, gateway, broker),
// preserving callers that want "the agent" rather than a specific replica.
func (r *Registry) Get(id string) (AgentProcess, bool) {
	set, ok := r.sets[id]
	if !ok || len(set) == 0 {
		return AgentProcess{}, false
	}
	return r.withBroker(set[0]), true
}

// Replicas returns the ordered replica set for id (broker attached to each).
func (r *Registry) Replicas(id string) ([]AgentProcess, bool) {
	set, ok := r.sets[id]
	if !ok {
		return nil, false
	}
	out := make([]AgentProcess, len(set))
	for i := range set {
		out[i] = r.withBroker(set[i])
	}
	return out, true
}

// Replica returns one replica by index (broker attached). false if id unknown
// or i out of range.
func (r *Registry) Replica(id string, i int) (AgentProcess, bool) {
	set, ok := r.sets[id]
	if !ok || i < 0 || i >= len(set) {
		return AgentProcess{}, false
	}
	return r.withBroker(set[i]), true
}

// NextReplica returns the next replica index for a NEW session, round-robin via
// an atomic per-agent counter. Blind to liveness. Returns 0 for unknown ids.
func (r *Registry) NextReplica(id string) int {
	set, ok := r.sets[id]
	if !ok || len(set) == 0 {
		return 0
	}
	n := r.rr[id].Add(1) - 1
	return int(n % uint64(len(set)))
}

// List returns agent infos in config order.
func (r *Registry) List() []AgentInfo {
	out := make([]AgentInfo, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.infos[id])
	}
	return out
}
