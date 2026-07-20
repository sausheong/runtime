package identity

import (
	"fmt"
	"net/url"
)

// Credential type discriminators stored in secrets.type.
const (
	CredTypeStatic = "static"
	CredTypeOAuth2 = "oauth2_client_credentials"
	CredTypeOBO    = "oauth2_obo" // RFC 8693 token exchange (on-behalf-of the caller)
)

// RFC 8693 token-type URNs (defaults when OBOConfig leaves the fields blank).
const (
	tokenTypeJWT         = "urn:ietf:params:oauth:token-type:jwt"
	tokenTypeAccessToken = "urn:ietf:params:oauth:token-type:access_token"
)

// OAuth2Config is the client_credentials grant configuration for an outbound
// gateway credential. The whole struct is sealed under the keyring as JSON in
// the secret's value_enc; ClientSecret therefore never leaves the broker.
type OAuth2Config struct {
	TokenURL       string            `json:"token_url"`
	ClientID       string            `json:"client_id"`
	ClientSecret   string            `json:"client_secret"`
	Scopes         []string          `json:"scopes,omitempty"`
	Audience       string            `json:"audience,omitempty"`
	EndpointParams map[string]string `json:"endpoint_params,omitempty"`
}

// Validate rejects an incomplete or malformed config at creation time.
func (c OAuth2Config) Validate() error {
	if c.TokenURL == "" {
		return fmt.Errorf("oauth2: token_url is required")
	}
	u, err := url.Parse(c.TokenURL)
	if err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("oauth2: token_url must be an absolute http(s) URL")
	}
	if c.ClientID == "" {
		return fmt.Errorf("oauth2: client_id is required")
	}
	if c.ClientSecret == "" {
		return fmt.Errorf("oauth2: client_secret is required")
	}
	return nil
}

// OAuth2Meta is the non-secret read model for a listing. ClientSecret is
// deliberately absent — it must never be surfaced through any read path.
type OAuth2Meta struct {
	TokenURL string   `json:"token_url"`
	ClientID string   `json:"client_id"`
	Scopes   []string `json:"scopes,omitempty"`
	Audience string   `json:"audience,omitempty"`
}

// OBOConfig is a token-exchange (RFC 8693) outbound credential: the gateway
// swaps the caller's JWT (subject_token) for a downstream token at the tenant's
// IdP. ClientID/ClientSecret authenticate the gateway AS the exchange client to
// the IdP; Audience/Scopes scope the downstream token. Sealed under the keyring;
// ClientSecret never leaves the broker.
type OBOConfig struct {
	TokenURL           string            `json:"token_url"`
	ClientID           string            `json:"client_id"`
	ClientSecret       string            `json:"client_secret"`
	Scopes             []string          `json:"scopes,omitempty"`
	Audience           string            `json:"audience,omitempty"`
	EndpointParams     map[string]string `json:"endpoint_params,omitempty"`
	SubjectTokenType   string            `json:"subject_token_type,omitempty"`   // default tokenTypeJWT
	RequestedTokenType string            `json:"requested_token_type,omitempty"` // default tokenTypeAccessToken
}

// SubjectTokenTypeOrDefault / RequestedTokenTypeOrDefault return the configured
// value or the RFC 8693 default. Used by the exchange POST.
func (c OBOConfig) SubjectTokenTypeOrDefault() string {
	if c.SubjectTokenType != "" {
		return c.SubjectTokenType
	}
	return tokenTypeJWT
}

func (c OBOConfig) RequestedTokenTypeOrDefault() string {
	if c.RequestedTokenType != "" {
		return c.RequestedTokenType
	}
	return tokenTypeAccessToken
}

// Validate mirrors OAuth2Config.Validate: absolute http/https TokenURL, non-empty
// ClientID + ClientSecret. Token-type fields are optional (defaulted at exchange).
func (c OBOConfig) Validate() error {
	if c.TokenURL == "" {
		return fmt.Errorf("obo: token_url is required")
	}
	u, err := url.Parse(c.TokenURL)
	if err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("obo: token_url must be an absolute http(s) URL")
	}
	if c.ClientID == "" {
		return fmt.Errorf("obo: client_id is required")
	}
	if c.ClientSecret == "" {
		return fmt.Errorf("obo: client_secret is required")
	}
	return nil
}

// OBOMeta is the write-only read model for listing (no ClientSecret).
type OBOMeta struct {
	TokenURL           string   `json:"token_url"`
	ClientID           string   `json:"client_id"`
	Scopes             []string `json:"scopes,omitempty"`
	Audience           string   `json:"audience,omitempty"`
	SubjectTokenType   string   `json:"subject_token_type,omitempty"`
	RequestedTokenType string   `json:"requested_token_type,omitempty"`
}
