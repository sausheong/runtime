package controlplane

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/sausheong/runtime/internal/config"
)

// subjectForwardingEnabled reports whether RUNTIME_SUBJECT_FORWARDING is truthy
// (1/true/yes/on, case-insensitive, trimmed). It is a platform-wide edge switch
// read once at registry construction; controlplane has no shared env helper, so
// this small truthy idiom is duplicated locally.
func subjectForwardingEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("RUNTIME_SUBJECT_FORWARDING"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

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
	pools  map[string]*PoolManager   // id -> manager (autoscaled agents only)

	policyResolver PolicyResolver // optional; injected into each AgentProcess on read.

	// mu guards the agent set (order/sets/infos/rr/managed/disabled) for runtime
	// mutation. The file-config set is built once in NewRegistry and was
	// historically read lock-free; dynamically-managed agents (AddRemote/
	// RemoveAgent, called while serving) make concurrent reads+writes possible, so
	// the set readers below take RLock. SetBroker/SetGateway still run before
	// serving starts and are not locked.
	mu       sync.RWMutex
	managed  map[string]bool // id -> registered dynamically (DB-backed), not file config
	disabled map[string]bool // id -> administratively disabled (kept, but out of routing/listing)

	// reachMu guards reach: id → (replicaIndex → reachable). Absent entry ⇒
	// "unknown", treated as reachable until the first health probe reports.
	// Written by main's per-ordinal HealthMonitor OnChange; read by NextReplica.
	reachMu sync.RWMutex
	reach   map[string]map[int]bool
}

