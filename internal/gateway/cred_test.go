package gateway

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sausheong/runtime/internal/config"
)

func TestCredentialInjection(t *testing.T) {
	gotHeader := make(chan string, 1)
	dial := func(ctx context.Context, s config.GatewayServer) (upstreamConn, error) {
		gotHeader <- s.Headers["Authorization"]
		return &fakeConn{}, nil
	}
	resolver := func(ctx context.Context, tenant, name string) (string, error) {
		if tenant == "t1" && name == "ORDERS_KEY" {
			return "Bearer secret123", nil
		}
		return "", errors.New("no such secret")
	}
	m := NewManager(nil, WithDial(dial), WithCredentials(resolver),
		WithBackoff(5*time.Millisecond, 10*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	_ = m.Add(config.GatewayServer{
		Name: "orders", URL: "http://x", Tenants: []string{"t1"},
		CredSecret: "ORDERS_KEY", CredHeader: "Authorization",
	})
	select {
	case h := <-gotHeader:
		if h != "Bearer secret123" {
			t.Fatalf("injected header = %q", h)
		}
	case <-time.After(time.Second):
		t.Fatal("dial not observed")
	}
	m.Close()
}

func TestCredentialMissingFailsClosed(t *testing.T) {
	dial := func(ctx context.Context, s config.GatewayServer) (upstreamConn, error) {
		t.Fatal("dial must NOT be called when credential resolution fails")
		return nil, nil
	}
	resolver := func(ctx context.Context, tenant, name string) (string, error) {
		return "", errors.New("missing")
	}
	m := NewManager(nil, WithDial(dial), WithCredentials(resolver),
		WithBackoff(5*time.Millisecond, 10*time.Millisecond))
	_, err := m.dialWith(context.Background(), config.GatewayServer{
		Name: "orders", URL: "http://x", Tenants: []string{"t1"},
		CredSecret: "ORDERS_KEY", CredHeader: "Authorization",
	})
	if err == nil {
		t.Fatal("expected dial error when credential missing")
	}
	if strings.Contains(err.Error(), "secret123") {
		t.Fatalf("error leaks secret: %q", err.Error())
	}
}
