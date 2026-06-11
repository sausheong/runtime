package gateway

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sausheong/harness/tool"

	"github.com/sausheong/runtime/internal/config"
	"github.com/sausheong/runtime/internal/obs"
)

// UpstreamStatus is the operator-facing state of one upstream.
type UpstreamStatus struct {
	Name        string    `json:"name"`
	Transport   string    `json:"transport"` // "stdio" | "http" | "openapi"
	State       string    `json:"state"`     // "up" | "down"
	ToolCount   int       `json:"tool_count"`
	LastError   string    `json:"last_error,omitempty"`
	ConnectedAt time.Time `json:"connected_at,omitzero"`
}

// upstream is one configured server plus its live connection state.
type upstream struct {
	cfg config.GatewayServer

	mu          sync.Mutex
	conn        upstreamConn
	tools       []tool.Tool // renamed view (gateway names), nil when down
	lastErr     error
	connectedAt time.Time
}

// Manager owns the configured upstreams. Start launches one supervision
// goroutine per upstream (connect → on failure retry with capped backoff).
// All read methods are safe for concurrent use.
type Manager struct {
	ups        []*upstream
	dial       dialFunc
	minBackoff time.Duration
	maxBackoff time.Duration

	generation atomic.Uint64
	wg         sync.WaitGroup
	cancel     context.CancelFunc // set by Start; called by Close

	// Metrics (nil-safe) tracks upstream connection state. Set before Start.
	Metrics *obs.ControlMetrics
}

// Option configures a Manager.
type Option func(*Manager)

// WithDial overrides the production dialer (tests).
func WithDial(d dialFunc) Option { return func(m *Manager) { m.dial = d } }

// WithBackoff overrides retry pacing (tests).
func WithBackoff(min, max time.Duration) Option {
	return func(m *Manager) { m.minBackoff, m.maxBackoff = min, max }
}

// NewManager builds a Manager for the configured servers. Call Start to begin
// connecting.
func NewManager(servers []config.GatewayServer, opts ...Option) *Manager {
	m := &Manager{dial: dialProduction, minBackoff: time.Second, maxBackoff: time.Minute}
	for _, s := range servers {
		m.ups = append(m.ups, &upstream{cfg: s})
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Start launches the supervision loops. Non-blocking; safe to call once.
// The loops run until the given context is cancelled or Close is called
// (Start derives its own cancellable context so Close never deadlocks).
func (m *Manager) Start(ctx context.Context) {
	ctx, m.cancel = context.WithCancel(ctx)
	for _, u := range m.ups {
		m.wg.Add(1)
		go m.supervise(ctx, u)
	}
}

// pingTimeout bounds each supervise-loop health ping. 5s is generous for a
// no-op MCP ping yet short enough that a hung upstream is detected within a
// few poll cycles (the loop pings every minBackoff).
const pingTimeout = 5 * time.Second

// supervise keeps one upstream connected: dial with capped exponential
// backoff, mark up, then health-check on a minBackoff cadence — a failed
// ping (crashed stdio child, restarted HTTP upstream) calls markDown so
// the next iteration redials. This loop is markDown's production caller;
// any other markDown source (e.g. a future tool-path failure report) is
// also picked up here because the loop redials whenever conn==nil.
func (m *Manager) supervise(ctx context.Context, u *upstream) {
	defer m.wg.Done()
	backoff := m.minBackoff
	for {
		if ctx.Err() != nil {
			return
		}
		u.mu.Lock()
		connected := u.conn != nil
		u.mu.Unlock()
		if !connected {
			conn, err := m.dial(ctx, u.cfg)
			if err != nil {
				u.mu.Lock()
				u.lastErr = err
				u.mu.Unlock()
				m.Metrics.GatewayUpstreamUp(u.cfg.Name, false)
				slog.Warn("gateway: upstream connect failed",
					"server", u.cfg.Name, "transport", transportOf(u.cfg), "err", err)
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff + time.Duration(rand.Int64N(int64(backoff/2+1)))):
				}
				backoff = min(backoff*2, m.maxBackoff)
				continue
			}
			renamed := renameTools(conn.Tools())
			u.mu.Lock()
			u.conn, u.tools, u.lastErr, u.connectedAt = conn, renamed, nil, time.Now()
			u.mu.Unlock()
			m.generation.Add(1)
			backoff = m.minBackoff
			m.Metrics.GatewayUpstreamUp(u.cfg.Name, true)
			slog.Info("gateway: upstream connected",
				"server", u.cfg.Name, "transport", transportOf(u.cfg), "tools", len(renamed))
		}
		// Sleep, then health-check. The sleep doubles as the poll for
		// markDown from the tool execution path (cheap; avoids threading a
		// notification channel through it).
		select {
		case <-ctx.Done():
			return
		case <-time.After(m.minBackoff):
		}
		u.mu.Lock()
		conn := u.conn
		u.mu.Unlock()
		if conn != nil {
			// Ping OUTSIDE the lock: it does I/O and may block up to
			// pingTimeout. markDown's conn-identity check makes a stale
			// ping result harmless if the conn was replaced meanwhile.
			pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
			err := conn.Ping(pingCtx)
			cancel()
			if err != nil && ctx.Err() == nil {
				m.markDown(u, conn, err)
			}
		}
	}
}

