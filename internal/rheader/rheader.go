// Package rheader defines the canonical platform identity headers projected
// onto internal hops (control plane → agentd, gateway enrich). These name the
// authenticated caller's claims; the X-Runtime- prefix is reserved for
// platform-set values, and callers' inbound X-Runtime-* headers are stripped
// before these are set (anti-spoof).
package rheader

const (
	// Prefix marks platform-set claim headers. Any inbound header with this
	// canonical prefix is caller-supplied and must be stripped before the
	// platform sets its own.
	Prefix = "X-Runtime-"

	User   = "X-Runtime-User"   // identity.Principal.Subject
	Tenant = "X-Runtime-Tenant" // identity.Principal.TenantID
	Role   = "X-Runtime-Role"   // identity.Principal.Role

	Assertion = "X-Runtime-Assertion" // caller's raw verified OIDC JWT (OBO subject_token); bearer secret, never logged

	Session = "X-Runtime-Session" // agent session id: isolation bucket for session-scoped sandbox/browser tools; NOT a secret
)
