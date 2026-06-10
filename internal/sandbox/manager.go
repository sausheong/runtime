package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// errNoSandbox is the single not-found error: a wrong-tenant id and a
// nonexistent id are indistinguishable (existence hidden, Identity M1
// posture).
var errNoSandbox = errors.New("no such sandbox")

const (
	defaultExecTimeout = 30 * time.Second
	maxExecTimeout     = 120 * time.Second
)

// Config bounds Manager behavior; zero/invalid fields get defaults in
// NewManager.
type Config struct {
	MaxPerTenant int           // concurrent sandboxes per tenant (default 5)
	IdleTTL      time.Duration // close after this long unused (default 10m)
	MaxLifetime  time.Duration // close this long after create (default 1h)
	ReadLimit    int           // read_file byte cap (default 256 KiB)
}

// Session is one live sandbox.
type Session struct {
	ID          string
	Tenant      string
	ContainerID string
	CreatedAt   time.Time
	LastUsed    time.Time
	ExpiresAt   time.Time // CreatedAt + MaxLifetime
}

// Manager owns the sandbox sessions over a container Backend.
type Manager struct {
	be  Backend
	cfg Config
	now func() time.Time

	mu       sync.Mutex
	sessions map[string]*Session
}

// NewManager builds a Manager over be, applying defaults for any
// zero/invalid Config fields.
func NewManager(be Backend, cfg Config) *Manager {
	if cfg.MaxPerTenant <= 0 {
		cfg.MaxPerTenant = 5
	}
	if cfg.IdleTTL <= 0 {
		cfg.IdleTTL = 10 * time.Minute
	}
	if cfg.MaxLifetime <= 0 {
		cfg.MaxLifetime = time.Hour
	}
	if cfg.ReadLimit <= 0 {
		cfg.ReadLimit = 256 << 10
	}
	return &Manager{
		be:       be,
		cfg:      cfg,
		now:      time.Now,
		sessions: map[string]*Session{},
	}
}

// newSandboxID returns "sbx-" + 32 hex chars from 16 random bytes.
func newSandboxID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is unrecoverable.
		panic(fmt.Sprintf("sandbox: crypto/rand failed: %v", err))
	}
	return "sbx-" + hex.EncodeToString(b[:])
}

// Create starts a new sandbox for tenant, enforcing the per-tenant cap.
func (m *Manager) Create(ctx context.Context, tenant string) (*Session, error) {
	m.mu.Lock()
	count := 0
	for _, s := range m.sessions {
		if s.Tenant == tenant {
			count++
		}
	}
	if count >= m.cfg.MaxPerTenant {
		m.mu.Unlock()
		return nil, fmt.Errorf("sandbox limit reached (%d per tenant): close one with close_sandbox", m.cfg.MaxPerTenant)
	}
	m.mu.Unlock()

	containerID, err := m.be.Create(ctx, tenant)
	if err != nil {
		return nil, fmt.Errorf("sandbox backend unavailable: %w", err)
	}

	now := m.now()
	s := &Session{
		ID:          newSandboxID(),
		Tenant:      tenant,
		ContainerID: containerID,
		CreatedAt:   now,
		LastUsed:    now,
		ExpiresAt:   now.Add(m.cfg.MaxLifetime),
	}
	m.mu.Lock()
	m.sessions[s.ID] = s
	m.mu.Unlock()
	return s, nil
}

// lookup resolves a sandbox id for tenant, touching LastUsed. A missing id
// and a foreign tenant's id return the identical errNoSandbox.
func (m *Manager) lookup(tenant, id string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok || s.Tenant != tenant {
		return nil, errNoSandbox
	}
	s.LastUsed = m.now()
	return s, nil
}

// clampTimeout converts a caller-supplied timeout in seconds into a bounded
// duration: <=0 means the default, anything above the max is clamped.
func clampTimeout(seconds int) time.Duration {
	if seconds <= 0 {
		return defaultExecTimeout
	}
	if d := time.Duration(seconds) * time.Second; d <= maxExecTimeout {
		return d
	}
	return maxExecTimeout
}

