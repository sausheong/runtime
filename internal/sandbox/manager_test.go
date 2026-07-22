package sandbox

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
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	m := NewManager(be, Config{
		MaxPerTenant: 2,
		IdleTTL:      10 * time.Minute,
		MaxLifetime:  time.Hour,
		ReadLimit:    1024,
	})
	m.now = func() time.Time { return now }
	return m, be, &now
}

func testManagerScoped(t *testing.T) (*Manager, *time.Time) {
	t.Helper()
	be := NewFakeBackend()
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	m := NewManager(be, Config{
		MaxPerTenant: 5, IdleTTL: 10 * time.Minute, MaxLifetime: time.Hour,
		ReadLimit: 1024, SessionScoped: true,
	})
	m.now = func() time.Time { return now }
	return m, &now
}

func TestSessionScopedCrossSessionHidden(t *testing.T) {
	ctx := context.Background()
	m, _ := testManagerScoped(t)
	s, err := m.Create(ctx, "acme", "sessA")
	if err != nil {
		t.Fatal(err)
	}
	// Same session: works.
	if _, err := m.ExecCode(ctx, "acme", "sessA", s.ID, "print(1)", 0); err != nil {
		t.Fatalf("same-session exec failed: %v", err)
	}
	// Different session, same tenant: hidden as not-found.
	if _, err := m.ExecCode(ctx, "acme", "sessB", s.ID, "print(1)", 0); err == nil {
		t.Fatal("cross-session exec should return errNoSandbox")
	}
}