// NewRegistry builds a Registry from parsed config. binPath is the agentd
// binary all local agents run; dsn is the shared Postgres DSN. A local agent
// with replicas: N expands to N AgentProcess entries on derived ports; a remote
// agent (url:) is a single attach-only entry.
func NewRegistry(cfg *config.Config, binPath, dsn string) *Registry {
	r := &Registry{
		sets:     map[string][]AgentProcess{},
		infos:    map[string]AgentInfo{},
		rr:       map[string]*atomic.Uint64{},
		pools:    map[string]*PoolManager{},
		reach:    map[string]map[int]bool{},
		managed:  map[string]bool{},
		disabled: map[string]bool{},
	}
	// Platform-wide edge switch, read once at registry construction; stamped on
	// every agent so both local spawn and remote /register carry it explicitly.
	subjectForwarding := subjectForwardingEnabled()
	for _, a := range cfg.Agents {
		r.order = append(r.order, a.ID)
		r.infos[a.ID] = AgentInfo{ID: a.ID, Name: a.Name, Model: a.Model, Tenant: a.Tenant}
		r.rr[a.ID] = &atomic.Uint64{}

		var pricingJSON string
		if mp, ok := cfg.Pricing.PriceFor(a.Model); ok {
			pricingJSON = mp.JSON()
		}
		base := AgentProcess{
			AgentID: a.ID, BinPath: binPath, PGDSN: dsn,
			Kind: a.Kind, Command: a.Command, WorkDir: a.WorkDir, Tenant: a.Tenant,
			Memory: a.Memory, GatewayOn: a.Gateway.Enabled(),
			GatewaySearch:     a.Gateway == config.GatewaySearch,
			LimitsJSON:        a.Limits.JSON(),
			PricingJSON:       pricingJSON,
			SubjectForwarding: subjectForwarding,
		}
		if a.URL != "" {
			n := a.RemotePoolSize()
			set := make([]AgentProcess, n)
			for i := 0; i < n; i++ {
				ou, err := a.RemoteReplicaURL(i)
				if err != nil {
					// Validate() proved these expand; fall back defensively.
					ou = a.URL
				}
				rem := base
				rem.Remote = true
				rem.BaseURL = ou
				rem.AuthToken = a.AuthToken
				rem.ReplicaIndex = i
				set[i] = rem
			}
			r.sets[a.ID] = set
			continue
		}
		// Autoscaled local agent (Spine A2): a PoolManager owns the mutable set;
		// the static slice stays nil (reads delegate). main.go starts it.
		if a.Autoscale != nil {
			ac := a // capture for the addrOf closure
			pm := newPoolManager(a.ID, base, *a.Autoscale,
				func(i int) (string, error) { return ac.ReplicaAddr(i) }, nil, nil)
			r.pools[a.ID] = pm
			r.sets[a.ID] = nil
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
// brokering. It also stamps each PoolManager's base so replicas spawned later by
// autoscaling inherit the broker (secret injection at spawn time).
func (r *Registry) SetBroker(b SecretBroker) {
	r.broker = b
	for _, pm := range r.pools {
		pm.base.broker = b
	}
}

// SetPolicyResolver installs the eval PolicyResolver injected into every
// AgentProcess returned by Get/Replicas/Replica. Like SetBroker: NOT safe to
// call concurrently with reads (must happen-before the HTTP server and
// supervisor goroutines start); nil ⇒ no eval policy. It also stamps each
// PoolManager's base so replicas spawned later by autoscaling inherit it (policy
// resolution at spawn time).
func (r *Registry) SetPolicyResolver(pr PolicyResolver) {
	r.policyResolver = pr
	for _, pm := range r.pools {
		pm.base.policyResolver = pr
	}
}

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
	for _, pm := range r.pools {
		if pm.base.GatewayOn {
			pm.base.GatewayURL = url
			pm.base.GatewayKey = keys[pm.base.Tenant]
		}
	}
}

// AgentTenants returns agentID→tenantID for all registered agents.
func (r *Registry) AgentTenants() map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m := make(map[string]string, len(r.order))
	for _, id := range r.order {
		m[id] = r.infos[id].Tenant
	}
	return m
}

// withBroker returns a copy of ap with the registry's broker AND policy resolver
// attached, so both ride along on what callers get without mutating the stored
// entry. Called by Get/Replicas/Replica, so every returned AgentProcess (including
// PoolManager-backed replicas) carries the resolver and spawns inject the policy.
func (r *Registry) withBroker(ap AgentProcess) AgentProcess {
	ap.broker = r.broker
	ap.policyResolver = r.policyResolver
	return ap
}

// Get returns replica 0 of id (agent-level info: tenant, gateway, broker),
// preserving callers that want "the agent" rather than a specific replica.
func (r *Registry) Get(id string) (AgentProcess, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if pm, ok := r.pools[id]; ok {
		ap, ok := pm.replica0Info()
		if !ok {
			return AgentProcess{}, false
		}
		return r.withBroker(ap), true
	}
	set, ok := r.sets[id]
	if !ok || len(set) == 0 {
		return AgentProcess{}, false
	}
	return r.withBroker(set[0]), true
}

// Replicas returns the ordered replica set for id (broker attached to each).
func (r *Registry) Replicas(id string) ([]AgentProcess, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if pm, ok := r.pools[id]; ok {
		reps := pm.Replicas()
		out := make([]AgentProcess, len(reps))
		for i := range reps {
			out[i] = r.withBroker(reps[i])
		}
		return out, true
	}
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
	r.mu.RLock()
	defer r.mu.RUnlock()
	if pm, ok := r.pools[id]; ok {
		ap, ok := pm.Replica(i)
		if !ok {
			return AgentProcess{}, false
		}
		return r.withBroker(ap), true
	}
	set, ok := r.sets[id]
	if !ok || i < 0 || i >= len(set) {
		return AgentProcess{}, false
	}
	return r.withBroker(set[i]), true
}

// ResetReachable clears all recorded reachability for id, returning it to the
// "unknown" (treated as reachable) state until the next probe reports. Used by
// a monitor restart / re-attach.
func (r *Registry) ResetReachable(id string) {
	r.reachMu.Lock()
	defer r.reachMu.Unlock()
	delete(r.reach, id)
}

// SetReachable records a replica's reachability (called by main's per-ordinal
// HealthMonitor on each transition). Used by NextReplica to skip down ordinals
// of a remote pool. Safe for concurrent use.
func (r *Registry) SetReachable(id string, replica int, reachable bool) {
	r.reachMu.Lock()
	defer r.reachMu.Unlock()
	m := r.reach[id]
	if m == nil {
		m = map[int]bool{}
		r.reach[id] = m
	}
	m[replica] = reachable
}

// reachableOrUnknown reports whether replica i of id may receive new sessions:
// true unless a probe has explicitly marked it unreachable.
func (r *Registry) reachableOrUnknown(id string, i int) bool {
	r.reachMu.RLock()
	defer r.reachMu.RUnlock()
	m := r.reach[id]
	if m == nil {
		return true
	}
	v, ok := m[i]
	if !ok {
		return true // unknown ⇒ reachable until first probe
	}
	return v
}

// NextReplica returns the next replica index for a NEW session, round-robin via
// an atomic per-agent counter, SKIPPING ordinals a health probe has marked
// unreachable. Falls back to 0 if every ordinal is unreachable. Autoscaled
// agents delegate to their PoolManager (which skips draining replicas).
func (r *Registry) NextReplica(id string) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if pm, ok := r.pools[id]; ok {
		return pm.NextReplica()
	}
	set, ok := r.sets[id]
	if !ok || len(set) == 0 {
		return 0
	}
	n := len(set)
	for tries := 0; tries < n; tries++ {
		idx := int((r.rr[id].Add(1) - 1) % uint64(n))
		if r.reachableOrUnknown(id, idx) {
			return idx
		}
	}
	return 0
}

