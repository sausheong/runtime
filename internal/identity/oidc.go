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

func (c *coreOIDCVerifier) Verify(ctx context.Context, raw string) (string, error) {
	tok, err := c.v.Verify(ctx, raw)
	if err != nil {
		return "", err
	}
	return tok.Subject, nil
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
