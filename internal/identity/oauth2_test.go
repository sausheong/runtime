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
