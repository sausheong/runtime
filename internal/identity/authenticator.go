package identity

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
)

// ErrUnauthenticated: no valid credential (no/garbled/expired/wrong secret).
// ErrNotProvisioned: a valid OIDC token whose subject has no users row.
var (
	ErrUnauthenticated = errors.New("identity: unauthenticated")
	ErrNotProvisioned  = errors.New("identity: subject not provisioned")
)

// ErrTenantSelectionRequired: a valid OIDC subject that belongs to more than one
// tenant, with no (or a non-member) runtime_tenant selection cookie. The caller
// should send the user to the tenant picker. Authenticate returns a partial
// Principal (Subject + KindOIDC, empty TenantID) alongside this error so the
// picker can identify the user without re-verifying the token.
var ErrTenantSelectionRequired = errors.New("identity: tenant selection required")

// TenantCookieName holds the console's selected tenant. It is a HINT: the
// authenticator only honors it when the value is one of the subject's actual
// memberships, so a forged/stale value cannot grant access.
const TenantCookieName = "runtime_tenant"

// principalSource is the subset of *Store the Authenticator needs (so tests can
// substitute an in-memory fake).
type principalSource interface {
	UsersBySubject(ctx context.Context, subject string) ([]UserRow, error)
	ActiveKeyByID(ctx context.Context, id string) (activeKey, error)
}

// Authenticator resolves an HTTP request to a Principal. It tries, in order: the
// bootstrap superuser key (if configured), a legacy M3 token (deprecated
// compat), a service key (svk- prefix), then an OIDC token (three-segment JWT).
// The OIDC verifier may be nil (OIDC disabled).
type Authenticator struct {
	src          principalSource
	oidc         OIDCVerifier
	bootstrapKey string            // plaintext superuser key from env; "" = disabled
	legacy       map[string]string // deprecated M3 token -> label; each maps to a default-tenant superuser; nil = none
}

// NewAuthenticator builds an Authenticator. oidc may be nil when no issuer is
// configured; bootstrapKey may be "" when none is set; legacy may be nil when
// there are no deprecated M3 tokens to honour.
func NewAuthenticator(src principalSource, oidc OIDCVerifier, bootstrapKey string, legacy map[string]string) *Authenticator {
	return &Authenticator{src: src, oidc: oidc, bootstrapKey: bootstrapKey, legacy: legacy}
}

// Authenticate resolves r to a Principal or returns an error
// (ErrUnauthenticated / ErrNotProvisioned).
func (a *Authenticator) Authenticate(ctx context.Context, r *http.Request) (Principal, error) {
	cred := extractCredential(r)
	if cred == "" {
		return Principal{}, ErrUnauthenticated
	}

	// 1. Bootstrap superuser key (constant-time compare).
	if a.bootstrapKey != "" &&
		subtle.ConstantTimeCompare([]byte(cred), []byte(a.bootstrapKey)) == 1 {
		return Principal{Subject: "bootstrap", Role: RoleAdmin, Superuser: true, Kind: KindBootstrap}, nil
	}

	// 1b. Legacy M3 token (deprecated): maps to a default-tenant superuser so
	// existing deployments keep working after upgrade. Removed in a later
	// milestone once service keys are adopted. The map lookup is intentionally
	// not constant-time: these tokens are an opt-in, deprecated compat shim, and
	// the cost of hardening a feature slated for removal isn't worth it.
	if lbl, ok := a.legacy[cred]; ok {
		return Principal{Subject: "legacy:" + lbl, Role: RoleAdmin, Superuser: true, Kind: KindLegacy}, nil
	}

	// 2. Service key.
	if id, secret, ok := ParseServiceKey(cred); ok {
		k, err := a.src.ActiveKeyByID(ctx, id)
		if err != nil {
			return Principal{}, ErrUnauthenticated // unknown/revoked id
		}
		if !VerifyKey(k.Hash, secret) {
			return Principal{}, ErrUnauthenticated
		}
		return Principal{TenantID: k.TenantID, Subject: id, Role: k.Role, Kind: KindServiceKey}, nil
	}

	// 3. OIDC token.
	if a.oidc != nil && looksLikeJWT(cred) {
		sub, err := a.oidc.Verify(ctx, cred)
		if err != nil {
			return Principal{}, ErrUnauthenticated
		}
		rows, err := a.src.UsersBySubject(ctx, sub)
		if err != nil {
			return Principal{}, err
		}
		switch len(rows) {
		case 0:
			return Principal{}, ErrNotProvisioned
		case 1:
			u := rows[0]
			return Principal{TenantID: u.TenantID, Subject: u.Subject, Role: u.Role, Kind: KindOIDC}, nil
		default:
			// Multi-tenant: honor the selection cookie only if it names a tenant the
			// subject actually belongs to. Otherwise require (re)selection.
			if sel := selectedTenant(r); sel != "" {
				for _, u := range rows {
					if u.TenantID == sel {
						return Principal{TenantID: u.TenantID, Subject: u.Subject, Role: u.Role, Kind: KindOIDC}, nil
					}
				}
			}
			// Partial principal: subject known, tenant pending. The middleware uses it
			// to drive the picker.
			return Principal{Subject: sub, Kind: KindOIDC}, ErrTenantSelectionRequired
		}
	}

	return Principal{}, ErrUnauthenticated
}

// extractCredential pulls a bearer token from Authorization, falling back to the
// runtime_token cookie (EventSource / browser navigations can't set headers).
func extractCredential(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	if c, err := r.Cookie("runtime_token"); err == nil {
		return c.Value
	}
	return ""
}

// selectedTenant returns the runtime_tenant cookie value ("" if absent).
func selectedTenant(r *http.Request) string {
	if c, err := r.Cookie(TenantCookieName); err == nil {
		return c.Value
	}
	return ""
}
