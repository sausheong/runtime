package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/sausheong/runtime/internal/config"
)

// Reuses the package's existing test fakes: fakeConn (manager_test.go) already
// satisfies upstreamConn, so the dial closure returns one of those.
func TestManagerAddRemove(t *testing.T) {
	dialed := make(chan string, 8)
	dial := func(ctx context.Context, s config.GatewayServer) (upstreamConn, error) {
		dialed <- s.Name
		return &fakeConn{}, nil
	}
	m := NewManager(nil, WithDial(dial), WithBackoff(5*time.Millisecond, 10*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)

	if err := m.Add(config.GatewayServer{Name: "a", URL: "http://a"}); err != nil {
		t.Fatal(err)
	}
	waitDial(t, dialed, "a")
	if err := m.Add(config.GatewayServer{Name: "a", URL: "http://a2"}); err == nil {
		t.Fatal("expected duplicate-name rejection")
	}
	if st := m.Status(""); len(st) != 1 || st[0].Name != "a" {
		t.Fatalf("status after add: %+v", st)
	}
	m.Remove("a")
	if st := m.Status(""); len(st) != 0 {
		t.Fatalf("status after remove: %+v", st)
	}
	m.Close()
}

func waitDial(t *testing.T, ch <-chan string, want string) {
	t.Helper()
	select {
	case got := <-ch:
		if got != want {
			t.Fatalf("dialed %q want %q", got, want)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for dial %q", want)
	}
}
