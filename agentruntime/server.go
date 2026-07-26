package agentruntime

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/sausheong/runtime/internal/obs"
	"github.com/sausheong/runtime/internal/rheader"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// readForwardedIdentity reads the caller's forwarded identity trio
// (X-Runtime-{User,Tenant,Role}) set by the control-plane edge. It is gated by
// the agent-side RUNTIME_SUBJECT_FORWARDING flag: when off it returns empty
// strings regardless of any inbound headers, so isolation depends on ON.
func readForwardedIdentity(r *http.Request, on bool) (subject, tenant, role string) {
	if !on {
		return "", "", ""
	}
	return r.Header.Get(rheader.User), r.Header.Get(rheader.Tenant), r.Header.Get(rheader.Role)
}

// readAssertion reads the caller's forwarded verified OIDC JWT
// (X-Runtime-Assertion) set by the control-plane edge for OBO. Gated by the same
// RUNTIME_SUBJECT_FORWARDING flag as the identity trio: off ⇒ "" regardless of
// any inbound header. The JWT is a bearer secret — request-scoped only, never
// checkpointed, never logged.
func readAssertion(r *http.Request, on bool) string {
	if !on {
		return ""
	}
	return r.Header.Get(rheader.Assertion)
}

func (m *Manager) tenantID() string {
	if m.tenant == "" {
		return "default"
	}
	return m.tenant
}

// maxSessionBodyBytes bounds the only agent-contract request that can carry
// inline binary data. Sixteen MiB leaves room for a roughly 12 MiB source image
// after base64 expansion while preventing an unauthenticated/local agent port
// from buffering an arbitrarily large JSON body.
const maxSessionBodyBytes int64 = 16 << 20

const signedNonceCapacity = 131072

// nonceReplayCache keeps two fixed-duration buckets. Lookup and insertion are
// constant-time; rotating a bucket drops the whole expired map without scanning
// entries on the request path. At most maxEntries authenticated nonces are
// retained, and saturation fails closed.
type nonceReplayCache struct {
	mu          sync.Mutex
	current     map[string]struct{}
	previous    map[string]struct{}
	currentFrom time.Time
	maxEntries  int
}

func newNonceReplayCache(maxEntries int) *nonceReplayCache {
	if maxEntries < 1 {
		maxEntries = signedNonceCapacity
	}
	return &nonceReplayCache{
		current:    make(map[string]struct{}),
		previous:   make(map[string]struct{}),
		maxEntries: maxEntries,
	}
}

func (c *nonceReplayCache) accept(nonce string, now time.Time, window time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.currentFrom.IsZero() {
		c.currentFrom = now
	} else {
		elapsed := now.Sub(c.currentFrom)
		switch {
		case elapsed >= 2*window:
			c.current = make(map[string]struct{})
			c.previous = make(map[string]struct{})
			c.currentFrom = now
		case elapsed >= window:
			c.previous = c.current
			c.current = make(map[string]struct{})
			c.currentFrom = now
		}
	}
	if _, exists := c.current[nonce]; exists {
		return false
	}
	if _, exists := c.previous[nonce]; exists {
		return false
	}
	if len(c.current)+len(c.previous) >= c.maxEntries {
		return false
	}
	c.current[nonce] = struct{}{}
	return true
}

// handler is the full agentd HTTP stack, outermost to innermost:
// RequestID (mutates r.Header, so nothing may observe the request first) →
// requireBearer (only when an auth token is set; a 401 short-circuits before
// any span) → otelhttp server span (continues an inbound traceparent, named by
// matched route) → access log (skips the probe paths /healthz and /metrics) →
// the route mux.
func (m *Manager) handler() http.Handler {
	mux := m.newMux()
	logged := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" && r.URL.Path != "/metrics" {
			slog.Info("http",
				"method", r.Method,
				"path", r.URL.Path,
				"request_id", obs.RequestIDFromContext(r.Context()))
		}
		mux.ServeHTTP(w, r)
	})
	var h http.Handler = logged
	// otelhttp server span: continues an inbound traceparent (parent) so the
	// agentd work nests under runtimed's trace. Named by route, not raw path.
	h = otelhttp.NewHandler(h, "agentd.request",
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			if r.Pattern != "" {
				return r.Method + " " + r.Pattern
			}
			return r.Method
		}),
	)
	if m.authToken != "" {
		if m.subjectForwarding {
			h = requireSignedIdentity(m.identityVerifyKey, h)
		}
		h = requireBearer(m.authToken, h)
	}
	h = m.limitRequests(h)
	return obs.RequestID(h)
}

