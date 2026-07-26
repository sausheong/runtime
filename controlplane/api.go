package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"github.com/sausheong/runtime/internal/identity"
	"github.com/sausheong/runtime/internal/obs"
	"github.com/sausheong/runtime/internal/rheader"
	"github.com/sausheong/runtime/internal/store"
	"github.com/sausheong/runtime/internal/xfan"
)

// stripRuntimeHeaders deletes every inbound header under the reserved
// X-Runtime- prefix. Callers must never smuggle a platform claim past the
// proxy; the platform sets its own afterward.
func stripRuntimeHeaders(h http.Header) {
	for k := range h {
		if strings.HasPrefix(http.CanonicalHeaderKey(k), rheader.Prefix) {
			h.Del(k)
		}
	}
}

// forwardSubject implements the anti-spoof strip-then-set for the caller's
// authenticated identity. When forwarding is off it is a no-op (today's
// behavior). When on it ALWAYS strips inbound X-Runtime-*, then sets the trio
// from the request's Principal when one is present with a non-empty value.
func forwardSubject(r *http.Request, forwarding bool) {
	if !forwarding {
		return
	}
	stripRuntimeHeaders(r.Header)
	p, ok := PrincipalFromContext(r.Context())
	if !ok {
		return
	}
	if p.Subject != "" {
		r.Header.Set(rheader.User, p.Subject)
	}
	if p.TenantID != "" {
		r.Header.Set(rheader.Tenant, p.TenantID)
	}
	if p.Role != "" {
		r.Header.Set(rheader.Role, string(p.Role))
	}
	if jwt := identity.AssertionFrom(r.Context()); jwt != "" {
		r.Header.Set(rheader.Assertion, jwt)
	}
}

