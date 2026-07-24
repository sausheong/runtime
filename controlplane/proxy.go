package controlplane

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/sausheong/runtime/internal/netpolicy"
)

// SecretBroker resolves a tenant's secrets to name->plaintext at spawn time.
// *identity.Broker implements it. A nil broker means no brokering (back-compat).
type SecretBroker interface {
	SecretsFor(ctx context.Context, tenant string) (map[string]string, error)
}

// PolicyResolver resolves an agent's online eval policy JSON at spawn time.
// "" ⇒ no policy. Mirrors SecretBroker (resolved in envDelta, nil-safe,
// fail-open): a resolver error yields no policy, never a spawn failure.
type PolicyResolver interface {
	PolicyJSON(ctx context.Context, tenant, agentID string) (string, error)
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

	SubjectForwarding bool // opt-in: when true, spawn env carries RUNTIME_SUBJECT_FORWARDING=1 so agentd consumes the forwarded caller subject.

	// Remote marks an attach-only agent: no spawn, no Supervisor — runtimed
	// health-checks, proxies, and reports status, but never restarts it.
	Remote bool
	// BaseURL is the full dial base "scheme://host:port". For local agents it
	// is synthesized as "http://"+Addr; for remote agents it is the config url.
	BaseURL string
	// AuthToken is an optional shared bearer added to every request runtimed
	// makes to this agent (proxy, health, metrics). "" ⇒ no auth header.
	AuthToken string
	// RestrictOutbound applies the public-network-only dial policy. It is set
	// for tenant-registered remote agents; file-configured remotes remain an
	// operator trust decision and may intentionally target private services.
	RestrictOutbound bool

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

	// LimitsJSON is the resolved lifecycle-limit set (config.Limits.JSON()),
	// injected as RUNTIME_AGENT_LIMITS. "" ⇒ no limits.
	LimitsJSON string

	// PricingJSON is this agent's resolved per-model price (config.ModelPrice.JSON()),
	// injected as RUNTIME_AGENT_PRICING. "" ⇒ the model is unpriced (tokens flow,
	// cost skipped). Each agent serves one model, so one price object suffices.
	PricingJSON string

	broker SecretBroker // optional; injected by the Registry. nil ⇒ no secret brokering.

	policyResolver PolicyResolver // optional; injected by the Registry. nil ⇒ no eval policy.
}

// envDelta returns the complete platform-owned environment delta for an agent:
// the RUNTIME_* control vars, opt-in feature vars, and (if a broker is set) the
// tenant's decrypted secrets. It NEVER includes os.Environ(), so it is safe to
// serialize across the network to a remote agent. A broker error fails closed.
func (a AgentProcess) envDelta(ctx context.Context) ([]string, error) {
	env := []string{
		"RUNTIME_PG_DSN=" + a.PGDSN,
		"RUNTIME_LISTEN_ADDR=" + a.Addr,
		"RUNTIME_AGENT_ID=" + a.AgentID,
		"RUNTIME_AGENT_KIND=" + a.Kind,
		"RUNTIME_AGENT_TENANT=" + a.Tenant,
		// Per-replica identity (Spine A1): RUNTIME_AGENT_REPLICA tells agentd its
		// index (stamped on sessions); DBOS__VMID is the stable executor id so a
		// restarted replica recovers exactly its own in-flight workflows.
		"RUNTIME_AGENT_REPLICA=" + strconv.Itoa(a.ReplicaIndex),
		"DBOS__VMID=" + a.DBOSVMID,
		// Resolved lifecycle limits (P1.2). Always emitted — explicit empty when
		// no limits, so an inherited operator var can't smuggle limits in.
		"RUNTIME_AGENT_LIMITS=" + a.LimitsJSON,
		// Resolved per-model price (P1.3). Always emitted — explicit empty when
		// unpriced, so an inherited operator var can't smuggle a price in.
		"RUNTIME_AGENT_PRICING=" + a.PricingJSON,
	}
	// Resolved per-agent online eval policy (P3.1). Always emitted — explicit
	// empty when no policy, so an inherited operator var can't smuggle one in.
	// Fail-open: a resolver error ⇒ empty policy ⇒ the agent scores nothing,
	// never a spawn failure (unlike the broker, which fails closed).
	policyJSON := ""
	if a.policyResolver != nil {
		pj, err := a.policyResolver.PolicyJSON(ctx, a.Tenant, a.AgentID)
		if err != nil {
			slog.Warn("eval policy resolve failed; agent runs without a policy", "tenant", a.Tenant, "agent", a.AgentID, "err", err)
		} else {
			policyJSON = pj
		}
	}
	env = append(env, "RUNTIME_EVAL_POLICY="+policyJSON)
	// Agents that did NOT opt in get explicit empty-value entries so an
	// inherited operator var (e.g. a leaked RUNTIME_GATEWAY_URL) can't enable
	// the feature: exec.Cmd uses the LAST duplicate env entry, and agentd
	// treats empty as unset (memory requires "1", gateway requires a URL).
	if a.Memory {
		env = append(env, "RUNTIME_AGENT_MEMORY=1")
	} else {
		env = append(env, "RUNTIME_AGENT_MEMORY=")
	}
	// Edge subject-forwarding switch (P2.2 M2). Always emitted — explicit empty
	// when off, so an inherited operator var can't flip it on. Lives in envDelta
	// so both local spawn and the remote /register handshake carry it.
	if a.SubjectForwarding {
		env = append(env, "RUNTIME_SUBJECT_FORWARDING=1")
	} else {
		env = append(env, "RUNTIME_SUBJECT_FORWARDING=")
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
			// Defense-in-depth: brokered secrets come AFTER the fixed control
			// block above, and the last duplicate wins (exec.Cmd, and the
			// /register fold-to-map), so a reserved-prefix name could shadow an
			// operator control var (e.g. RUNTIME_AGENT_LIMITS={} ⇒ unlimited).
			// Creation already rejects these; skip any that predate the guard.
			// Logging the NAME here does not violate the no-secret-name-in-logs
			// invariant: that invariant protects legitimate tenant secret names,
			// and a reserved-prefix name is invalid by construction (rejected at
			// creation, attacker-chosen). The VALUE is never logged.
			if HasReservedEnvPrefix(name) {
				slog.Warn("skipping brokered secret with reserved-prefix name",
					"tenant", a.Tenant, "agent", a.AgentID, "name", name)
				continue
			}
			env = append(env, name+"="+val)
		}
	}
	return env, nil
}

