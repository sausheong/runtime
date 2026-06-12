package controlplane

import (
	"context"
	"net/http"
	"time"
)

// HealthMonitor polls a remote agent's /healthz until ctx is cancelled,
// reporting reachability transitions via OnChange (edge-triggered). It NEVER
// restarts the agent — runtimed does not own a remote process; it only
// observes. This is the remote-agent counterpart to Supervisor.
type HealthMonitor struct {
	BaseURL  string               // full base "scheme://host:port"
	Token    string               // optional bearer ("" ⇒ none)
	Interval time.Duration        // poll period (default 10s)
	OnChange func(reachable bool) // fired only when reachability flips; nil ⇒ no-op
}

// Run polls until ctx is cancelled. The first observation always fires OnChange
// (unknown→reachable or unknown→unreachable).
func (h *HealthMonitor) Run(ctx context.Context) {
	interval := h.Interval
	if interval == 0 {
		interval = 10 * time.Second
	}
	client := &http.Client{Timeout: 2 * time.Second}
	var last int // 0=unknown, 1=reachable, -1=unreachable
	probe := func() {
		ok := h.healthy(ctx, client)
		cur := -1
		if ok {
			cur = 1
		}
		if cur != last {
			last = cur
			if h.OnChange != nil {
				h.OnChange(ok)
			}
		}
	}
	probe() // immediate first check, no initial Interval delay
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			probe()
		}
	}
}

// healthy reports whether GET BaseURL+"/healthz" returns 200.
func (h *HealthMonitor) healthy(ctx context.Context, client *http.Client) bool {
	req, err := http.NewRequestWithContext(ctx, "GET", h.BaseURL+"/healthz", nil)
	if err != nil {
		return false
	}
	if h.Token != "" {
		req.Header.Set("Authorization", "Bearer "+h.Token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
