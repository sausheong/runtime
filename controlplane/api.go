package controlplane

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sausheong/runtime/internal/obs"
)

// NewAPI returns the control-plane HTTP handler routing /agents/{id}/... to
// each agent's subprocess, plus GET /agents and GET /healthz. m records
// proxy-error metrics; nil ⇒ no-op (obs methods are nil-receiver-safe).
func NewAPI(reg *Registry, m *obs.ControlMetrics) *http.ServeMux {
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
		// Results are collected concurrently; order is not significant (no
		// consumer relies on registry order).
		var out []agentStatus
		var mu sync.Mutex
		var wg sync.WaitGroup
		client := &http.Client{Timeout: 1 * time.Second}
		for _, info := range infos {
			// In open mode (no principal) show all; otherwise filter by tenant.
			if hasP && !p.Superuser && info.Tenant != p.TenantID {
				continue
			}
			ap, _ := reg.Get(info.ID)
			wg.Add(1)
			go func(info AgentInfo, ap AgentProcess) {
				defer wg.Done()
				st := agentStatus{ID: info.ID, Name: info.Name, Model: info.Model}
				req, _ := http.NewRequest("GET", ap.baseURL()+"/healthz", nil)
				if ap.AuthToken != "" {
					req.Header.Set("Authorization", "Bearer "+ap.AuthToken)
				}
				resp, err := client.Do(req)
				if err == nil {
					st.Healthy = resp.StatusCode == 200
					resp.Body.Close()
				}
				mu.Lock()
				out = append(out, st)
				mu.Unlock()
			}(info, ap)
		}
		wg.Wait()
		if out == nil {
			out = []agentStatus{}
		}
		_ = json.NewEncoder(w).Encode(out)
	})

	// Subtree pattern: matches /agents/{id}/sessions, /agents/{id}/healthz, etc.
	// A bare /agents/{id} (no trailing slash) gets a stdlib 301 redirect to the
	// trailing-slash form — harmless, since every agent-contract endpoint lives
	// at a subpath, never at the bare prefix.
	mux.HandleFunc("/agents/{id}/", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		ap, ok := reg.Get(id)
		if !ok {
			http.Error(w, "unknown agent "+id, http.StatusNotFound)
			return
		}
		prefix := "/agents/" + id
		r.URL.Path = strings.TrimPrefix(r.URL.Path, prefix)
		if r.URL.Path == "" {
			r.URL.Path = "/"
		}
		r.URL.RawPath = "" // avoid stale encoded-path mismatches after rewrite
		reverseProxy(ap.baseURL(), ap.AuthToken, func() { m.ProxyError(ap.AgentID) }).ServeHTTP(w, r)
	})

	return mux
}