// childBaseEnv returns the deliberately small subset of runtimed's environment
// that a local agent process may inherit. In particular, platform credentials
// such as RUNTIME_SECRETS_KEYS, RUNTIME_ADMIN_BOOTSTRAP, OIDC client secrets,
// and arbitrary operator provider keys are never copied implicitly.
//
// RUNTIME_AGENT_ENV_PASSTHROUGH is an operator escape hatch for a named,
// comma-separated set of additional variables. Platform-reserved names remain
// forbidden: an agent must receive those through envDelta, where their value is
// scoped and controlled. Secret provider credentials should be stored in the
// tenant secret broker rather than passed through here.
func childBaseEnv() []string {
	allowed := map[string]struct{}{
		"HOME":                        {},
		"LANG":                        {},
		"LC_ALL":                      {},
		"LC_CTYPE":                    {},
		"NODE_EXTRA_CA_CERTS":         {},
		"PATH":                        {},
		"SSL_CERT_DIR":                {},
		"SSL_CERT_FILE":               {},
		"TMPDIR":                      {},
		"TZ":                          {},
		"OTEL_EXPORTER_OTLP_ENDPOINT": {},
		"RUNTIME_LOG_FORMAT":          {},
		"RUNTIME_TRACE_SAMPLE_RATIO":  {},
		"RUNTIME_TRACING_ENABLED":     {},
	}
	for _, name := range strings.Split(os.Getenv("RUNTIME_AGENT_ENV_PASSTHROUGH"), ",") {
		name = strings.TrimSpace(name)
		if name == "" || HasReservedEnvPrefix(name) {
			continue
		}
		allowed[name] = struct{}{}
	}
	names := make([]string, 0, len(allowed))
	for name := range allowed {
		names = append(names, name)
	}
	sort.Strings(names)
	env := make([]string, 0, len(names))
	for _, name := range names {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return env
}

// buildEnv assembles the full child environment for a LOCAL spawn: a minimal
// safe base followed by envDelta. The tenant's scoped secrets come last and
// therefore win over an explicitly passed-through variable of the same name.
// A broker error fails closed — the caller must not start the process.
func (a AgentProcess) buildEnv(ctx context.Context) ([]string, error) {
	delta, err := a.envDelta(ctx)
	if err != nil {
		return nil, err
	}
	return append(childBaseEnv(), delta...), nil
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
	return reverseProxyWithTransport(base, token, nil, onError)
}

func reverseProxyWithTransport(base, token string, baseTransport http.RoundTripper, onError func()) *httputil.ReverseProxy {
	target, _ := url.Parse(base)
	rp := httputil.NewSingleHostReverseProxy(target)
	// otelhttp wraps the auth transport: injects traceparent from the active
	// span and records a client span. With tracing off (no-op provider) this is
	// a cheap pass-through.
	rp.Transport = otelhttp.NewTransport(authTransport{token: token, base: baseTransport})
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

func agentOutboundTransport(ap AgentProcess) http.RoundTripper {
	if ap.RestrictOutbound {
		return netpolicy.PublicTransport()
	}
	return nil
}
