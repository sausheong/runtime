package controlplane

import (
	"context"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
)

// SecretBroker resolves a tenant's secrets to name->plaintext at spawn time.
// *identity.Broker implements it. A nil broker means no brokering (back-compat).
type SecretBroker interface {
	SecretsFor(ctx context.Context, tenant string) (map[string]string, error)
}

// AgentProcess describes a supervised agent subprocess.
type AgentProcess struct {
	AgentID string
	Addr    string // host:port the subprocess listens on, e.g. "127.0.0.1:8081"
	BinPath string // path to the agentd binary
	PGDSN   string
	Kind    string   // optional agent kind; "" ⇒ testagent. Passed via RUNTIME_AGENT_KIND.
	Command []string // when non-empty, exec this instead of BinPath (foreign-process agents)
	WorkDir string   // optional working directory for Command
	Tenant  string   // tenant that owns this agent (from runtime.yaml; "default" if unset)
	Memory  bool     // opt-in: when true, the spawn env carries RUNTIME_AGENT_MEMORY=1 so agentd wires the memory tool.

	GatewayOn  bool   // opt-in: when true, spawn env carries RUNTIME_GATEWAY_URL (+_KEY when set).
	GatewayURL string // full URL of the platform gateway MCP endpoint.
	GatewayKey string // tenant service key for the gateway; "" in open mode.

	GatewaySearch bool // search-mode opt-in: appends ?mode=search to the injected gateway URL.

	broker SecretBroker // optional; injected by the Registry. nil ⇒ no secret brokering.
}

// buildEnv assembles the child environment: the inherited operator env, then the
// RUNTIME_* control vars, then (if a broker is set) the tenant's decrypted
// secrets LAST so they shadow any inherited var of the same name. A broker error
// fails closed — the caller must not start the process.
func (a AgentProcess) buildEnv(ctx context.Context) ([]string, error) {
	env := append(os.Environ(),
		"RUNTIME_PG_DSN="+a.PGDSN,
		"RUNTIME_LISTEN_ADDR="+a.Addr,
		"RUNTIME_AGENT_ID="+a.AgentID,
		"RUNTIME_AGENT_KIND="+a.Kind,
		"RUNTIME_AGENT_TENANT="+a.Tenant,
	)
	// Agents that did NOT opt in get explicit empty-value entries so an
	// inherited operator var (e.g. a leaked RUNTIME_GATEWAY_URL) can't enable
	// the feature: exec.Cmd uses the LAST duplicate env entry, and agentd
	// treats empty as unset (memory requires "1", gateway requires a URL).
	if a.Memory {
		env = append(env, "RUNTIME_AGENT_MEMORY=1")
	} else {
		env = append(env, "RUNTIME_AGENT_MEMORY=")
	}
	if a.GatewayOn {
		u := a.GatewayURL
		if a.GatewaySearch {
			u += "?mode=search"
		}
		env = append(env, "RUNTIME_GATEWAY_URL="+u)
		if a.GatewayKey != "" {
			env = append(env, "RUNTIME_GATEWAY_KEY="+a.GatewayKey)
		} else {
			env = append(env, "RUNTIME_GATEWAY_KEY=")
		}
	} else {
		env = append(env, "RUNTIME_GATEWAY_URL=", "RUNTIME_GATEWAY_KEY=")
	}
	if a.broker != nil {
		secrets, err := a.broker.SecretsFor(ctx, a.Tenant)
		if err != nil {
			return nil, err
		}
		for name, val := range secrets {
			env = append(env, name+"="+val)
		}
	}
	return env, nil
}

// SpawnFunc returns a Supervisor-compatible spawn closure that launches agentd
// (or, when Command is set, an arbitrary command) with the brokered env and
// reports its exit on the returned channel.
func (a AgentProcess) SpawnFunc() func(ctx context.Context) <-chan error {
	return func(ctx context.Context) <-chan error {
		ch := make(chan error, 1)
		env, err := a.buildEnv(ctx)
		if err != nil {
			ch <- err
			return ch
		}
		var cmd *exec.Cmd
		if len(a.Command) > 0 {
			cmd = exec.CommandContext(ctx, a.Command[0], a.Command[1:]...)
			if a.WorkDir != "" {
				cmd.Dir = a.WorkDir
			}
		} else {
			cmd = exec.CommandContext(ctx, a.BinPath)
		}
		cmd.Env = env
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
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
