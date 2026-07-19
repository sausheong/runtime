package gateway

import (
	"context"
	"net/url"
	"sync"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"

	"github.com/sausheong/runtime/internal/identity"
)

// OAuth2Source is the broker slice the credential manager needs. Declared as an
// interface so the manager is unit-testable without a real broker/keyring.
type OAuth2Source interface {
	OAuth2ConfigFor(ctx context.Context, tenant, name string) (identity.OAuth2Config, error)
	CredType(ctx context.Context, tenant, name string) (string, error)
	Generation() uint64
}

// OAuth2Manager mints and caches client_credentials access tokens for outbound
// gateway credentials. A cached TokenSource (x/oauth2) handles TTL refresh; a
// broker generation bump (rotation) rebuilds the source with fresh config. It
// is safe for concurrent use.
type OAuth2Manager struct {
	base context.Context // lifetime ctx for token-fetch HTTP requests
	src  OAuth2Source

	mu    sync.Mutex
	cache map[string]cachedSource // key: tenant + "\x00" + name
}

type cachedSource struct {
	gen uint64
	ts  oauth2.TokenSource
}

// NewOAuth2Manager builds a manager. base bounds the lifetime of token-fetch
// HTTP requests (use the process/gateway context, not a per-call ctx).
func NewOAuth2Manager(base context.Context, src OAuth2Source) *OAuth2Manager {
	return &OAuth2Manager{base: base, src: src, cache: map[string]cachedSource{}}
}

// IsOAuth2 reports whether (tenant, name) is an oauth2 credential. A lookup
// error is treated as "not oauth2" (the caller then follows the static path).
func (m *OAuth2Manager) IsOAuth2(ctx context.Context, tenant, name string) bool {
	ct, err := m.src.CredType(ctx, tenant, name)
	return err == nil && ct == identity.CredTypeOAuth2
}

// Bearer returns the "Bearer <token>" header value for an oauth2 credential.
//   - applies=false, err=nil ⇒ (tenant, name) is a static (non-oauth2) cred;
//     the caller should use the static path (header baked at dial).
//   - applies=false, err!=nil ⇒ the cred could not be classified (CredType
//     lookup errored, e.g. deleted mid-session or a transient DB error): the
//     caller MUST fail closed regardless of applies.
//   - applies=true, err!=nil ⇒ oauth2 cred but minting failed: the caller MUST
//     fail closed (reject the tool call, never send without the credential).
func (m *OAuth2Manager) Bearer(ctx context.Context, tenant, name string) (string, bool, error) {
	ct, err := m.src.CredType(ctx, tenant, name)
	if err != nil {
		// Cred lookup failed: the credential is unclassifiable. Surface the
		// error so the caller fails CLOSED (gate #5 rejects the call). Accepted
		// trade-off: a static cred deleted mid-session, or a transient DB error,
		// also fails closed for this upstream even though a static header was
		// baked at dial — correct posture for a security control; the collateral
		// is a rare operator action / transient blip.
		return "", false, err
	}
	if ct != identity.CredTypeOAuth2 {
		return "", false, nil
	}
	ts, err := m.sourceFor(ctx, tenant, name)
	if err != nil {
		return "", true, err
	}
	tok, err := ts.Token()
	if err != nil {
		return "", true, err
	}
	return "Bearer " + tok.AccessToken, true, nil
}

// sourceFor returns a cached TokenSource for (tenant, name), rebuilding it when
// the broker generation has advanced since it was built (live rotation).
func (m *OAuth2Manager) sourceFor(ctx context.Context, tenant, name string) (oauth2.TokenSource, error) {
	gen := m.src.Generation()
	key := tenant + "\x00" + name
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.cache[key]; ok && c.gen == gen {
		return c.ts, nil
	}
	cfg, err := m.src.OAuth2ConfigFor(ctx, tenant, name)
	if err != nil {
		return nil, err
	}
	ep := url.Values{}
	for k, v := range cfg.EndpointParams {
		ep.Set(k, v)
	}
	if cfg.Audience != "" {
		ep.Set("audience", cfg.Audience)
	}
	cc := &clientcredentials.Config{
		ClientID:       cfg.ClientID,
		ClientSecret:   cfg.ClientSecret,
		TokenURL:       cfg.TokenURL,
		Scopes:         cfg.Scopes,
		EndpointParams: ep,
		AuthStyle:      oauth2.AuthStyleAutoDetect,
	}
	ts := cc.TokenSource(m.base) // ReuseTokenSource: caches + refreshes on TTL
	m.cache[key] = cachedSource{gen: gen, ts: ts}
	return ts, nil
}

type credHeaderCtxKey struct{}

type credHeader struct{ header, value string }

// WithCredentialHeader attaches the resolved oauth2 credential header to ctx
// (read by the REST adapter's Execute). Empty header ⇒ ctx unchanged.
func WithCredentialHeader(ctx context.Context, header, value string) context.Context {
	if header == "" {
		return ctx
	}
	return context.WithValue(ctx, credHeaderCtxKey{}, credHeader{header, value})
}

// CredentialHeaderFrom returns the oauth2 credential header on ctx, or ("","").
func CredentialHeaderFrom(ctx context.Context) (string, string) {
	h, _ := ctx.Value(credHeaderCtxKey{}).(credHeader)
	return h.header, h.value
}