// NewAPI returns the control-plane HTTP handler routing /agents/{id}/... to each
// agent's replica pool, plus GET /agents and GET /healthz. New sessions
// round-robin across replicas; session-scoped requests pin to the owning replica
// (resolved from st); replica-agnostic paths use replica 0. m records
// proxy-error metrics; nil ⇒ no-op. st resolves session→replica affinity and is
// REQUIRED (non-nil); unlike m it is not nil-safe (pickReplica dereferences it).
func NewAPI(reg *Registry, m *obs.ControlMetrics, st store.Store, subjectForwarding bool) *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := st.Ping(ctx); err != nil {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	})

	mux.HandleFunc("GET /agents", func(w http.ResponseWriter, r *http.Request) {
		type agentStatus struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Model   string `json:"model"`
			Healthy bool   `json:"healthy"`
		}
		p, hasP := PrincipalFromContext(r.Context())
		infos := reg.List()
		type agentTarget struct {
			info     AgentInfo
			replicas []AgentProcess
		}
		// Filter first, then fan out over the survivors: the ceiling applies to
		// the probes actually issued.
		var targets []agentTarget
		for _, info := range infos {
			if hasP && !p.Superuser && info.Tenant != p.TenantID {
				continue
			}
			if reg.Disabled(info.ID) {
				continue // administratively disabled: hidden from the public listing
			}
			replicas, _ := reg.Replicas(info.ID)
			targets = append(targets, agentTarget{info: info, replicas: replicas})
		}
		// Pre-sized and index-disjoint: no mutex, and the response order is
		// deterministic (registry order) rather than completion order.
		out := make([]agentStatus, len(targets))
		xfan.Each(r.Context(), len(targets), xfan.DefaultLimit, func(ctx context.Context, i int) {
			t := targets[i]
			st := agentStatus{ID: t.info.ID, Name: t.info.Name, Model: t.info.Model}
			// An agent is healthy if ANY replica answers /healthz.
			for _, ap := range t.replicas {
				client := NewAgentHTTPClient(ap, time.Second)
				req, err := http.NewRequestWithContext(ctx, "GET", ap.baseURL()+"/healthz", nil)
				if err != nil {
					continue
				}
				resp, err := client.Do(req)
				if err == nil {
					ok := resp.StatusCode == 200
					resp.Body.Close()
					if ok {
						st.Healthy = true
						break
					}
				}
			}
			out[i] = st
		})
		// make() above never yields nil, so this only documents the contract:
		// the body is a JSON array even when no agent is visible, never null.
		if out == nil {
			out = []agentStatus{}
		}
		_ = json.NewEncoder(w).Encode(out)
	})

	// Subtree pattern: /agents/{id}/sessions, /agents/{id}/healthz, etc.
	mux.HandleFunc("/agents/{id}/", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if _, ok := reg.Get(id); !ok {
			http.Error(w, "unknown agent "+id, http.StatusNotFound)
			return
		}
		if reg.Disabled(id) {
			// Administratively disabled: the agent process may still be up, but
			// the control plane does not route to it. 404, same as an unknown id.
			http.Error(w, "unknown agent "+id, http.StatusNotFound)
			return
		}
		prefix := "/agents/" + id
		r.URL.Path = strings.TrimPrefix(r.URL.Path, prefix)
		if r.URL.Path == "" {
			r.URL.Path = "/"
		}
		r.URL.RawPath = "" // avoid stale encoded-path mismatches after rewrite

		ap, routeErr := pickReplica(r, reg, st, id)
		if routeErr != nil {
			if errors.Is(routeErr, store.ErrSessionNotFound) {
				http.Error(w, "unknown session", http.StatusNotFound)
			} else {
				if errors.Is(routeErr, errSessionRoutingStore) {
					m.RoutingStoreError(id)
				}
				slog.Warn("session routing unavailable", "agent", id, "err", routeErr)
				http.Error(w, "session routing unavailable", http.StatusServiceUnavailable)
			}
			return
		}
		// The identity middleware and this handler deliberately do not share a
		// mutable registry lookup. Re-authorize against the exact immutable
		// AgentProcess snapshot selected for forwarding so delete/recreate or
		// tenant reassignment cannot turn a stale authorization into access to a
		// replacement agent.
		if p, ok := PrincipalFromContext(r.Context()); ok {
			if err := authorizeSelectedAgent(p, ap, actionForRequest(r.Method, "/agents/"+id+r.URL.Path)); err != nil {
				status := authzStatus(err)
				http.Error(w, authzMessage(status), status)
				return
			}
		}
		releaseReplica, leased := reg.LeaseReplica(
			ap, r.Method == http.MethodPost && r.URL.Path == "/sessions")
		if !leased {
			http.Error(w, "session routing unavailable", http.StatusServiceUnavailable)
			return
		}
		defer releaseReplica()
		m.ProxyCall(id, proxyKind(r.Method, r.URL.Path))
		forwardSubject(r, subjectForwarding)
		if subjectForwarding {
			if err := rheader.Sign(r, ap.IdentitySigningPrivateKey, time.Now()); err != nil {
				slog.Error("sign forwarded identity", "agent", id, "err", err)
				http.Error(w, "agent identity forwarding unavailable", http.StatusServiceUnavailable)
				return
			}
		}
		rp := reverseProxyWithTransport(ap.baseURL(), ap.AuthToken, agentOutboundTransport(ap), func() { m.ProxyError(id) })
		if r.Method == http.MethodPost && r.URL.Path == "/sessions" && (ap.Remote || len(ap.Command) > 0) {
			addSessionBinding(rp, st, ap)
		}
		rp.ServeHTTP(w, r)
	})

	return mux
}

func authorizeSelectedAgent(p identity.Principal, ap AgentProcess, action identity.Action) error {
	az := identity.NewAuthorizer(map[string]string{ap.AgentID: ap.Tenant})
	return az.Authorize(p, ap.AgentID, action)
}

// proxyKind classifies a prefix-stripped agent request path+method into the
// obs.Proxy* kind label. Mirrors the routing intent in pickReplica.
func proxyKind(method, path string) string {
	if method == "POST" && path == "/sessions" {
		return obs.ProxyNewSession
	}
	if _, ok := sessionID(path); ok {
		if strings.HasSuffix(path, "/messages") {
			return obs.ProxyMessage
		}
		if strings.HasSuffix(path, "/stream") {
			return obs.ProxyStream
		}
	}
	return obs.ProxyOther
}

// pickReplica chooses which replica serves this (already-prefix-stripped)
// request:
//   - POST /sessions (exactly)         → round-robin a new session
//   - /sessions/{sid}[/...]            → pin to the owner replica (from st)
//   - everything else (list, healthz)  → replica 0 (agent-level, replica-agnostic)
var errSessionOwnerUnavailable = errors.New("session owner replica unavailable")
var errSessionRoutingStore = errors.New("session routing store unavailable")

