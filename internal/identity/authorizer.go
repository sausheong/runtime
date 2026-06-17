package identity

import "errors"

// ErrForbidden: authenticated but the role does not permit the action.
// ErrNotFound: agent does not exist OR is in another tenant (existence hidden).
var (
	ErrForbidden = errors.New("identity: forbidden")
	ErrNotFound  = errors.New("identity: not found")
)

// Authorizer decides Principal × agent × Action. It resolves an agent's tenant
// from the static map built at startup (runtime.yaml) and, if set, a live lookup
// that also covers agents registered dynamically after startup.
type Authorizer struct {
	agentTenant map[string]string
	live        func(agentID string) (string, bool) // optional; consulted when the static map misses
}

// NewAuthorizer builds an Authorizer from the agentID→tenantID map.
func NewAuthorizer(agentTenant map[string]string) *Authorizer {
	m := make(map[string]string, len(agentTenant))
	for k, v := range agentTenant {
		m[k] = v
	}
	return &Authorizer{agentTenant: m}
}

// WithLiveLookup installs a resolver consulted when the static map does not know
// an agent — so dynamically-registered agents (added after startup) are
// authorizable without rebuilding the Authorizer. Returns a (tenant, ok) pair.
func (a *Authorizer) WithLiveLookup(f func(agentID string) (string, bool)) *Authorizer {
	a.live = f
	return a
}

// tenantOf resolves an agent's tenant from the static map, falling back to the
// live lookup (dynamic agents). ok=false ⇒ the agent is unknown.
func (a *Authorizer) tenantOf(agentID string) (string, bool) {
	if t, ok := a.agentTenant[agentID]; ok {
		return t, true
	}
	if a.live != nil {
		return a.live(agentID)
	}
	return "", false
}

// AgentTenant returns the tenant for an agent, or "" if unknown.
func (a *Authorizer) AgentTenant(agentID string) string {
	t, _ := a.tenantOf(agentID)
	return t
}

// CanSeeAgent reports whether p's tenant owns agentID (superuser sees all). A
// non-superuser with an empty TenantID is always denied, so a misconfigured
// empty tenant can never match.
func (a *Authorizer) CanSeeAgent(p Principal, agentID string) bool {
	t, ok := a.tenantOf(agentID)
	if !ok {
		return false
	}
	if p.Superuser {
		return true
	}
	return p.TenantID != "" && t == p.TenantID
}

// Authorize returns nil if p may take action on agentID. It returns ErrNotFound
// when the agent is unknown or in another tenant (so a tenant cannot learn that
// another tenant's agents exist), and ErrForbidden when the role is too low.
func (a *Authorizer) Authorize(p Principal, agentID string, action Action) error {
	if !a.CanSeeAgent(p, agentID) {
		return ErrNotFound
	}
	if roleAllows(p.Role, action) {
		return nil
	}
	return ErrForbidden
}

// roleAllows is the fixed role×action matrix.
func roleAllows(r Role, action Action) bool {
	switch action {
	case ActionRead:
		return r == RoleViewer || r == RoleOperator || r == RoleAdmin
	case ActionInvoke:
		return r == RoleOperator || r == RoleAdmin
	case ActionAdmin:
		return r == RoleAdmin
	default:
		return false
	}
}
