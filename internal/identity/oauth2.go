package identity

import (
	"fmt"
	"net/url"
)

// Credential type discriminators stored in secrets.type.
const (
	CredTypeStatic = "static"
	CredTypeOAuth2 = "oauth2_client_credentials"
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
