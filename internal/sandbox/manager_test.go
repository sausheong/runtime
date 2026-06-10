package sandbox

import (
	"context"
	"strings"
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

func TestCreateExecCloseRoundTrip(t *testing.T) {
	ctx := context.Background()
	m, _, _ := testManager(t)

	s, err := m.Create(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s.ID, "sbx-") || len(s.ID) != 4+32 {
		t.Fatalf("bad id %q", s.ID)
	}

	res, err := m.ExecCode(ctx, "acme", s.ID, "print(1)", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Stdout, "python3") {
		t.Fatalf("exec didn't run python3: %+v", res)
	}

	if err := m.Close(ctx, "acme", s.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(ctx, "acme", s.ID); err != nil {
		t.Fatal("second close should be nil (idempotent)")
	}
	if _, err := m.ExecCode(ctx, "acme", s.ID, "x", 0); err == nil {
		t.Fatal("exec after close should fail")
	}
}

func TestCrossTenantHiddenAsNotFound(t *testing.T) {
	ctx := context.Background()
	m, _, _ := testManager(t)
	s, _ := m.Create(ctx, "acme")

	_, errCross := m.ExecCode(ctx, "globex", s.ID, "x", 0)
	_, errMissing := m.ExecCode(ctx, "globex", "sbx-doesnotexist", "x", 0)
	if errCross == nil || errMissing == nil {
		t.Fatal("both must error")
	}
	if errCross.Error() != errMissing.Error() {
		t.Fatalf("cross-tenant error %q must equal missing-id error %q (existence hidden)",
			errCross, errMissing)
	}

	if got := m.List("globex"); len(got) != 0 {
		t.Fatalf("globex sees %d sandboxes", len(got))
	}
	if got := m.List("acme"); len(got) != 1 {
		t.Fatalf("acme sees %d sandboxes", len(got))
	}
}

func TestPerTenantCap(t *testing.T) {
	ctx := context.Background()
	m, _, _ := testManager(t) // cap 2
	if _, err := m.Create(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(ctx, "acme"); err == nil ||
		!strings.Contains(err.Error(), "limit") {
		t.Fatalf("third create should hit the cap, got %v", err)
	}
	if _, err := m.Create(ctx, "globex"); err != nil {
		t.Fatal(err)
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
}

func TestFilesConfinedAndLimited(t *testing.T) {
	ctx := context.Background()
	m, _, _ := testManager(t)
	s, _ := m.Create(ctx, "acme")

	if err := m.WriteFile(ctx, "acme", s.ID, "../etc/passwd", []byte("x")); err == nil {
		t.Fatal("escape should be rejected")
	}
	if err := m.WriteFile(ctx, "acme", s.ID, "big.txt", []byte(strings.Repeat("a", 2048))); err != nil {
		t.Fatal(err)
	}
	content, truncated, err := m.ReadFile(ctx, "acme", s.ID, "big.txt")
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

	idle, _ := m.Create(ctx, "acme")
	busy, _ := m.Create(ctx, "acme")

	*now = now.Add(9 * time.Minute)
	if _, err := m.ExecCode(ctx, "acme", busy.ID, "x", 0); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(2 * time.Minute)
	m.ReapOnce(ctx)
	if _, err := m.ExecCode(ctx, "acme", idle.ID, "x", 0); err == nil {
		t.Fatal("idle sandbox should be reaped")
	}
	if _, err := m.ExecCode(ctx, "acme", busy.ID, "x", 0); err != nil {
		t.Fatalf("busy sandbox reaped early: %v", err)
	}

	*now = now.Add(50 * time.Minute)
	m.ReapOnce(ctx)
	if _, err := m.ExecCode(ctx, "acme", busy.ID, "x", 0); err == nil {
		t.Fatal("sandbox past max lifetime should be reaped")
	}
	ids, _ := be.ListLeftovers(ctx)
	if len(ids) != 0 {
		t.Fatalf("backend still has %v", ids)
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
