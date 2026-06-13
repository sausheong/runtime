package controlplane

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sausheong/runtime/internal/obs"
	"github.com/sausheong/runtime/internal/store"
)

// NewAPI returns the control-plane HTTP handler routing /agents/{id}/... to each
// agent's replica pool, plus GET /agents and GET /healthz. New sessions
// round-robin across replicas; session-scoped requests pin to the owning replica
// (resolved from st); replica-agnostic paths use replica 0. m records
// proxy-error metrics; nil ⇒ no-op. st resolves session→replica affinity and is
// REQUIRED (non-nil); unlike m it is not nil-safe (pickReplica dereferences it).
func NewAPI(reg *Registry, m *obs.ControlMetrics, st store.Store) *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
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
		var out []agentStatus
		var mu sync.Mutex
		var wg sync.WaitGroup
		client := &http.Client{Timeout: 1 * time.Second}
		for _, info := range infos {
			if hasP && !p.Superuser && info.Tenant != p.TenantID {
				continue
			}
			replicas, _ := reg.Replicas(info.ID)
			wg.Add(1)
			go func(info AgentInfo, replicas []AgentProcess) {
				defer wg.Done()
				st := agentStatus{ID: info.ID, Name: info.Name, Model: info.Model}
				// An agent is healthy if ANY replica answers /healthz.
				for _, ap := range replicas {
					req, _ := http.NewRequest("GET", ap.baseURL()+"/healthz", nil)
					if ap.AuthToken != "" {
						req.Header.Set("Authorization", "Bearer "+ap.AuthToken)
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
				mu.Lock()
				out = append(out, st)
				mu.Unlock()
			}(info, replicas)
		}
		wg.Wait()
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
		prefix := "/agents/" + id
		r.URL.Path = strings.TrimPrefix(r.URL.Path, prefix)
		if r.URL.Path == "" {
			r.URL.Path = "/"
		}
		r.URL.RawPath = "" // avoid stale encoded-path mismatches after rewrite

		ap, ok := pickReplica(r, reg, st, id)
		if !ok {
			http.Error(w, "unknown session", http.StatusNotFound)
			return
		}
		reverseProxy(ap.baseURL(), ap.AuthToken, func() { m.ProxyError(id) }).ServeHTTP(w, r)
	})

	return mux
}

// pickReplica chooses which replica serves this (already-prefix-stripped)
// request:
//   - POST /sessions (exactly)         → round-robin a new session
//   - /sessions/{sid}[/...]            → pin to the owner replica (from st)
//   - everything else (list, healthz)  → replica 0 (agent-level, replica-agnostic)
//
// Returns ok=false only when a session-scoped path names an unknown session.
func pickReplica(r *http.Request, reg *Registry, st store.Store, id string) (AgentProcess, bool) {
	path := r.URL.Path
	if r.Method == "POST" && path == "/sessions" {
		return reg.Replica(id, reg.NextReplica(id))
	}
	if sid, ok := sessionID(path); ok {
		i, err := st.SessionReplica(r.Context(), sid)
		if err != nil {
			return AgentProcess{}, false // session truly unknown
		}
		// A known session whose stored owner index is now out of range (e.g. the
		// agent was reconfigured to fewer replicas than when this session was
		// created) is unroutable: only that original executor can resume its
		// workflow. reg.Replica returns false here and the caller 404s. Honest:
		// the session exists but its owner replica no longer does.
		return reg.Replica(id, i)
	}
	return reg.Replica(id, 0)
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