func TestSessionScopedListIsolation(t *testing.T) {
	ctx := context.Background()
	m, _ := testManagerScoped(t)
	if _, err := m.Create(ctx, "acme", "sessA"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(ctx, "acme", "sessB"); err != nil {
		t.Fatal(err)
	}
	if got := m.List("acme", "sessA"); len(got) != 1 {
		t.Fatalf("List(sessA) = %d, want 1", len(got))
	}
}

func TestCloseSessionReapsOnlyTarget(t *testing.T) {
	ctx := context.Background()
	m, _ := testManagerScoped(t)
	a, _ := m.Create(ctx, "acme", "sessA")
	b, _ := m.Create(ctx, "acme", "sessB")
	if err := m.CloseSession(ctx, "acme", "sessA"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ExecCode(ctx, "acme", "sessA", a.ID, "x", 0); err == nil {
		t.Fatal("sessA box should be gone")
	}
	if _, err := m.ExecCode(ctx, "acme", "sessB", b.ID, "print(1)", 0); err != nil {
		t.Fatalf("sessB box should survive: %v", err)
	}
}

func TestCloseSessionNoopWhenTenantScoped(t *testing.T) {
	ctx := context.Background()
	m, _, _ := testManager(t) // tenant-scoped
	s, _ := m.Create(ctx, "acme", "")
	if err := m.CloseSession(ctx, "acme", "anything"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ExecCode(ctx, "acme", "", s.ID, "print(1)", 0); err != nil {
		t.Fatalf("tenant-scoped box must survive CloseSession no-op: %v", err)
	}
}

func TestCreateExecCloseRoundTrip(t *testing.T) {
	ctx := context.Background()
	m, _, _ := testManager(t)

	s, err := m.Create(ctx, "acme", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s.ID, "sbx-") || len(s.ID) != 4+32 {
		t.Fatalf("bad id %q", s.ID)
	}

	res, err := m.ExecCode(ctx, "acme", "", s.ID, "print(1)", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Stdout, "python3") {
		t.Fatalf("exec didn't run python3: %+v", res)
	}

	if err := m.Close(ctx, "acme", "", s.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(ctx, "acme", "", s.ID); err != nil {
		t.Fatal("second close should be nil (idempotent)")
	}
	if _, err := m.ExecCode(ctx, "acme", "", s.ID, "x", 0); err == nil {
		t.Fatal("exec after close should fail")
	}
}

func TestCrossTenantHiddenAsNotFound(t *testing.T) {
	ctx := context.Background()
	m, _, _ := testManager(t)
	s, _ := m.Create(ctx, "acme", "")

	_, errCross := m.ExecCode(ctx, "globex", "", s.ID, "x", 0)
	_, errMissing := m.ExecCode(ctx, "globex", "", "sbx-doesnotexist", "x", 0)
	if errCross == nil || errMissing == nil {
		t.Fatal("both must error")
	}
	if errCross.Error() != errMissing.Error() {
		t.Fatalf("cross-tenant error %q must equal missing-id error %q (existence hidden)",
			errCross, errMissing)
	}

	if got := m.List("globex", ""); len(got) != 0 {
		t.Fatalf("globex sees %d sandboxes", len(got))
	}
	if got := m.List("acme", ""); len(got) != 1 {
		t.Fatalf("acme sees %d sandboxes", len(got))
	}
}

func TestPerTenantCap(t *testing.T) {
	ctx := context.Background()
	m, _, _ := testManager(t) // cap 2
	if _, err := m.Create(ctx, "acme", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(ctx, "acme", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(ctx, "acme", ""); err == nil ||
		!strings.Contains(err.Error(), "limit") {
		t.Fatalf("third create should hit the cap, got %v", err)
	}
	if _, err := m.Create(ctx, "globex", ""); err != nil {
		t.Fatal(err)
	}
}

// slowCreateBackend delegates to an inner Backend but makes Create slow,
// widening the window in which concurrent Creates could race past the cap.
type slowCreateBackend struct {
	Backend
	delay time.Duration
}

func (b *slowCreateBackend) Create(ctx context.Context, tenant string) (string, error) {
	time.Sleep(b.delay)
	return b.Backend.Create(ctx, tenant)
}

func TestPerTenantCapUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	be := &slowCreateBackend{Backend: NewFakeBackend(), delay: 50 * time.Millisecond}
	m := NewManager(be, Config{
		MaxPerTenant: 2,
		IdleTTL:      10 * time.Minute,
		MaxLifetime:  time.Hour,
		ReadLimit:    1024,
	})

	const attempts = 6
	var ok, limited atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := m.Create(ctx, "acme", "")
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
		t.Fatalf("got %d successes / %d limit errors, want 2 / 4", ok.Load(), limited.Load())
	}
	if got := m.List("acme", ""); len(got) != 2 {
		t.Fatalf("manager tracks %d sessions, want 2", len(got))
	}
	ids, _ := be.ListLeftovers(ctx)
	if len(ids) != 2 {
		t.Fatalf("backend has %d containers, want 2 (no leaks past the cap)", len(ids))
	}
}

// blockingCreateBackend delegates to an inner Backend but parks Create until
// release is closed, signaling entry on entered.
type blockingCreateBackend struct {
	Backend
	entered chan struct{}
	release chan struct{}
}

func (b *blockingCreateBackend) Create(ctx context.Context, tenant string) (string, error) {
	close(b.entered)
	<-b.release
	return b.Backend.Create(ctx, tenant)
}

// TestCloseDuringCreateDoesNotLeakContainer pins the lost-reservation path:
// if the session is closed while be.Create is in flight, Create must remove
// the freshly created container and report errNoSandbox.
func TestCloseDuringCreateDoesNotLeakContainer(t *testing.T) {
	ctx := context.Background()
	be := &blockingCreateBackend{
		Backend: NewFakeBackend(),
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	m := NewManager(be, Config{
		MaxPerTenant: 2,
		IdleTTL:      10 * time.Minute,
		MaxLifetime:  time.Hour,
		ReadLimit:    1024,
	})

	errCh := make(chan error, 1)
	go func() {
		_, err := m.Create(ctx, "acme", "")
		errCh <- err
	}()

	<-be.entered // be.Create is in flight; the reservation is visible

	sessions := m.List("acme", "")
	if len(sessions) != 1 {
		t.Fatalf("expected 1 reserved session, got %d", len(sessions))
	}
	if err := m.Close(ctx, "acme", "", sessions[0].ID); err != nil {
		t.Fatal(err)
	}

	close(be.release)
	if err := <-errCh; !errors.Is(err, errNoSandbox) {
		t.Fatalf("Create after lost reservation = %v, want errNoSandbox", err)
	}

	ids, err := be.ListLeftovers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Fatalf("container leaked: %v", ids)
	}
	if got := m.List("acme", ""); len(got) != 0 {
		t.Fatalf("manager still tracks %d sessions", len(got))
	}
}

func TestClampTimeout(t *testing.T) {
	if d := clampTimeout(0); d != 30*time.Second {
		t.Fatalf("default = %v", d)
	}
	if d := clampTimeout(999); d != 120*time.Second {
		t.Fatalf("clamp = %v", d)
	}
	if d := clampTimeout(60); d != 60*time.Second {
		t.Fatalf("pass-through = %v", d)
	}
	if d := clampTimeout(-5); d != 30*time.Second {
		t.Fatalf("negative = %v", d)
	}
	// Huge values must clamp, not overflow time.Duration into negative/zero.
	if d := clampTimeout(10_000_000_000); d != maxExecTimeout {
		t.Fatalf("10e9 seconds = %v, want %v", d, maxExecTimeout)
	}
	if d := clampTimeout(1 << 62); d != maxExecTimeout {
		t.Fatalf("1<<62 seconds = %v, want %v", d, maxExecTimeout)
	}
}

func TestFilesConfinedAndLimited(t *testing.T) {
	ctx := context.Background()
	m, _, _ := testManager(t)
	s, _ := m.Create(ctx, "acme", "")

	if err := m.WriteFile(ctx, "acme", "", s.ID, "../etc/passwd", []byte("x")); err == nil {
		t.Fatal("escape should be rejected")
	}
	if err := m.WriteFile(ctx, "acme", "", s.ID, "big.txt", []byte(strings.Repeat("a", 2048))); err != nil {
		t.Fatal(err)
	}
	content, truncated, err := m.ReadFile(ctx, "acme", "", s.ID, "big.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !truncated || len(content) != 1024 {
		t.Fatalf("len=%d truncated=%v", len(content), truncated)
	}
}

func TestReaperIdleAndMaxLifetime(t *testing.T) {
	ctx := context.Background()
	m, be, now := testManager(t)

	idle, _ := m.Create(ctx, "acme", "")
	busy, _ := m.Create(ctx, "acme", "")

	*now = now.Add(9 * time.Minute)
	if _, err := m.ExecCode(ctx, "acme", "", busy.ID, "x", 0); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(2 * time.Minute)
	m.ReapOnce(ctx)
	if _, err := m.ExecCode(ctx, "acme", "", idle.ID, "x", 0); err == nil {
		t.Fatal("idle sandbox should be reaped")
	}
	if _, err := m.ExecCode(ctx, "acme", "", busy.ID, "x", 0); err != nil {
		t.Fatalf("busy sandbox reaped early: %v", err)
	}

	*now = now.Add(50 * time.Minute)
	m.ReapOnce(ctx)
	if _, err := m.ExecCode(ctx, "acme", "", busy.ID, "x", 0); err == nil {
		t.Fatal("sandbox past max lifetime should be reaped")
	}
	ids, _ := be.ListLeftovers(ctx)
	if len(ids) != 0 {
		t.Fatalf("backend still has %v", ids)
	}
}

// TestReaperMaxLifetimeDespiteActivity pins the ExpiresAt branch on its own:
// the session is touched every 5 minutes (idle never exceeds IdleTTL), so
// only the hard max-lifetime clause can reap it.
func TestReaperMaxLifetimeDespiteActivity(t *testing.T) {
	ctx := context.Background()
	m, _, now := testManager(t) // IdleTTL 10m, MaxLifetime 1h

	s, err := m.Create(ctx, "acme", "")
	if err != nil {
		t.Fatal(err)
	}

	var elapsed time.Duration
	for elapsed <= time.Hour {
		*now = now.Add(5 * time.Minute)
		elapsed += 5 * time.Minute
		if _, err := m.ExecCode(ctx, "acme", "", s.ID, "x", 0); err != nil {
			t.Fatalf("exec at +%v: %v", elapsed, err)
		}
	}

	m.ReapOnce(ctx)
	if _, err := m.ExecCode(ctx, "acme", "", s.ID, "x", 0); err == nil {
		t.Fatal("active sandbox past max lifetime must still be reaped")
	}
}

func TestReapStartupRemovesLeftovers(t *testing.T) {
	ctx := context.Background()
	be := NewFakeBackend()
	_, _ = be.Create(ctx, "old1")
	_, _ = be.Create(ctx, "old2")
	m := NewManager(be, Config{MaxPerTenant: 5})
	if err := m.ReapStartup(ctx); err != nil {
		t.Fatal(err)
	}
	ids, _ := be.ListLeftovers(ctx)
	if len(ids) != 0 {
		t.Fatalf("leftovers not reaped: %v", ids)
	}
}
