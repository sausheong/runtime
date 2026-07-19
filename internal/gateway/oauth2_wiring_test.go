package gateway

import (
	"context"
	"testing"

	"github.com/sausheong/runtime/internal/config"
)

func TestCredForResolvesUpstreamCredential(t *testing.T) {
	m := NewManager([]config.GatewayServer{{
		Name: "orders", OpenAPI: "http://x/spec", BaseURL: "http://x",
		Tenants: []string{"acme"}, CredSecret: "orders_oauth", CredHeader: "Authorization",
	}})
	sec, hdr := m.CredFor("orders__list")
	if sec != "orders_oauth" || hdr != "Authorization" {
		t.Fatalf("CredFor = %q, %q", sec, hdr)
	}
	if s, _ := m.CredFor("search_tools"); s != "" {
		t.Fatalf("names without __ must not carry a credential, got %q", s)
	}
}

func TestCredentialHeaderCtxRoundTrip(t *testing.T) {
	ctx := WithCredentialHeader(context.Background(), "Authorization", "Bearer xyz")
	h, v := CredentialHeaderFrom(ctx)
	if h != "Authorization" || v != "Bearer xyz" {
		t.Fatalf("round trip = %q, %q", h, v)
	}
	if h, _ := CredentialHeaderFrom(context.Background()); h != "" {
		t.Fatalf("empty ctx should carry no header, got %q", h)
	}
}
