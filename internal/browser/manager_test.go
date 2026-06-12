package browser

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testManager(t *testing.T) (*Manager, Backend, *time.Time) {
	t.Helper()
	be := NewFakeBackend()
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)
	m := NewManager(be, Config{MaxPerTenant: 2, IdleTTL: 10 * time.Minute, MaxLifetime: time.Hour})
	m.now = func() time.Time { return now }
	return m, be, &now
}

func TestCreateCloseRoundTrip(t *testing.T) {
	ctx := context.Background()
	m, _, _ := testManager(t)
	s, err := m.Create(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s.ID, "brw-") || len(s.ID) != 4+32 {
		t.Fatalf("bad id %q", s.ID)
	}
	if err := m.Close(ctx, "acme", s.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(ctx, "acme", s.ID); err != nil {
		t.Fatal("second close should be nil (idempotent)")
	}
}

func TestCrossTenantHiddenAsNotFound(t *testing.T) {
	ctx := context.Background()
	m, _, _ := testManager(t)
	s, _ := m.Create(ctx, "acme")
	_, errCross := m.Lookup("globex", s.ID)
	_, errMissing := m.Lookup("globex", "brw-doesnotexist")
	if errCross == nil || errMissing == nil {
		t.Fatal("both must error")
	}
	if errCross.Error() != errMissing.Error() {
		t.Fatalf("cross-tenant %q must equal missing %q", errCross, errMissing)
	}
	if got := m.List("globex"); len(got) != 0 {
		t.Fatalf("globex sees %d", len(got))
	}
	if got := m.List("acme"); len(got) != 1 {
		t.Fatalf("acme sees %d", len(got))
	}
}

func TestPerTenantCap(t *testing.T) {
	ctx := context.Background()
	m, _, _ := testManager(t)
	if _, err := m.Create(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(ctx, "acme"); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("third create should hit the cap, got %v", err)
	}
}

type slowCreateBackend struct {
	Backend
	delay time.Duration
}

func (b *slowCreateBackend) Create(ctx context.Context, tenant, proxy string) (BrowserHandle, error) {
	time.Sleep(b.delay)
	return b.Backend.Create(ctx, tenant, proxy)
}

func TestPerTenantCapUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	be := &slowCreateBackend{Backend: NewFakeBackend(), delay: 50 * time.Millisecond}
	m := NewManager(be, Config{MaxPerTenant: 2, IdleTTL: 10 * time.Minute, MaxLifetime: time.Hour})
	const attempts = 6
	var ok, limited atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := m.Create(ctx, "acme")
			switch {
			case err == nil:
				ok.Add(1)
			case strings.Contains(err.Error(), "limit"):
				limited.Add(1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 2 || limited.Load() != 4 {
		t.Fatalf("got %d ok / %d limited, want 2/4", ok.Load(), limited.Load())
	}
	ids, _ := be.ListLeftovers(ctx)
	if len(ids) != 2 {
		t.Fatalf("backend has %d containers, want 2", len(ids))
	}
}

type blockingCreateBackend struct {
	Backend
	entered chan struct{}
	release chan struct{}
}

func (b *blockingCreateBackend) Create(ctx context.Context, tenant, proxy string) (BrowserHandle, error) {
	close(b.entered)
	<-b.release
	return b.Backend.Create(ctx, tenant, proxy)
}

func TestCloseDuringCreateDoesNotLeak(t *testing.T) {
	ctx := context.Background()
	be := &blockingCreateBackend{Backend: NewFakeBackend(), entered: make(chan struct{}), release: make(chan struct{})}
	m := NewManager(be, Config{MaxPerTenant: 2, IdleTTL: 10 * time.Minute, MaxLifetime: time.Hour})
	errCh := make(chan error, 1)
	go func() {
		_, err := m.Create(ctx, "acme")
		errCh <- err
	}()
	<-be.entered
	sessions := m.List("acme")
	if len(sessions) != 1 {
		t.Fatalf("expected 1 reserved session, got %d", len(sessions))
	}
	if err := m.Close(ctx, "acme", sessions[0].ID); err != nil {
		t.Fatal(err)
	}
	close(be.release)
	if err := <-errCh; !errors.Is(err, errNoSandbox) {
		t.Fatalf("Create after lost reservation = %v, want errNoSandbox", err)
	}
	ids, _ := be.ListLeftovers(ctx)
	if len(ids) != 0 {
		t.Fatalf("container leaked: %v", ids)
	}
}

func TestReaperIdleAndMaxLifetime(t *testing.T) {
	ctx := context.Background()
	m, be, now := testManager(t)
	idle, _ := m.Create(ctx, "acme")
	busy, _ := m.Create(ctx, "acme")
	*now = now.Add(9 * time.Minute)
	if _, err := m.Lookup("acme", busy.ID); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(2 * time.Minute)
	m.ReapOnce(ctx)
	if _, err := m.Lookup("acme", idle.ID); err == nil {
		t.Fatal("idle session should be reaped")
	}
	if _, err := m.Lookup("acme", busy.ID); err != nil {
		t.Fatalf("busy session reaped early: %v", err)
	}
	*now = now.Add(50 * time.Minute)
	m.ReapOnce(ctx)
	if _, err := m.Lookup("acme", busy.ID); err == nil {
		t.Fatal("session past max lifetime should be reaped")
	}
	ids, _ := be.ListLeftovers(ctx)
	if len(ids) != 0 {
		t.Fatalf("backend still has %v", ids)
	}
}

func TestReapStartupRemovesLeftovers(t *testing.T) {
	ctx := context.Background()
	be := NewFakeBackend()
	_, _ = be.Create(ctx, "old1", "")
	_, _ = be.Create(ctx, "old2", "")
	m := NewManager(be, Config{MaxPerTenant: 5})
	if err := m.ReapStartup(ctx); err != nil {
		t.Fatal(err)
	}
	ids, _ := be.ListLeftovers(ctx)
	if len(ids) != 0 {
		t.Fatalf("leftovers not reaped: %v", ids)
	}
}
