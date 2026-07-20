package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/sausheong/runtime/internal/identity"
)

const oboGrantType = "urn:ietf:params:oauth:grant-type:token-exchange"

// OBOSource is the broker slice OBOManager needs (interface for testability).
type OBOSource interface {
	OBOConfigFor(ctx context.Context, tenant, name string) (identity.OBOConfig, error)
	CredType(ctx context.Context, tenant, name string) (string, error)
	Generation() uint64
}

// OBOManager mints + caches on-behalf-of tokens via RFC 8693 token exchange.
// Unlike OAuth2Manager (per-tenant client_credentials), OBO tokens are
// PER-CALLER: the cache key includes the caller subject, and the caller's JWT is
// the subject_token. A cached ReuseTokenSource handles TTL refresh; a broker
// generation bump rebuilds with fresh config. Safe for concurrent use.
type OBOManager struct {
	base   context.Context
	src    OBOSource
	client *http.Client

	mu    sync.Mutex
	cache map[string]cachedSource // key: tenant + "\x00" + name + "\x00" + subject
}

func NewOBOManager(base context.Context, src OBOSource) *OBOManager {
	return &OBOManager{
		base:   base,
		src:    src,
		client: &http.Client{Timeout: 30 * time.Second},
		cache:  map[string]cachedSource{},
	}
}

// IsOBO reports whether (tenant, name) is an OBO credential. A lookup error is
// treated as "not OBO" (caller follows another path).
func (m *OBOManager) IsOBO(ctx context.Context, tenant, name string) bool {
	ct, err := m.src.CredType(ctx, tenant, name)
	return err == nil && ct == identity.CredTypeOBO
}

// Bearer returns "Bearer <obo-token>" for an OBO credential. Tri-state, mirroring
// OAuth2Manager.Bearer, plus OBO specifics:
//   - CredType lookup error ⇒ ("", false, err)  → caller fails CLOSED.
//   - ct != CredTypeOBO     ⇒ ("", false, nil)   → not an OBO cred.
//   - jwt == "" (no caller assertion) ⇒ ("", true, err) → fail CLOSED (an OBO
//     upstream must never dispatch without the caller's token).
//   - mint failure          ⇒ ("", true, err)   → fail CLOSED.
func (m *OBOManager) Bearer(ctx context.Context, tenant, name, subject, jwt string) (string, bool, error) {
	ct, err := m.src.CredType(ctx, tenant, name)
	if err != nil {
		return "", false, err
	}
	if ct != identity.CredTypeOBO {
		return "", false, nil
	}
	if jwt == "" {
		return "", true, fmt.Errorf("obo: no caller assertion for credential %q", name)
	}
	ts, err := m.sourceFor(ctx, tenant, name, subject, jwt)
	if err != nil {
		return "", true, err
	}
	tok, err := ts.Token()
	if err != nil {
		return "", true, err
	}
	return "Bearer " + tok.AccessToken, true, nil
}

func (m *OBOManager) sourceFor(ctx context.Context, tenant, name, subject, jwt string) (oauth2.TokenSource, error) {
	gen := m.src.Generation()
	key := tenant + "\x00" + name + "\x00" + subject
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.cache[key]; ok && c.gen == gen {
		return c.ts, nil
	}
	cfg, err := m.src.OBOConfigFor(ctx, tenant, name)
	if err != nil {
		return nil, err
	}
	ts := oauth2.ReuseTokenSource(nil, &oboTokenSource{cfg: cfg, jwt: jwt, client: m.client})
	m.cache[key] = cachedSource{gen: gen, ts: ts}
	return ts, nil
}

// oboTokenSource performs the RFC 8693 exchange on demand (ReuseTokenSource calls
// Token() when the cached token is absent/expired).
type oboTokenSource struct {
	cfg    identity.OBOConfig
	jwt    string
	client *http.Client
}

func (s *oboTokenSource) Token() (*oauth2.Token, error) {
	form := url.Values{}
	form.Set("grant_type", oboGrantType)
	form.Set("subject_token", s.jwt)
	form.Set("subject_token_type", s.cfg.SubjectTokenTypeOrDefault())
	form.Set("requested_token_type", s.cfg.RequestedTokenTypeOrDefault())
	if s.cfg.Audience != "" {
		form.Set("audience", s.cfg.Audience)
	}
	if len(s.cfg.Scopes) > 0 {
		form.Set("scope", strings.Join(s.cfg.Scopes, " "))
	}
	for k, v := range s.cfg.EndpointParams {
		form.Set(k, v)
	}
	tok, err := s.post(form, true) // try HTTP Basic client auth first
	if err == nil {
		return tok, nil
	}
	// Autodetect fallback: some IdPs want client creds in the form body.
	form.Set("client_id", s.cfg.ClientID)
	form.Set("client_secret", s.cfg.ClientSecret)
	return s.post(form, false)
}

func (s *oboTokenSource) post(form url.Values, basic bool) (*oauth2.Token, error) {
	req, err := http.NewRequest(http.MethodPost, s.cfg.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if basic {
		req.SetBasicAuth(url.QueryEscape(s.cfg.ClientID), url.QueryEscape(s.cfg.ClientSecret))
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("obo: token exchange %s returned %d", s.cfg.TokenURL, resp.StatusCode)
	}
	var out struct {
		AccessToken     string `json:"access_token"`
		TokenType       string `json:"token_type"`
		ExpiresIn       int64  `json:"expires_in"`
		IssuedTokenType string `json:"issued_token_type"`
	}
	if err := json.Unmarshal(bodyBytes, &out); err != nil {
		return nil, fmt.Errorf("obo: decode token exchange response: %w", err)
	}
	if out.AccessToken == "" {
		return nil, fmt.Errorf("obo: token exchange returned no access_token")
	}
	tok := &oauth2.Token{AccessToken: out.AccessToken, TokenType: "Bearer"}
	if out.ExpiresIn > 0 {
		tok.Expiry = time.Now().Add(time.Duration(out.ExpiresIn) * time.Second)
	}
	return tok, nil
}
