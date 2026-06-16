// Package identity provides multi-tenant authentication and authorization for
// the control-plane edge. A Principal is the resolved caller; the Authorizer
// decides whether a Principal may take an Action on an agent.
package identity

import "fmt"

// Role is a fixed access level. M1 has exactly three; no custom roles.
type Role string

const (
	RoleViewer   Role = "viewer"   // read-only: list/get/stream sessions and agents
	RoleOperator Role = "operator" // viewer + invoke (POST /sessions)
	RoleAdmin    Role = "admin"    // operator + manage identity within own tenant
)

// RoleFromString validates and converts s to a Role.
func RoleFromString(s string) (Role, error) {
	switch Role(s) {
	case RoleViewer, RoleOperator, RoleAdmin:
		return Role(s), nil
	default:
		return "", fmt.Errorf("identity: invalid role %q (want viewer|operator|admin)", s)
	}
}

// CredentialKind records how a Principal authenticated, so policy can treat
// human (OIDC) sessions differently from machine/break-glass credentials — e.g.
// the console may require an OIDC session and refuse a service-key cookie.
type CredentialKind string

const (
	KindNone       CredentialKind = ""            // unauthenticated / open mode
	KindOIDC       CredentialKind = "oidc"        // human via the IdP (Google)
	KindServiceKey CredentialKind = "service_key" // svk- machine credential
	KindBootstrap  CredentialKind = "bootstrap"   // RUNTIME_ADMIN_BOOTSTRAP superuser
	KindLegacy     CredentialKind = "legacy"      // deprecated M3 token
)

// Action is a coarse capability derived from the HTTP method+path at the edge.
type Action string

const (
	ActionRead   Action = "read"   // GET /agents, GET sessions, stream
	ActionInvoke Action = "invoke" // POST /sessions
	ActionAdmin  Action = "admin"  // manage tenants/users/keys
)

// Principal is an authenticated caller. Subject is an OIDC `sub` claim (humans)
// or a service-key id like "svk-..." (machines).
type Principal struct {
	TenantID string
	Subject  string
	Role     Role
	// Superuser is true only for the bootstrap key: it may create tenants and
	// act across tenants. Never set for DB-backed principals.
	Superuser bool
	// Kind is how this Principal authenticated. Set by the Authenticator; used by
	// edge policy (e.g. console requires KindOIDC).
	Kind CredentialKind
}
