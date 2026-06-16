package identity

import (
	"context"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
)

// OIDCVerifier validates a raw ID token and returns its subject claim.
// Abstracts go-oidc so the authenticator and tests don't need a live IdP.
type OIDCVerifier interface {
	Verify(ctx context.Context, rawIDToken string) (subject string, err error)
}

// coreOIDCVerifier is the production implementation backed by go-oidc's JWKS
// verification against a discovered issuer.
type coreOIDCVerifier struct {
	v *oidc.IDTokenVerifier
}

// NewOIDCVerifier discovers issuerURL and returns a verifier bound to clientID
// (the expected audience). Returns a nil verifier when issuerURL is empty (OIDC
// disabled). The returned verifier is safe for concurrent use.
func NewOIDCVerifier(ctx context.Context, issuerURL, clientID string) (OIDCVerifier, error) {
	if issuerURL == "" {
		return nil, nil
	}
	provider, err := oidc.NewProvider(ctx, issuerURL)
	if err != nil {
		return nil, err
	}
	return &coreOIDCVerifier{v: provider.Verifier(&oidc.Config{ClientID: clientID})}, nil
}

// emailClaims is the subset of ID-token claims we resolve a Principal subject
// from. We prefer a verified email over the opaque `sub` so the onboarding
// allow-list can be keyed by human-readable email (e.g. admin@acme.com) — for
// providers like Google, `sub` is an opaque numeric ID that no admin would ever
// type into the Users form.
type emailClaims struct {
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
}

// resolveSubject decides the Principal subject from a token's `sub` and its
// email claims. It returns the email ONLY when the provider asserts it is
// verified — an unverified (or absent) email falls back to `sub`, so a provider
// that lets users set an arbitrary unverified address can never impersonate
// another user's email-keyed grant. Matching against the onboarding rows is
// case-sensitive; both Google's email claim and the existing rows are lowercase.
func resolveSubject(sub string, c emailClaims) string {
	if c.Email != "" && c.EmailVerified {
		return c.Email
	}
	return sub
}

func (c *coreOIDCVerifier) Verify(ctx context.Context, raw string) (string, error) {
	tok, err := c.v.Verify(ctx, raw)
	if err != nil {
		return "", err
	}
	var claims emailClaims
	// A token that verified cryptographically but has no/garbled claims body is
	// not fatal: fall back to the always-present `sub`.
	_ = tok.Claims(&claims)
	return resolveSubject(tok.Subject, claims), nil
}

// looksLikeJWT is the cheap discriminator the authenticator uses to decide
// whether a non-service-key credential should be tried as an OIDC token: a JWT
// is three non-empty base64url segments separated by dots.
func looksLikeJWT(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
	}
	return true
}
