package identity

import "errors"

// ErrForbidden: authenticated but the role does not permit the action.
// ErrNotFound: agent does not exist OR is in another tenant (existence hidden).
var (
	ErrForbidden = errors.New("identity: forbidden")
	ErrNotFound  = errors.New("identity: not found")
)

// Authorizer decides Principal × agent × Action. It holds the static
// agentID→tenantID map resolved from runtime.yaml at startup.
type Authorizer struct {
	agentTenant map[string]string
}

// NewAuthorizer builds an Authorizer from the agentID→tenantID map.
func NewAuthorizer(agentTenant map[string]string) *Authorizer {
	m := make(map[string]string, len(agentTenant))
	for k, v := range agentTenant {
		m[k] = v
	}
	return &Authorizer{agentTenant: m}
}

// AgentTenant returns the tenant for an agent, or "" if unknown.
func (a *Authorizer) AgentTenant(agentID string) string { return a.agentTenant[agentID] }

// CanSeeAgent reports whether p's tenant owns agentID (superuser sees all). A
// non-superuser with an empty TenantID is always denied, so a misconfigured
// empty tenant can never match.
func (a *Authorizer) CanSeeAgent(p Principal, agentID string) bool {
	t, ok := a.agentTenant[agentID]
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