// A missing session is returned as store.ErrSessionNotFound. Persistence
// failures and unavailable owner replicas are returned distinctly so callers
// can serve 503 rather than turning an outage into a misleading 404.
func pickReplica(r *http.Request, reg *Registry, st store.Store, id string) (AgentProcess, error) {
	path := r.URL.Path
	if r.Method == "POST" && path == "/sessions" {
		ap, ok := reg.Replica(id, reg.NextReplica(id))
		if !ok {
			return AgentProcess{}, errSessionOwnerUnavailable
		}
		return ap, nil
	}
	if sid, ok := sessionID(path); ok {
		session, err := st.GetSession(r.Context(), sid)
		if err != nil {
			if !errors.Is(err, store.ErrSessionNotFound) {
				return AgentProcess{}, fmt.Errorf("%w: get session affinity: %w", errSessionRoutingStore, err)
			}
			// Historical external sessions without a durable binding cannot
			// prove tenant, agent generation, or replica ownership. Fail closed
			// even for a single replica: silently asking the current endpoint
			// lets ID/endpoint reuse inherit another instance's session.
			return AgentProcess{}, fmt.Errorf("%w: %q", store.ErrSessionNotFound, sid)
		}
		// Authentication authorizes the agent named in the URL. A session id is
		// not an authority token: it must also belong to that exact agent before
		// its replica affinity may influence routing. Without this check, a known
		// session id from agent B could be read through an authorized path for A.
		// A known session whose stored owner index is now out of range (e.g. the
		// agent was reconfigured to fewer replicas than when this session was
		// created) is unroutable: only that original executor can resume its
		// workflow. reg.Replica returns false here and the caller 404s. Honest:
		// the session exists but its owner replica no longer does.
		ap, ok := reg.Replica(id, session.Replica)
		if !ok {
			return AgentProcess{}, errSessionOwnerUnavailable
		}
		if session.AgentID != ap.AgentID || session.TenantID != ap.Tenant ||
			session.AgentGeneration == "" ||
			session.AgentGeneration != ap.RegistrationGeneration {
			return AgentProcess{}, fmt.Errorf("%w: %q", store.ErrSessionNotFound, sid)
		}
		if session.Status == "external" {
			if err := st.TouchSession(r.Context(), sid); err != nil {
				return AgentProcess{}, fmt.Errorf("%w: touch external session affinity: %w", errSessionRoutingStore, err)
			}
		}
		return ap, nil
	}
	ap, ok := reg.Replica(id, 0)
	if !ok {
		return AgentProcess{}, errSessionOwnerUnavailable
	}
	return ap, nil
}

// addSessionBinding records the chosen ordinal for an externally-owned session
// after a successful create response. It buffers only the small JSON create
// envelope, restores it byte-for-byte for the client, and fails the proxy
// response if durable affinity cannot be recorded.
func addSessionBinding(rp *httputil.ReverseProxy, st store.Store, ap AgentProcess) {
	previous := rp.ModifyResponse
	rp.ModifyResponse = func(resp *http.Response) error {
		if previous != nil {
			if err := previous(resp); err != nil {
				return err
			}
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil
		}
		const maxCreateResponse = 1 << 20
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxCreateResponse+1))
		if err != nil {
			return fmt.Errorf("read session create response: %w", err)
		}
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(body))
		resp.ContentLength = int64(len(body))
		if len(body) > maxCreateResponse {
			return fmt.Errorf("session create response exceeds %d bytes", maxCreateResponse)
		}
		var envelope struct {
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal(body, &envelope); err != nil || envelope.SessionID == "" {
			return fmt.Errorf("session create response has no valid session_id")
		}
		tenant := ap.Tenant
		if tenant == "" {
			tenant = "default"
		}
		bindCtx, cancel := context.WithTimeout(context.WithoutCancel(resp.Request.Context()), 5*time.Second)
		defer cancel()
		if ap.RegistrationGeneration == "" {
			return fmt.Errorf("persist external session affinity: selected agent has no generation")
		}
		if err := st.BindSession(
			bindCtx, envelope.SessionID, tenant, ap.AgentID,
			ap.RegistrationGeneration, ap.ReplicaIndex); err != nil {
			return fmt.Errorf("persist external session affinity: %w", err)
		}
		return nil
	}
}

// sessionID extracts the {sid} from "/sessions/{sid}" or "/sessions/{sid}/...".
// Returns ok=false for "/sessions" and "/sessions/" (the collection, not an
// element).
func sessionID(path string) (string, bool) {
	const p = "/sessions/"
	if !strings.HasPrefix(path, p) {
		return "", false
	}
	rest := path[len(p):]
	if rest == "" {
		return "", false
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	if rest == "" {
		return "", false
	}
	return rest, true
}
