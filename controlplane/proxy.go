package controlplane

import (
	"context"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
)

// AgentProcess describes the single hardcoded M1 agent subprocess.
type AgentProcess struct {
	AgentID string
	Addr    string // host:port the subprocess listens on, e.g. "127.0.0.1:8081"
	BinPath string // path to the agentd binary
	PGDSN   string
}

// SpawnFunc returns a Supervisor-compatible spawn closure that launches agentd
// with the right env and reports its exit on the returned channel.
func (a AgentProcess) SpawnFunc() func(ctx context.Context) <-chan error {
	return func(ctx context.Context) <-chan error {
		cmd := exec.CommandContext(ctx, a.BinPath)
		cmd.Env = append(os.Environ(),
			"RUNTIME_PG_DSN="+a.PGDSN,
			"RUNTIME_LISTEN_ADDR="+a.Addr,
			"RUNTIME_AGENT_ID="+a.AgentID,
		)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		ch := make(chan error, 1)
		if err := cmd.Start(); err != nil {
			ch <- err
			return ch
		}
		go func() { ch <- cmd.Wait() }()
		return ch
	}
}

// reverseProxy builds a passthrough to the agent subprocess at addr.
// FlushInterval = -1 ensures SSE/streaming responses are flushed immediately
// so events pass through promptly.
func reverseProxy(addr string) *httputil.ReverseProxy {
	target, _ := url.Parse("http://" + addr)
	rp := httputil.NewSingleHostReverseProxy(target)
	rp.FlushInterval = -1
	rp.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(w, "agent unavailable", http.StatusServiceUnavailable)
	}
	return rp
}
