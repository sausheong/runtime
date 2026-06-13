package gateway

import (
	"context"
	"fmt"
	"sync"
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

// TestManagerAddRemoveConcurrent hammers the concurrent Add/Remove path that the
// sequential TestManagerAddRemove cannot reach. The dial blocks ~2ms so Remove
// frequently fires while supervise is mid-dial, widening the C1 (cancel race /
// missed-cancel) and I2 (in-flight-dial conn leak) windows. Must pass under -race.
func TestManagerAddRemoveConcurrent(t *testing.T) {
	dial := func(ctx context.Context, s config.GatewayServer) (upstreamConn, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Millisecond):
		}
		return &fakeConn{}, nil
	}
	m := NewManager(nil, WithDial(dial), WithBackoff(time.Millisecond, 2*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		name := fmt.Sprintf("u%d", i)
		go func() { defer wg.Done(); _ = m.Add(config.GatewayServer{Name: name, URL: "http://x"}) }()
		go func() { defer wg.Done(); m.Remove(name) }()
	}
	wg.Wait()
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
