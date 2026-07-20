package identity

import "testing"

func TestOAuth2ConfigValidate(t *testing.T) {
	ok := OAuth2Config{TokenURL: "https://idp/token", ClientID: "c", ClientSecret: "s"}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	bad := []OAuth2Config{
		{ClientID: "c", ClientSecret: "s"},                      // no token_url
		{TokenURL: "notaurl", ClientID: "c", ClientSecret: "s"}, // bad url
		{TokenURL: "https://idp/token", ClientSecret: "s"},      // no client_id
		{TokenURL: "https://idp/token", ClientID: "c"},          // no client_secret
	}
	for i, c := range bad {
		if err := c.Validate(); err == nil {
			t.Errorf("case %d: expected error, got nil", i)
		}
	}
}

func TestOBOConfigValidate(t *testing.T) {
	ok := OBOConfig{TokenURL: "https://idp/token", ClientID: "c", ClientSecret: "s"}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	bad := []OBOConfig{
		{ClientID: "c", ClientSecret: "s"},                              // no token_url
		{TokenURL: "notaurl", ClientID: "c", ClientSecret: "s"},         // bad url
		{TokenURL: "ftp://idp/token", ClientID: "c", ClientSecret: "s"}, // non-http(s) scheme
		{TokenURL: "https://idp/token", ClientSecret: "s"},              // no client_id
		{TokenURL: "https://idp/token", ClientID: "c"},                  // no client_secret
	}
	for i, c := range bad {
		if err := c.Validate(); err == nil {
			t.Errorf("case %d: expected error, got nil", i)
		}
	}
}

func TestOBOConfigTokenTypeDefaults(t *testing.T) {
	// Blank fields fall back to the RFC 8693 defaults.
	blank := OBOConfig{}
	if got := blank.SubjectTokenTypeOrDefault(); got != tokenTypeJWT {
		t.Errorf("SubjectTokenTypeOrDefault blank = %q, want %q", got, tokenTypeJWT)
	}
	if got := blank.RequestedTokenTypeOrDefault(); got != tokenTypeAccessToken {
		t.Errorf("RequestedTokenTypeOrDefault blank = %q, want %q", got, tokenTypeAccessToken)
	}
	// Set values are returned verbatim.
	set := OBOConfig{
		SubjectTokenType:   "urn:custom:subject",
		RequestedTokenType: "urn:custom:requested",
	}
	if got := set.SubjectTokenTypeOrDefault(); got != "urn:custom:subject" {
		t.Errorf("SubjectTokenTypeOrDefault set = %q, want %q", got, "urn:custom:subject")
	}
	if got := set.RequestedTokenTypeOrDefault(); got != "urn:custom:requested" {
		t.Errorf("RequestedTokenTypeOrDefault set = %q, want %q", got, "urn:custom:requested")
	}
}