func (m *Manager) limitRequests(next http.Handler) http.Handler {
	if m.requestSem == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case m.requestSem <- struct{}{}:
			defer func() { <-m.requestSem }()
			next.ServeHTTP(w, r)
		default:
			m.metrics.HTTPRejected("requests")
			http.Error(w, "server busy", http.StatusServiceUnavailable)
		}
	})
}

func (m *Manager) acquireStream() (func(), bool) {
	if m.streamSem == nil {
		return func() {}, true
	}
	select {
	case m.streamSem <- struct{}{}:
		return func() { <-m.streamSem }, true
	default:
		m.metrics.HTTPRejected("streams")
		return nil, false
	}
}

func requireSignedIdentity(publicKey string, next http.Handler) http.Handler {
	replays := newNonceReplayCache(signedNonceCapacity)
	const skew = 30 * time.Second
	// Verify accepts timestamps on either side of the local clock. Retain each
	// nonce for at least twice the skew so a request first seen with a future
	// timestamp cannot become replayable while its signature is still valid.
	const replayWindow = 2 * skew
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && (r.URL.Path == "/healthz" || r.URL.Path == "/readyz") {
			next.ServeHTTP(w, r)
			return
		}
		now := time.Now()
		nonce, err := rheader.Verify(r, publicKey, now, skew)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !replays.accept(nonce, now, replayWindow) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireBearer rejects any request whose Authorization header is not exactly
// "Bearer <token>" with 401, using a constant-time compare. It guards every
// path EXCEPT "GET /healthz", which is exempt: K8s liveness/readiness probes hit
// it with no Authorization header, and it returns a static "ok" with zero data
// (an unauthenticated handler also harmlessly ignores any bearer runtimed's own
// C3 M1 health checks still send). /metrics is NOT exempt — it exposes per-agent
// metric values, so it stays guarded.
func requireBearer(token string, next http.Handler) http.Handler {
	want := "Bearer " + token
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && (r.URL.Path == "/healthz" || r.URL.Path == "/readyz") {
			next.ServeHTTP(w, r)
			return
		}
		got := r.Header.Get("Authorization")
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (m *Manager) newMux() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := m.st.Ping(ctx); err != nil {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	})
	mux.Handle("GET /metrics", m.metrics.Handler())
	mux.HandleFunc("GET /meta", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"agent_id": m.agentID, "contract_version": "v1",
		})
	})
	mux.HandleFunc("POST /sessions", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxSessionBodyBytes)
		var body struct {
			Message   string `json:"message"`
			ImageB64  string `json:"image_b64"`
			ImageMime string `json:"image_mime"`
		}
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		subject, tenant, role := readForwardedIdentity(r, m.subjectForwarding)
		assertion := readAssertion(r, m.subjectForwarding)
		id, err := m.startSession(r.Context(), body.Message, body.ImageB64, body.ImageMime, obs.RequestIDFromContext(r.Context()), subject, tenant, role, assertion)
		if err != nil {
			slog.Error("start session failed", "agent", m.agentID, "err", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"session_id": id})
	})
	mux.HandleFunc("GET /sessions", func(w http.ResponseWriter, r *http.Request) {
		rows, err := m.st.ListSessionsForTenant(r.Context(), m.tenantID(), m.agentID)
		if err != nil {
			slog.Error("list sessions failed", "agent", m.agentID, "err", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		type sessOut struct {
			ID          string  `json:"id"`
			Status      string  `json:"status"`
			TurnCount   int     `json:"turn_count"`
			TokensTotal int64   `json:"tokens_total"`
			CostUSD     float64 `json:"cost_usd"`
		}
		out := make([]sessOut, 0, len(rows))
		for _, s := range rows {
			out = append(out, sessOut{ID: s.ID, Status: s.Status, TurnCount: s.TurnCount,
				TokensTotal: s.TokensTotal, CostUSD: s.CostUSD})
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("GET /sessions/{id}/stream", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !m.requireOwnedSession(w, r, id) {
			return
		}
		release, ok := m.acquireStream()
		if !ok {
			http.Error(w, "too many streams", http.StatusServiceUnavailable)
			return
		}
		defer release()
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		var since int64
		if s := r.URL.Query().Get("since"); s != "" {
			since, _ = strconv.ParseInt(s, 10, 64)
		}

		live, unsub := m.subscribe(id)
		defer unsub()

		buffered, err := m.st.EventsSince(r.Context(), id, since)
		if err != nil {
			http.Error(w, "event replay unavailable", http.StatusServiceUnavailable)
			return
		}
		lastSent := since
		for _, e := range buffered {
			var ev WireEvent
			if json.Unmarshal(e.Payload, &ev) == nil {
				ev.Seq = e.Seq
				_ = writeSSE(w, ev)
				lastSent = e.Seq
			}
		}
		flusher.Flush()
		if n := len(buffered); n > 0 && (buffered[n-1].Type == "done" || buffered[n-1].Type == "error") {
			return // pure-replay terminal: stream already complete
		}

		for {
			select {
			case <-r.Context().Done():
				return
			case ev := <-live:
				// We subscribe before replay so no event can be missed. An
				// event committed during the replay query may therefore appear
				// in both buffered and live; sequence filtering removes that
				// deliberate overlap.
				if ev.Seq <= lastSent {
					continue
				}
				_ = writeSSE(w, ev)
				lastSent = ev.Seq
				flusher.Flush()
				if ev.Type == "done" || ev.Type == "error" {
					return
				}
			}
		}
	})
	mux.HandleFunc("GET /sessions/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !m.requireOwnedSession(w, r, id) {
			return
		}
		var since int64
		if s := r.URL.Query().Get("since"); s != "" {
			since, _ = strconv.ParseInt(s, 10, 64)
		}
		limit := 50
		if s := r.URL.Query().Get("limit"); s != "" {
			if n, err := strconv.Atoi(s); err == nil && n > 0 {
				limit = n
			}
		}
		if limit > 1000 {
			limit = 1000
		}
		// Non-blocking, unlike the SSE stream: a pure read of stored events. A
		// non-terminal session returns whatever is buffered so far and returns
		// immediately (no subscribe).
		evs, err := m.st.EventsSince(r.Context(), id, since)
		if err != nil {
			slog.Error("list session events failed", "agent", m.agentID, "session", id, "err", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		if len(evs) > limit {
			evs = evs[len(evs)-limit:] // events are seq-ascending; keep the most recent
		}
		type evOut struct {
			Seq  int64  `json:"seq"`
			Type string `json:"type"`
			Text string `json:"text,omitempty"`
			Err  string `json:"error,omitempty"`
		}
		out := make([]evOut, 0, len(evs))
		for _, e := range evs {
			var ev WireEvent
			_ = json.Unmarshal(e.Payload, &ev) // bad/empty payload → empty fields, never a 500
			out = append(out, evOut{Seq: e.Seq, Type: ev.Type, Text: ev.Text, Err: ev.Err})
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("GET /sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		row, err := m.st.GetSession(r.Context(), r.PathValue("id"))
		if err != nil || row.AgentID != m.agentID || row.TenantID != m.tenantID() {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": row.ID, "status": row.Status, "turn_count": row.TurnCount,
			"tokens_total": row.TokensTotal, "cost_usd": row.CostUSD,
		})
	})
	return mux
}

// requireOwnedSession enforces the agent boundary again inside agentd. The
// control plane performs the same check before proxying native sessions, but an
// agent port may also be reachable directly on a trusted host. Session ids are
// identifiers, not bearer capabilities.
func (m *Manager) requireOwnedSession(w http.ResponseWriter, r *http.Request, id string) bool {
	row, err := m.st.GetSession(r.Context(), id)
	if err != nil || row.AgentID != m.agentID || row.TenantID != m.tenantID() {
		http.Error(w, "session not found", http.StatusNotFound)
		return false
	}
	return true
}