// ExecCode runs Python code inside the sandbox.
func (m *Manager) ExecCode(ctx context.Context, tenant, id, code string, timeoutSeconds int) (ExecResult, error) {
	s, err := m.lookup(tenant, id)
	if err != nil {
		return ExecResult{}, err
	}
	return m.be.Exec(ctx, s.ContainerID, []string{"python3", "-c", code}, clampTimeout(timeoutSeconds))
}

// ExecCommand runs a shell command inside the sandbox.
func (m *Manager) ExecCommand(ctx context.Context, tenant, id, command string, timeoutSeconds int) (ExecResult, error) {
	s, err := m.lookup(tenant, id)
	if err != nil {
		return ExecResult{}, err
	}
	return m.be.Exec(ctx, s.ContainerID, []string{"sh", "-c", command}, clampTimeout(timeoutSeconds))
}

// WriteFile writes content to a /workspace-confined path in the sandbox.
func (m *Manager) WriteFile(ctx context.Context, tenant, id, path string, content []byte) error {
	confined, err := confinePath(path)
	if err != nil {
		return err
	}
	s, err := m.lookup(tenant, id)
	if err != nil {
		return err
	}
	return m.be.WriteFile(ctx, s.ContainerID, confined, content)
}

// ReadFile reads a /workspace-confined path from the sandbox, capped at
// cfg.ReadLimit bytes.
func (m *Manager) ReadFile(ctx context.Context, tenant, id, path string) ([]byte, bool, error) {
	confined, err := confinePath(path)
	if err != nil {
		return nil, false, err
	}
	s, err := m.lookup(tenant, id)
	if err != nil {
		return nil, false, err
	}
	return m.be.ReadFile(ctx, s.ContainerID, confined, m.cfg.ReadLimit)
}

// List returns copies of tenant's live sessions.
func (m *Manager) List(tenant string) []Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Session
	for _, s := range m.sessions {
		if s.Tenant == tenant {
			out = append(out, *s)
		}
	}
	return out
}

// Close removes the sandbox. It is idempotent and never reveals existence:
// an unknown or foreign id returns nil.
func (m *Manager) Close(ctx context.Context, tenant, id string) error {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if !ok || s.Tenant != tenant {
		m.mu.Unlock()
		return nil
	}
	delete(m.sessions, id)
	m.mu.Unlock()

	if err := m.be.Remove(ctx, s.ContainerID); err != nil {
		slog.Warn("sandbox close: container remove failed",
			"sandbox_id", s.ID, "container_id", s.ContainerID, "err", err)
	}
	return nil
}

// ReapOnce closes every session that is idle past IdleTTL or past its hard
// max lifetime.
func (m *Manager) ReapOnce(ctx context.Context) {
	now := m.now()
	m.mu.Lock()
	var expired []*Session
	for id, s := range m.sessions {
		if now.Sub(s.LastUsed) > m.cfg.IdleTTL || now.After(s.ExpiresAt) {
			expired = append(expired, s)
			delete(m.sessions, id)
		}
	}
	m.mu.Unlock()

	for _, s := range expired {
		if err := m.be.Remove(ctx, s.ContainerID); err != nil {
			slog.Warn("sandbox reap: container remove failed",
				"sandbox_id", s.ID, "container_id", s.ContainerID, "err", err)
			continue
		}
		slog.Info("sandbox reaped",
			"sandbox_id", s.ID, "tenant", s.Tenant, "container_id", s.ContainerID)
	}
}

// StartReaper runs ReapOnce every interval until ctx is canceled.
func (m *Manager) StartReaper(ctx context.Context, interval time.Duration) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.ReapOnce(ctx)
			}
		}
	}()
}

// ReapStartup removes all leftover sandbox containers (crash recovery).
// Per-container removal failures are logged, not fatal.
func (m *Manager) ReapStartup(ctx context.Context) error {
	ids, err := m.be.ListLeftovers(ctx)
	if err != nil {
		return fmt.Errorf("list leftover sandboxes: %w", err)
	}
	for _, id := range ids {
		if err := m.be.Remove(ctx, id); err != nil {
			slog.Warn("startup reap: container remove failed", "container_id", id, "err", err)
		}
	}
	return nil
}
