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

	// SessionScoped keys sandboxes by (tenant, session, id) instead of
	// (tenant, id): a handle minted in one agent session is invisible to
	// other sessions of the same tenant. Default false (tenant-scoped).
	SessionScoped bool
}

// Session is one live sandbox.
type Session struct {
	ID          string
	Tenant      string
	Session     string // owning agent session; "" in tenant-scoped mode
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
//
// The slot is RESERVED under the lock before the backend call: the session
// (with ID but no ContainerID yet) is inserted while holding mu so that N
// concurrent Creates at cap-1 cannot all pass the count check during a slow
// be.Create. The ID is generated before any backend work, so a crypto/rand
// panic cannot leak a container.
func (m *Manager) Create(ctx context.Context, tenant, session string) (*Session, error) {
	now := m.now()
	s := &Session{
		ID:        newSandboxID(),
		Tenant:    tenant,
		Session:   session,
		CreatedAt: now,
		LastUsed:  now,
		ExpiresAt: now.Add(m.cfg.MaxLifetime),
	}

	m.mu.Lock()
	// Cap-counting is per-tenant across ALL sessions regardless of scope
	// (documented invariant): session scoping isolates handles, not quota.
	count := 0
	for _, other := range m.sessions {
		if other.Tenant == tenant {
			count++
		}
	}
	if count >= m.cfg.MaxPerTenant {
		m.mu.Unlock()
		return nil, fmt.Errorf("sandbox limit reached (%d per tenant): close one with close_sandbox", m.cfg.MaxPerTenant)
	}
	m.sessions[s.ID] = s // reservation: counts toward the cap from here on
	m.mu.Unlock()

	containerID, err := m.be.Create(ctx, tenant)
	if err != nil {
		m.mu.Lock()
		delete(m.sessions, s.ID)
		m.mu.Unlock()
		// This wraps the raw backend error, which reaches the LLM via the
		// tool result. That is deliberate: create failures (daemon down,
		// image missing) are operator-relevant and carry no per-sandbox
		// state worth hiding — unlike post-create errors, which maskIfGone
		// scrubs.
		return nil, fmt.Errorf("sandbox backend unavailable: %w", err)
	}

	m.mu.Lock()
	if _, ok := m.sessions[s.ID]; !ok {
		// The reservation vanished during be.Create (the tenant closed it or
		// the reaper fired): nobody tracks this container, so remove it now
		// rather than leaving it for the startup reap.
		m.mu.Unlock()
		if rmErr := m.be.Remove(ctx, containerID); rmErr != nil {
			slog.Warn("sandbox create: container remove after lost reservation failed",
				"sandbox_id", s.ID, "container_id", containerID, "err", rmErr)
		}
		return nil, errNoSandbox
	}
	s.ContainerID = containerID
	m.mu.Unlock()
	return s, nil
}

// lookup resolves a sandbox id for tenant, touching LastUsed. A missing id
// and a foreign tenant's id return the identical errNoSandbox. When
// SessionScoped, a foreign session's id is hidden identically to nonexistent.
func (m *Manager) lookup(tenant, session, id string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok || s.Tenant != tenant {
		return nil, errNoSandbox
	}
	if m.cfg.SessionScoped && s.Session != session {
		return nil, errNoSandbox // foreign session hidden identically to nonexistent
	}
	s.LastUsed = m.now()
	return s, nil
}

// maskIfGone scrubs backend errors before they reach the LLM via tool
// results. If the session vanished mid-call (e.g. the reaper removed it
// while the call was in flight), return errNoSandbox. If the session still
// exists, the raw error (which can carry container ids and engine internals)
// is logged for the operator and replaced with a generic message.
func (m *Manager) maskIfGone(id string, err error) error {
	if err == nil {
		return nil
	}
	m.mu.Lock()
	_, ok := m.sessions[id]
	m.mu.Unlock()
	if !ok {
		return errNoSandbox
	}
	// A missing file is user-actionable and leaks nothing — pass it through.
	if errors.Is(err, ErrNoSuchFile) {
		return err
	}
	slog.Warn("sandbox: backend error", "sandbox", id, "err", err)
	return errors.New("sandbox execution failed (see sandboxd logs)")
}

// clampTimeout converts a caller-supplied timeout in seconds into a bounded
// duration: <=0 means the default, anything above the max is clamped. The
// comparison happens in seconds BEFORE multiplying, so huge values cannot
// overflow time.Duration into negative/zero.
func clampTimeout(seconds int) time.Duration {
	if seconds <= 0 {
		return defaultExecTimeout
	}
	if seconds > int(maxExecTimeout/time.Second) {
		return maxExecTimeout
	}
	return time.Duration(seconds) * time.Second
}

// ExecCode runs Python code inside the sandbox.
func (m *Manager) ExecCode(ctx context.Context, tenant, session, id, code string, timeoutSeconds int) (ExecResult, error) {
	s, err := m.lookup(tenant, session, id)
	if err != nil {
		return ExecResult{}, err
	}
	res, err := m.be.Exec(ctx, s.ContainerID, []string{"python3", "-c", code}, clampTimeout(timeoutSeconds))
	return res, m.maskIfGone(id, err)
}

// ExecCommand runs a shell command inside the sandbox.
func (m *Manager) ExecCommand(ctx context.Context, tenant, session, id, command string, timeoutSeconds int) (ExecResult, error) {
	s, err := m.lookup(tenant, session, id)
	if err != nil {
		return ExecResult{}, err
	}
	res, err := m.be.Exec(ctx, s.ContainerID, []string{"sh", "-c", command}, clampTimeout(timeoutSeconds))
	return res, m.maskIfGone(id, err)
}

// WriteFile writes content to a /workspace-confined path in the sandbox.
func (m *Manager) WriteFile(ctx context.Context, tenant, session, id, path string, content []byte) error {
	confined, err := confinePath(path)
	if err != nil {
		return err
	}
	s, err := m.lookup(tenant, session, id)
	if err != nil {
		return err
	}
	return m.maskIfGone(id, m.be.WriteFile(ctx, s.ContainerID, confined, content))
}

// ReadFile reads a /workspace-confined path from the sandbox, capped at
// cfg.ReadLimit bytes.
func (m *Manager) ReadFile(ctx context.Context, tenant, session, id, path string) ([]byte, bool, error) {
	confined, err := confinePath(path)
	if err != nil {
		return nil, false, err
	}
	s, err := m.lookup(tenant, session, id)
	if err != nil {
		return nil, false, err
	}
	content, truncated, err := m.be.ReadFile(ctx, s.ContainerID, confined, m.cfg.ReadLimit)
	return content, truncated, m.maskIfGone(id, err)
}

// List returns copies of tenant's live sessions. When SessionScoped, only the
// calling session's sandboxes are returned.
func (m *Manager) List(tenant, session string) []Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Session
	for _, s := range m.sessions {
		if s.Tenant != tenant {
			continue
		}
		if m.cfg.SessionScoped && s.Session != session {
			continue
		}
		out = append(out, *s)
	}
	return out
}

// Close removes the sandbox. It is idempotent and never reveals existence:
// an unknown, foreign-tenant, or (when SessionScoped) foreign-session id
// returns nil.
func (m *Manager) Close(ctx context.Context, tenant, session, id string) error {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if !ok || s.Tenant != tenant || (m.cfg.SessionScoped && s.Session != session) {
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

// CloseSession removes every sandbox owned by (tenant, session). It is a no-op
// when scope=tenant — session-end teardown must never tear down tenant-shared
// boxes. Idempotent; never reveals existence.
func (m *Manager) CloseSession(ctx context.Context, tenant, session string) error {
	if !m.cfg.SessionScoped {
		return nil
	}
	m.mu.Lock()
	var victims []*Session
	for id, s := range m.sessions {
		if s.Tenant == tenant && s.Session == session {
			victims = append(victims, s)
			delete(m.sessions, id)
		}
	}
	m.mu.Unlock()
	for _, s := range victims {
		if err := m.be.Remove(ctx, s.ContainerID); err != nil {
			slog.Warn("sandbox close-session: container remove failed",
				"sandbox_id", s.ID, "tenant", tenant, "session", session,
				"container_id", s.ContainerID, "err", err)
		}
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
