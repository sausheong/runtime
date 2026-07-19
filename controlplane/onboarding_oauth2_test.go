package controlplane

import (
	"context"
	"strings"
	"testing"

	"github.com/sausheong/runtime/internal/identity"
)

func TestRegisterUpstreamRejectsOAuth2OnNonOpenAPI(t *testing.T) {
	credType := func(_ context.Context, _, name string) (string, error) {
		if name == "orders_oauth" {
			return identity.CredTypeOAuth2, nil
		}
		return identity.CredTypeStatic, nil
	}
	// url: (http/MCP) upstream with an oauth2 cred → rejected.
	err := checkOAuth2Openapi(context.Background(), credType, "acme",
		UpstreamParams{Name: "o", URL: "http://x", CredSecret: "orders_oauth", CredHeader: "Authorization"})
	if err == nil || !strings.Contains(err.Error(), "openapi") {
		t.Fatalf("url upstream with oauth2 cred should be rejected, got %v", err)
	}
	// openapi upstream with the same oauth2 cred → allowed.
	if err := checkOAuth2Openapi(context.Background(), credType, "acme",
		UpstreamParams{Name: "o", OpenAPI: "http://x/spec", BaseURL: "http://x", CredSecret: "orders_oauth", CredHeader: "Authorization"}); err != nil {
		t.Fatalf("openapi upstream with oauth2 cred should be allowed, got %v", err)
	}
	// static cred on a url: upstream → allowed (unchanged behavior).
	if err := checkOAuth2Openapi(context.Background(), credType, "acme",
		UpstreamParams{Name: "o", URL: "http://x", CredSecret: "static_key", CredHeader: "Authorization"}); err != nil {
		t.Fatalf("static cred on url upstream should be allowed, got %v", err)
	}
	// nil credType (broker disabled) → skip the check.
	if err := checkOAuth2Openapi(context.Background(), nil, "acme",
		UpstreamParams{Name: "o", URL: "http://x", CredSecret: "orders_oauth", CredHeader: "Authorization"}); err != nil {
		t.Fatalf("nil credType should skip the check, got %v", err)
	}
}
