package controlplane

import "net/http"

// NewAPI returns the control-plane HTTP handler. M1: a transparent passthrough
// proxy to the single agent subprocess at agentAddr. The agent contract is
// served verbatim through this proxy. Multi-agent prefix routing arrives in M2.
func NewAPI(agentAddr string) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/", reverseProxy(agentAddr))
	return mux
}
