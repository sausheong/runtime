package controlplane

import (
	"context"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
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

	// Remote marks an attach-only agent: no spawn, no Supervisor — runtimed
	// health-checks, proxies, and reports status, but never restarts it.
	Remote bool
	// BaseURL is the full dial base "scheme://host:port". For local agents it
	// is synthesized as "http://"+Addr; for remote agents it is the config url.
	BaseURL string
	// AuthToken is an optional shared bearer added to every request runtimed
	// makes to this agent (proxy, health, metrics). "" ⇒ no auth header.
	AuthToken string

	// ReplicaIndex is this replica's 0-based index within its agent's pool.
	// 0 for single-replica and remote agents. Injected into the child as
	// RUNTIME_AGENT_REPLICA and used to derive the listen port and executor id.
	ReplicaIndex int
	// DBOSVMID is the stable per-replica DBOS executor id "<AgentID>#<index>"
	// (injected as DBOS__VMID). "" for remote agents (the remote owns its own
	// executor id). A restart at the same index reuses this id, so the replica
	// recovers exactly its own in-flight workflows.
	DBOSVMID string

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

// DialBase returns the agent's full dial base URL (exported for callers in
// package main: runtimed's metrics target builder).
func (a AgentProcess) DialBase() string { return a.baseURL() }

// baseURL returns the full dial base for the agent. Local agents (set only via
// Addr) fall back to http://Addr; remote agents carry an explicit BaseURL.
func (a AgentProcess) baseURL() string {
	if a.BaseURL != "" {
		return a.BaseURL
	}
	return "http://" + a.Addr
}

// authTransport adds a bearer token to every request. token=="" ⇒ pass through
// unchanged. The request is cloned so the caller's *http.Request is never
// mutated (the ReverseProxy reuses its outgoing request object).
type authTransport struct {
	token string
	base  http.RoundTripper // nil ⇒ http.DefaultTransport
}

func (t authTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	if t.token != "" {
		r = r.Clone(r.Context())
		r.Header.Set("Authorization", "Bearer "+t.token)
	}
	return base.RoundTrip(r)
}

// reverseProxy builds a passthrough to the agent at base ("scheme://host:port").
// When token != "", every forwarded request carries an Authorization: Bearer
// header (remote agents). FlushInterval = -1 keeps SSE/streaming prompt.
// onError (nil ⇒ no-op) fires before each 503 served by the ErrorHandler.
func reverseProxy(base, token string, onError func()) *httputil.ReverseProxy {
	target, _ := url.Parse(base)
	rp := httputil.NewSingleHostReverseProxy(target)
	// otelhttp wraps the auth transport: injects traceparent from the active
	// span and records a client span. With tracing off (no-op provider) this is
	// a cheap pass-through.
	rp.Transport = otelhttp.NewTransport(authTransport{token: token})
	rp.FlushInterval = -1
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, _ error) {
		// Client-initiated cancellation is not an agent failure; don't count it.
		if onError != nil && r.Context().Err() == nil {
			onError()
		}
		http.Error(w, "agent unavailable", http.StatusServiceUnavailable)
	}
	rp.ModifyResponse = func(resp *http.Response) error {
		// runtimed's own echo of X-Request-Id is authoritative; drop the
		// agent's duplicate (ReverseProxy copies backend headers with Add).
		resp.Header.Del("X-Request-Id")
		return nil
	}
	return rp
}