// markDown records a mid-flight failure observed on a specific connection:
// it closes and clears the connection so the supervision loop redials — but
// only if that connection is still the current one. A stale report (the
// supervise loop already replaced the conn) is a no-op, so one upstream
// outage cannot cascade into closing its healthy replacement.
func (m *Manager) markDown(u *upstream, observed upstreamConn, err error) {
	u.mu.Lock()
	if u.conn == nil || u.conn != observed {
		u.mu.Unlock()
		return
	}
	conn := u.conn
	u.conn, u.tools, u.lastErr = nil, nil, err
	u.mu.Unlock()
	// Gauge BEFORE conn.Close(): Close may block on I/O, and a delayed Set(0)
	// from a concurrent caller could otherwise land after the supervise loop
	// has already reconnected and Set(1), wedging the gauge at 0 while up.
	m.Metrics.GatewayUpstreamUp(u.cfg.Name, false)
	_ = conn.Close() // outside the lock: Close may block on I/O
	m.generation.Add(1)
	slog.Warn("gateway: upstream marked down", "server", u.cfg.Name, "err", err)
}

// Close stops the supervision loops (cancelling the context derived in
// Start) and tears down all connections. Safe to call whether or not the
// caller has cancelled the Start context; safe when Start was never called.
// The upstream-up gauge is deliberately left untouched here: the process is
// exiting and scrapes have already stopped.
func (m *Manager) Close() {
	if m.cancel != nil {
		m.cancel()
	}
	m.wg.Wait()
	for _, u := range m.ups {
		u.mu.Lock()
		conn := u.conn
		u.conn = nil
		u.mu.Unlock()
		if conn != nil {
			_ = conn.Close() // outside the lock: Close may block on I/O
		}
	}
}

// Generation increments whenever the federated tool set may have changed
// (connect, reconnect, down). Server caches key on it.
func (m *Manager) Generation() uint64 { return m.generation.Load() }

// noneTenant is the impossible tenant filter viewKey produces for a
// non-superuser principal with an empty TenantID. It matches no upstream at
// all — not even ones with an empty Tenants list, which are otherwise
// visible to every real tenant.
const noneTenant = "\x00none"

// visibleTo reports whether an upstream is visible to tenant. Empty Tenants ⇒
// visible to all. The empty tenant ("") means the unscoped view (superuser or
// open mode) and sees everything.
func visibleTo(s config.GatewayServer, tenant string) bool {
	if tenant == noneTenant {
		return false
	}
	if tenant == "" || len(s.Tenants) == 0 {
		return true
	}
	for _, t := range s.Tenants {
		if t == tenant {
			return true
		}
	}
	return false
}

// ToolsFor returns the live tools visible to tenant.
func (m *Manager) ToolsFor(tenant string) []tool.Tool {
	var out []tool.Tool
	for _, u := range m.ups {
		if !visibleTo(u.cfg, tenant) {
			continue
		}
		u.mu.Lock()
		out = append(out, u.tools...)
		u.mu.Unlock()
	}
	return out
}

// AllTools is the unscoped view (open mode / superuser).
func (m *Manager) AllTools() []tool.Tool { return m.ToolsFor("") }

// ForwardsTenant reports whether the upstream serving the given gateway tool
// name (<server>__<tool>) has forward_tenant configured. Names without the
// "__" separator (e.g. search_tools) never forward.
func (m *Manager) ForwardsTenant(toolName string) bool {
	srv, _, ok := strings.Cut(toolName, "__")
	if !ok {
		return false
	}
	for _, u := range m.ups {
		if u.cfg.Name == srv {
			return u.cfg.ForwardTenant
		}
	}
	return false
}

// Status returns per-upstream state. tenant=="" ⇒ unscoped (all upstreams);
// otherwise only upstreams visible to that tenant.
func (m *Manager) Status(tenant string) []UpstreamStatus {
	var out []UpstreamStatus
	for _, u := range m.ups {
		if !visibleTo(u.cfg, tenant) {
			continue
		}
		u.mu.Lock()
		st := UpstreamStatus{
			Name:      u.cfg.Name,
			Transport: transportOf(u.cfg),
			State:     "down",
			ToolCount: len(u.tools),
		}
		if u.conn != nil {
			st.State = "up"
			st.ConnectedAt = u.connectedAt
		}
		if u.lastErr != nil {
			st.LastError = u.lastErr.Error()
		}
		u.mu.Unlock()
		out = append(out, st)
	}
	return out
}

func transportOf(s config.GatewayServer) string {
	if s.Command != "" {
		return "stdio"
	}
	if s.OpenAPI != "" {
		return "openapi"
	}
	return "http"
}

// renameTools wraps each harness-adapted tool so its gateway-facing name is
// "<server>__<tool>" instead of the adapter's "mcp__<server>__<tool>". The
// consuming harness client prepends its own "mcp__gateway__", so stripping
// here avoids a double prefix. Names not following the adapter convention
// pass through unchanged (TrimPrefix is a no-op).
func renameTools(ts []tool.Tool) []tool.Tool {
	out := make([]tool.Tool, 0, len(ts))
	for _, t := range ts {
		out = append(out, renamedTool{Tool: t, name: strings.TrimPrefix(t.Name(), "mcp__")})
	}
	return out
}

// renamedTool overrides only Name; everything else delegates.
type renamedTool struct {
	tool.Tool
	name string
}

func (r renamedTool) Name() string { return r.name }
