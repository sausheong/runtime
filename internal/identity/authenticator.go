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

// principalSource is the subset of *Store the Authenticator needs (so tests can
// substitute an in-memory fake).
type principalSource interface {
	UserBySubject(ctx context.Context, subject string) (UserRow, error)
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
		return Principal{Subject: "bootstrap", Role: RoleAdmin, Superuser: true}, nil
	}

	// 1b. Legacy M3 token (deprecated): maps to a default-tenant superuser so
	// existing deployments keep working after upgrade. Removed in a later
	// milestone once service keys are adopted.
	if lbl, ok := a.legacy[cred]; ok {
		return Principal{Subject: "legacy:" + lbl, Role: RoleAdmin, Superuser: true}, nil
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
		return Principal{TenantID: k.TenantID, Subject: id, Role: k.Role}, nil
	}

	// 3. OIDC token.
	if a.oidc != nil && looksLikeJWT(cred) {
		sub, err := a.oidc.Verify(ctx, cred)
		if err != nil {
			return Principal{}, ErrUnauthenticated
		}
		u, err := a.src.UserBySubject(ctx, sub)
		if errors.Is(err, ErrNoUser) {
			return Principal{}, ErrNotProvisioned
		}
		if err != nil {
			return Principal{}, err
		}
		return Principal{TenantID: u.TenantID, Subject: u.Subject, Role: u.Role}, nil
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