// Pools returns the autoscaled agents' managers, keyed by id. main.go starts each.
func (r *Registry) Pools() map[string]*PoolManager { return r.pools }

// List returns agent infos in registration order, INCLUDING administratively
// disabled ones (callers that present the public agent list — /agents, the
// console overview — filter with Disabled; startup/monitor loops want them all).
func (r *Registry) List() []AgentInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]AgentInfo, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.infos[id])
	}
	return out
}

// AddRemote registers a remote (attach-only) agent at runtime: a single
// replica-0 entry dialing ap.BaseURL. Idempotent on id (a re-add replaces the
// entry). managed marks it DB-backed so deregister can distinguish it from a
// file-config agent. Safe for concurrent use with reads.
func (r *Registry) AddRemote(info AgentInfo, ap AgentProcess, managed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.infos[info.ID]; !exists {
		r.order = append(r.order, info.ID)
	}
	r.infos[info.ID] = info
	if r.rr[info.ID] == nil {
		r.rr[info.ID] = &atomic.Uint64{}
	}
	ap.Remote = true
	ap.ReplicaIndex = 0
	r.sets[info.ID] = []AgentProcess{ap}
	r.managed[info.ID] = managed
	delete(r.disabled, info.ID)
}

// RemoveAgent drops an agent from the registry entirely (all maps + order).
// Idempotent. Used by deregister; the caller stops the agent's health monitor.
func (r *Registry) RemoveAgent(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sets, id)
	delete(r.infos, id)
	delete(r.rr, id)
	delete(r.managed, id)
	delete(r.disabled, id)
	for i, x := range r.order {
		if x == id {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
	r.reachMu.Lock()
	delete(r.reach, id)
	r.reachMu.Unlock()
}

// TenantOf returns the tenant that owns id from the LIVE registry (covers
// dynamically-added agents), ok=false if unknown. Used as the authorizer's live
// lookup so dynamic agents are authorizable without rebuilding it.
func (r *Registry) TenantOf(id string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	info, ok := r.infos[id]
	if !ok {
		return "", false
	}
	return info.Tenant, true
}

// IsManaged reports whether id was registered dynamically (DB-backed) rather
// than from file config. Deregister/enable/disable are managed-only.
func (r *Registry) IsManaged(id string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.managed[id]
}

// SetEnabled administratively enables/disables an agent. A disabled agent stays
// registered and monitored but is dropped from new-session routing and the
// public listing. Safe for concurrent use.
func (r *Registry) SetEnabled(id string, enabled bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if enabled {
		delete(r.disabled, id)
	} else {
		r.disabled[id] = true
	}
}

// Disabled reports whether id is administratively disabled.
func (r *Registry) Disabled(id string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.disabled[id]
}
