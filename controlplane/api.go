package controlplane

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

// NewAPI returns the control-plane HTTP handler routing /agents/{id}/... to
// each agent's subprocess, plus GET /agents and GET /healthz.
func NewAPI(reg *Registry) *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("GET /agents", func(w http.ResponseWriter, _ *http.Request) {
		type agentStatus struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Model   string `json:"model"`
			Healthy bool   `json:"healthy"`
		}
		infos := reg.List()
		out := make([]agentStatus, len(infos))
		var wg sync.WaitGroup
		client := &http.Client{Timeout: 1 * time.Second}
		for i, info := range infos {
			out[i] = agentStatus{ID: info.ID, Name: info.Name, Model: info.Model}
			ap, _ := reg.Get(info.ID)
			wg.Add(1)
			go func(i int, addr string) {
				defer wg.Done()
				resp, err := client.Get("http://" + addr + "/healthz")
				if err == nil {
					out[i].Healthy = resp.StatusCode == 200
					resp.Body.Close()
				}
			}(i, ap.Addr)
		}
		wg.Wait()
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
		reverseProxy(ap.Addr).ServeHTTP(w, r)
	})

	return mux
}
