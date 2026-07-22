package browser

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
// nonexistent id are indistinguishable (existence hidden, Identity M1 posture).
var errNoSandbox = errors.New("no such browser")

// Config bounds Manager behavior; zero/invalid fields get defaults.
type Config struct {
	MaxPerTenant int           // concurrent browsers per tenant (default 5)
	IdleTTL      time.Duration // close after this long unused (default 10m)
	MaxLifetime  time.Duration // close this long after create (default 1h)
	ProxyAddr    string        // egress proxy address passed to Backend.Create

	// SessionScoped keys browsers by (tenant, session, id) instead of
	// (tenant, id): a handle minted in one agent session is invisible to
	// other sessions of the same tenant. Default false (tenant-scoped).
	SessionScoped bool
}

// Session is one live browser. The chromedp context fields are populated lazily
// by the action layer (a later task); the fake backend leaves them nil.
type Session struct {
	ID          string
	Tenant      string
	Session     string // owning agent session; "" in tenant-scoped mode
	ContainerID string
	Endpoint    string
	CreatedAt   time.Time
	LastUsed    time.Time
	ExpiresAt   time.Time
	CurrentURL  string

	mu      sync.Mutex         // serializes chromedp actions (one tab)
	taskCtx context.Context    // chromedp task ctx (later task)
	cancel  context.CancelFunc // tears down both (later task)
}

// Manager owns the browser sessions over a Backend. Mirrors the M1 sandbox
// Manager contract.
type Manager struct {
	be  Backend
	cfg Config
	now func() time.Time

	mu       sync.Mutex
	sessions map[string]*Session
}

// NewManager builds a Manager over be, applying defaults for zero fields.
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
	return &Manager{be: be, cfg: cfg, now: time.Now, sessions: map[string]*Session{}}
}

// newBrowserID returns "brw-" + 32 hex chars from 16 random bytes.
func newBrowserID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("browser: crypto/rand failed: %v", err))
	}
	return "brw-" + hex.EncodeToString(b[:])
}

// Create starts a new browser for tenant, enforcing the per-tenant cap with a
// slot reservation under lock (identical discipline to M1).
func (m *Manager) Create(ctx context.Context, tenant, session string) (*Session, error) {
	now := m.now()
	s := &Session{
		ID:        newBrowserID(),
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
		return nil, fmt.Errorf("browser limit reached (%d per tenant): close one with close_browser", m.cfg.MaxPerTenant)
	}
	m.sessions[s.ID] = s // reservation
	m.mu.Unlock()

	h, err := m.be.Create(ctx, tenant, m.cfg.ProxyAddr)
	if err != nil {
		m.mu.Lock()
		delete(m.sessions, s.ID)
		m.mu.Unlock()
		return nil, fmt.Errorf("browser backend unavailable: %w", err)
	}

	m.mu.Lock()
	if _, ok := m.sessions[s.ID]; !ok {
		m.mu.Unlock()
		if rmErr := m.be.Remove(ctx, h.ContainerID); rmErr != nil {
			slog.Warn("browser create: container remove after lost reservation failed",
				"browser_id", s.ID, "container_id", h.ContainerID, "err", rmErr)
		}
		return nil, errNoSandbox
	}
	s.ContainerID = h.ContainerID
	s.Endpoint = h.Endpoint
	m.mu.Unlock()
	return s, nil
}

// Lookup resolves a browser id for tenant, touching LastUsed. A missing id and
// a foreign tenant's id return the identical errNoSandbox. When SessionScoped,
// a foreign session's id is hidden identically to nonexistent.
func (m *Manager) Lookup(tenant, session, id string) (*Session, error) {
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

// maskNav scrubs action errors: a vanished session → errNoSandbox;
// a still-live session's error passes through (selector/egress/JS errors are
// user-actionable and leak nothing).
func (m *Manager) maskNav(id string, err error) error {
	if err == nil {
		return nil
	}
	m.mu.Lock()
	_, ok := m.sessions[id]
	m.mu.Unlock()
	if !ok {
		return errNoSandbox
	}
	return err
}

// List returns copies of tenant's live sessions (without the unexported fields).
// When SessionScoped, only the calling session's browsers are returned.
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
		out = append(out, Session{
			ID: s.ID, Tenant: s.Tenant, Session: s.Session, ContainerID: s.ContainerID,
			CreatedAt: s.CreatedAt, LastUsed: s.LastUsed, ExpiresAt: s.ExpiresAt,
			CurrentURL: s.CurrentURL,
		})
	}
	return out
}

// Close removes the browser. It is idempotent and never reveals existence:
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
	if s.cancel != nil {
		s.cancel()
	}
	if err := m.be.Remove(ctx, s.ContainerID); err != nil {
		slog.Warn("browser close: container remove failed",
			"browser_id", s.ID, "container_id", s.ContainerID, "err", err)
	}
	return nil
}

// CloseSession removes every browser owned by (tenant, session). It is a no-op
// when scope=tenant — session-end teardown must never tear down tenant-shared
// browsers. Idempotent; never reveals existence.
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
		if s.cancel != nil {
			s.cancel()
		}
		if err := m.be.Remove(ctx, s.ContainerID); err != nil {
			slog.Warn("browser close-session: container remove failed",
				"browser_id", s.ID, "tenant", tenant, "session", session,
				"container_id", s.ContainerID, "err", err)
		}
	}
	return nil
}

// ReapOnce closes every session idle past IdleTTL or past its max lifetime.
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
		if s.cancel != nil {
			s.cancel()
		}
		if err := m.be.Remove(ctx, s.ContainerID); err != nil {
			slog.Warn("browser reap: container remove failed",
				"browser_id", s.ID, "container_id", s.ContainerID, "err", err)
			continue
		}
		slog.Info("browser reaped", "browser_id", s.ID, "tenant", s.Tenant)
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

// ReapStartup removes all leftover browser containers (crash recovery).
func (m *Manager) ReapStartup(ctx context.Context) error {
	ids, err := m.be.ListLeftovers(ctx)
	if err != nil {
		return fmt.Errorf("list leftover browsers: %w", err)
	}
	for _, id := range ids {
		if err := m.be.Remove(ctx, id); err != nil {
			slog.Warn("startup reap: container remove failed", "container_id", id, "err", err)
		}
	}
	return nil
}
