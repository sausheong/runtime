// Package config loads and validates the runtime.yaml agent list.
package config

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// AgentConfig is one agent entry in runtime.yaml.
type AgentConfig struct {
	ID         string   `yaml:"id"`
	Name       string   `yaml:"name"`
	Model      string   `yaml:"model"`
	ListenAddr string   `yaml:"listen_addr"`
	Kind       string   `yaml:"kind"`     // optional; "" ⇒ testagent. Resolved by agentd's kind registry.
	Command    []string `yaml:"command"`  // optional; when set, the supervisor execs this instead of the agentd binary (polyglot/foreign agents). argv form.
	WorkDir    string   `yaml:"workdir"`  // optional working directory for Command (e.g. a Python shim project root).
	Tenant     string   `yaml:"tenant"`   // optional; "" ⇒ "default" tenant. Owns this agent for access control.
	Memory     bool     `yaml:"memory"`   // optional; opt-in to the per-tenant Postgres memory tool. Default false.
	Replicas   int      `yaml:"replicas"` // optional; 0/omitted ⇒ 1. Local agents only: replica i listens on base_port+i.

	Autoscale *AutoscaleConfig `yaml:"autoscale"` // optional; nil ⇒ static A1 behavior (Replicas).

	// URL marks a REMOTE agent: runtimed attaches (health-check + proxy +
	// status) instead of spawning. Full base, e.g. "https://host:8443".
	// Mutually exclusive with ListenAddr — exactly one is required.
	URL string `yaml:"url"`
	// AuthToken is an optional shared bearer for the runtimed→remote-agent hop;
	// ${VAR}-expanded at load. Only valid with URL.
	AuthToken string `yaml:"auth_token"`

	// Gateway opts the agent into the platform MCP gateway (env-injected
	// URL+key). Optional; off (default) | full (true) | search.
	Gateway GatewayMode `yaml:"gateway"`

	// LimitsRaw is the yaml `limits:` block; Limits is the RESOLVED set (yaml
	// merged over RUNTIME_LIMIT_* env defaults) populated by Load. Valid on
	// local AND remote agents — enforcement is agent-side.
	LimitsRaw *LimitsConfig `yaml:"limits"`
	Limits    Limits        `yaml:"-"`
}

// LimitsConfig is the yaml-facing per-agent `limits:` block. Pointer fields
// distinguish "absent" (inherit platform env default) from an explicit zero
// (opt out of the default). Durations are Go duration strings.
type LimitsConfig struct {
	TurnTimeout    *string `yaml:"turn_timeout"`
	SessionTimeout *string `yaml:"session_timeout"`
	MaxTurns       *int    `yaml:"max_turns"`
	MaxTokens      *int    `yaml:"max_tokens"`
}

// Limits is the RESOLVED per-agent limit set (yaml merged over platform env
// defaults). Zero value of any field ⇒ that limit is unlimited. This struct is
// the wire shape serialized into RUNTIME_AGENT_LIMITS.
type Limits struct {
	TurnTimeoutMS    int64 `json:"turn_timeout_ms,omitempty"`
	SessionTimeoutMS int64 `json:"session_timeout_ms,omitempty"`
	MaxTurns         int   `json:"max_turns,omitempty"`
	MaxTokens        int   `json:"max_tokens,omitempty"`
}

// Empty reports whether no limit is set.
func (l Limits) Empty() bool { return l == Limits{} }

// JSON returns the RUNTIME_AGENT_LIMITS payload, or "" when no limit is set
// (the injector emits an empty env value, which agentd treats as unset).
func (l Limits) JSON() string {
	if l.Empty() {
		return ""
	}
	b, _ := json.Marshal(l)
	return string(b)
}

// ResolveLimits merges an agent's limits block over the platform env defaults
// (RUNTIME_LIMIT_TURN_TIMEOUT, RUNTIME_LIMIT_SESSION_TIMEOUT,
// RUNTIME_LIMIT_MAX_TURNS, RUNTIME_LIMIT_MAX_TOKENS). Per field: agent-set
// wins (an explicit zero means unlimited, overriding the default); absent
// falls back to the env default; neither ⇒ unlimited.
func ResolveLimits(raw *LimitsConfig, getenv func(string) string) (Limits, error) {
	dur := func(field string, agentVal *string, envKey string) (int64, error) {
		src, from := "", ""
		if agentVal != nil {
			src, from = *agentVal, "limits."+field
		} else if v := getenv(envKey); v != "" {
			src, from = v, envKey
		}
		if src == "" {
			return 0, nil
		}
		d, err := time.ParseDuration(src)
		if err != nil {
			return 0, fmt.Errorf("config: %s: invalid duration %q: %w", from, src, err)
		}
		if d < 0 {
			return 0, fmt.Errorf("config: %s: negative duration %q", from, src)
		}
		return d.Milliseconds(), nil
	}
	num := func(field string, agentVal *int, envKey string) (int, error) {
		if agentVal != nil {
			if *agentVal < 0 {
				return 0, fmt.Errorf("config: limits.%s: negative value %d", field, *agentVal)
			}
			return *agentVal, nil
		}
		if v := getenv(envKey); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return 0, fmt.Errorf("config: %s: invalid value %q", envKey, v)
			}
			return n, nil
		}
		return 0, nil
	}

	var l Limits
	var err error
	if l.TurnTimeoutMS, err = dur("turn_timeout", raw.turnTimeoutPtr(), "RUNTIME_LIMIT_TURN_TIMEOUT"); err != nil {
		return Limits{}, err
	}
	if l.SessionTimeoutMS, err = dur("session_timeout", raw.sessionTimeoutPtr(), "RUNTIME_LIMIT_SESSION_TIMEOUT"); err != nil {
		return Limits{}, err
	}
	if l.MaxTurns, err = num("max_turns", raw.maxTurnsPtr(), "RUNTIME_LIMIT_MAX_TURNS"); err != nil {
		return Limits{}, err
	}
	if l.MaxTokens, err = num("max_tokens", raw.maxTokensPtr(), "RUNTIME_LIMIT_MAX_TOKENS"); err != nil {
		return Limits{}, err
	}
	if l.TurnTimeoutMS > 0 && l.SessionTimeoutMS > 0 && l.TurnTimeoutMS > l.SessionTimeoutMS {
		return Limits{}, fmt.Errorf("config: limits: turn_timeout (%dms) exceeds session_timeout (%dms)", l.TurnTimeoutMS, l.SessionTimeoutMS)
	}
	return l, nil
}

// nil-safe accessors so ResolveLimits(nil, ...) means "no block".
func (lc *LimitsConfig) turnTimeoutPtr() *string {
	if lc == nil {
		return nil
	}
	return lc.TurnTimeout
}

func (lc *LimitsConfig) sessionTimeoutPtr() *string {
	if lc == nil {
		return nil
	}
	return lc.SessionTimeout
}

func (lc *LimitsConfig) maxTurnsPtr() *int {
	if lc == nil {
		return nil
	}
	return lc.MaxTurns
}

func (lc *LimitsConfig) maxTokensPtr() *int {
	if lc == nil {
		return nil
	}
	return lc.MaxTokens
}

// AutoscaleConfig, when present on a local agent, makes its replica pool float
// between Min and Max driven by active-session load. Absent (nil) ⇒ the static
// A1 pool (Replicas, or 1). See docs/superpowers/specs/2026-06-13-spine-a2-*.
type AutoscaleConfig struct {
	Min                      int `yaml:"min"`
	Max                      int `yaml:"max"`
	TargetSessionsPerReplica int `yaml:"target_sessions_per_replica"`
}

// TokenConfig is one control-plane API token. Label is for log attribution.
type TokenConfig struct {
	Token string `yaml:"token"`
	Label string `yaml:"label"`
}

// GatewayMode is the per-agent gateway opt-in. YAML accepts a bool
// (true ⇒ full, false ⇒ off) or a string ("full" | "search"); anything
// else is a load error. The zero value is off.
type GatewayMode string

const (
	GatewayOff    GatewayMode = ""       // not opted in
	GatewayFull   GatewayMode = "full"   // M1 behavior: full federated tools/list
	GatewaySearch GatewayMode = "search" // M2: list only search_tools; catalog via search
)

// Enabled reports whether the agent consumes the gateway at all.
func (g GatewayMode) Enabled() bool { return g == GatewayFull || g == GatewaySearch }

// UnmarshalYAML implements the bool-or-string union (yaml.v3 node form).
func (g *GatewayMode) UnmarshalYAML(value *yaml.Node) error {
	var b bool
	if err := value.Decode(&b); err == nil {
		if b {
			*g = GatewayFull
		} else {
			*g = GatewayOff
		}
		return nil
	}
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("config: invalid gateway mode (want true|false|full|search)")
	}
	switch s {
	case "full":
		*g = GatewayFull
	case "search":
		*g = GatewaySearch
	default:
		return fmt.Errorf("config: invalid gateway mode %q (want true|false|full|search)", s)
	}
	return nil
}

// GatewayServer is one upstream server the gateway federates. Exactly one of
// Command (stdio MCP), URL (Streamable HTTP MCP), or OpenAPI (REST adapter)
// must be set. Headers, Env, and (in GatewayConfig) AgentKeys values support
// ${VAR} expansion from the operator environment at load time so secrets stay
// out of the YAML file.
type GatewayServer struct {
	Name    string            `yaml:"name"`    // required, unique; namespaces tools as <name>__<tool>
	Command string            `yaml:"command"` // stdio transport: argv[0]
	Args    []string          `yaml:"args"`
	Env     map[string]string `yaml:"env"`     // extra env for the stdio child
	URL     string            `yaml:"url"`     // Streamable HTTP transport
	Headers map[string]string `yaml:"headers"` // static headers (auth) for HTTP
	Tenants []string          `yaml:"tenants"` // nil/empty ⇒ visible to ALL tenants

	// ForwardTenant makes the gateway inject the calling principal's tenant
	// into forwarded tool-call arguments as the reserved "__rt_tenant" key
	// (stripping any caller-supplied value first). Only valid for stdio
	// (command:) upstreams: the trust argument is that a stdio child is
	// reachable ONLY through the gateway.
	ForwardTenant bool `yaml:"forward_tenant"`

	// OpenAPI declares a REST upstream: a path or URL to an OpenAPI 3.x
	// document whose operations become gateway tools (third transport,
	// mutually exclusive with Command and URL).
	OpenAPI string `yaml:"openapi"`
	// BaseURL overrides the spec's servers[0] entry as the request base.
	// Only valid with OpenAPI. Required at dial time if the spec declares
	// no usable server entry.
	BaseURL string `yaml:"base_url"`
	// Operations is an optional allowlist: operationIds or "METHOD /glob"
	// patterns (path.Match syntax). Empty ⇒ all operations. Only valid
	// with OpenAPI.
	Operations []string `yaml:"operations"`

	// CredSecret names a per-tenant secret (in the secrets broker) whose value
	// is injected into CredHeader at dial time. Only valid for http/openapi
	// upstreams (never stdio). Set programmatically for DB-registered upstreams;
	// file upstreams normally use ${VAR}-expanded Headers instead.
	CredSecret string `yaml:"cred_secret"`
	// CredHeader is the header CredSecret's value is injected into (e.g.
	// "Authorization"). Required iff CredSecret is set.
	CredHeader string `yaml:"cred_header"`

	// Enrich maps a fixed principal claim (tenant|subject|role) to an outbound
	// header name, injected per-call from the calling principal. OpenAPI-only
	// (MCP-over-HTTP sessions set headers once at connect). Platform-set headers
	// overwrite caller values.
	Enrich map[string]string `yaml:"enrich"`
}

// GatewayConfig is the optional top-level gateway: section.
type GatewayConfig struct {
	Servers   []GatewayServer   `yaml:"servers"`
	AgentKeys map[string]string `yaml:"agent_keys"` // tenant → service key injected into gateway:true agents
	SelfURL   string            `yaml:"self_url"`   // optional base URL agents use to reach the gateway
}

// Enabled reports whether any upstream is configured.
func (g GatewayConfig) Enabled() bool { return len(g.Servers) > 0 }

// QuotaConfig is one file-configured gateway rate quota. Either key may be "*".
type QuotaConfig struct {
	Tenant     string `yaml:"tenant"`
	Upstream   string `yaml:"upstream"`
	RatePerMin int    `yaml:"rate_per_min"`
}

// Config is the parsed runtime.yaml.
type Config struct {
	Agents  []AgentConfig `yaml:"agents"`
	Tokens  []TokenConfig `yaml:"tokens"`
	Gateway GatewayConfig `yaml:"gateway"`
	Pricing Pricing       `yaml:"pricing"`
	Quotas  []QuotaConfig `yaml:"quotas"`
}

// Load reads and validates the config file at path.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	if err := c.Pricing.validate(); err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Validate checks required fields and uniqueness, and applies defaults for
// empty optional fields (an absent tenant becomes "default"). It mutates the
// config in place — c.Agents gets defaulted values, and gateway Headers, Env,
// and AgentKeys get ${VAR} env expansion — so callers see the resolved values
// after Load.
func (c *Config) Validate() error {
	if len(c.Agents) == 0 {
		return fmt.Errorf("config: at least one agent is required")
	}
	ids := map[string]bool{}
	dials := map[string]bool{} // unified: listen_addr OR url must be unique
	for i := range c.Agents {
		a := &c.Agents[i]
		if a.ID == "" || a.Name == "" || a.Model == "" {
			return fmt.Errorf("config: agent[%d] requires id, name, model", i)
		}
		// Exactly one of listen_addr / url.
		if (a.ListenAddr == "") == (a.URL == "") {
			return fmt.Errorf("config: agent %q requires exactly one of listen_addr (local) or url (remote)", a.ID)
		}
		remote := a.URL != ""
		if remote {
			// Validate the concrete dial form: substitute the {i} placeholder
			// (no-op for a single remote) so url.Parse never sees "{i}", which
			// is not a legal URL host character.
			probeURL := strings.ReplaceAll(a.URL, remoteOrdinalPlaceholder, "0")
			u, err := url.Parse(probeURL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return fmt.Errorf("config: agent %q url must be http(s)://host[:port] (got %q)", a.ID, a.URL)
			}
			// Local-only spawn fields can't be delivered to a process we don't
			// spawn. NOTE: replicas IS allowed on a remote agent (C2 M2 remote
			// pool) — but only paired with an {i}-templated url (checked below).
			if len(a.Command) > 0 || a.WorkDir != "" || a.Kind != "" || a.Memory || a.Gateway.Enabled() || a.Autoscale != nil {
				return fmt.Errorf("config: remote agent %q must not set command, workdir, kind, memory, gateway, or autoscale (these are spawn-time only)", a.ID)
			}
			// Remote replica pool (C2 M2): replicas>1 requires an {i} ordinal
			// placeholder in the url; a single remote must NOT contain {i}.
			hasTmpl := strings.Contains(a.URL, remoteOrdinalPlaceholder)
			if a.Replicas > 1 && !hasTmpl {
				return fmt.Errorf("config: remote agent %q has replicas %d but url %q has no %q ordinal placeholder", a.ID, a.Replicas, a.URL, remoteOrdinalPlaceholder)
			}
			if a.Replicas <= 1 && hasTmpl {
				return fmt.Errorf("config: remote agent %q url contains %q but is not a pool (set replicas > 1)", a.ID, remoteOrdinalPlaceholder)
			}
			if err := expandEnvScalar(&a.AuthToken, "agent "+a.ID+" auth_token"); err != nil {
				return err
			}
		} else if a.AuthToken != "" {
			return fmt.Errorf("config: agent %q auth_token is only valid with url (remote agents)", a.ID)
		}
		if a.Tenant == "" {
			a.Tenant = "default"
		}
		// Resolve limits (yaml block merged over RUNTIME_LIMIT_* env defaults)
		// so callers see a.Limits populated after Load. Valid on local AND
		// remote agents — enforcement is agent-side.
		l, err := ResolveLimits(a.LimitsRaw, os.Getenv)
		if err != nil {
			return fmt.Errorf("agent %q: %w", a.ID, err)
		}
		a.Limits = l
		if ids[a.ID] {
			return fmt.Errorf("config: duplicate agent id %q", a.ID)
		}
		ids[a.ID] = true
		if remote {
			for i := 0; i < a.RemotePoolSize(); i++ {
				ou, err := a.RemoteReplicaURL(i)
				if err != nil {
					return fmt.Errorf("config: %w", err)
				}
				if dials[ou] {
					return fmt.Errorf("config: agent %q ordinal url %q collides with another agent", a.ID, ou)
				}
				dials[ou] = true
			}
		} else {
			if a.Autoscale != nil {
				as := a.Autoscale
				if as.Min < 1 || as.Min > as.Max {
					return fmt.Errorf("config: agent %q autoscale requires 1 <= min <= max (got min=%d max=%d)", a.ID, as.Min, as.Max)
				}
				if as.TargetSessionsPerReplica < 1 {
					return fmt.Errorf("config: agent %q autoscale target_sessions_per_replica must be >= 1 (got %d)", a.ID, as.TargetSessionsPerReplica)
				}
				if a.Replicas > 0 {
					fmt.Fprintf(os.Stderr, "config: agent %q sets both replicas and autoscale; replicas is ignored (autoscale starts at min=%d)\n", a.ID, as.Min)
				}
			}
			addrs, err := a.reservedAddrs()
			if err != nil {
				return fmt.Errorf("config: %w", err)
			}
			for _, addr := range addrs {
				if dials[addr] {
					return fmt.Errorf("config: agent %q derived address %q collides with another agent", a.ID, addr)
				}
				dials[addr] = true
			}
		}
	}
	seen := map[string]bool{}
	for i, tk := range c.Tokens {
		if tk.Token == "" {
			return fmt.Errorf("config: token[%d] has empty token string", i)
		}
		if seen[tk.Token] {
			return fmt.Errorf("config: duplicate token at index %d", i)
		}
		seen[tk.Token] = true
	}
	names := map[string]bool{}
	for i := range c.Gateway.Servers {
		s := &c.Gateway.Servers[i]
		if s.Name == "" {
			return fmt.Errorf("config: gateway server[%d] requires name", i)
		}
		if names[s.Name] {
			return fmt.Errorf("config: duplicate gateway server name %q", s.Name)
		}
		// "__" is the <server>__<tool> separator: the gateway resolves the
		// owning server by cutting a tool name at the FIRST "__", so a name
		// containing "__" would alias against another server and could
		// silently disable tenant forwarding.
		if strings.Contains(s.Name, "__") {
			return fmt.Errorf("config: gateway server name %q must not contain \"__\" (reserved as the <server>__<tool> separator)", s.Name)
		}
		names[s.Name] = true
		transports := 0
		for _, v := range []string{s.Command, s.URL, s.OpenAPI} {
			if v != "" {
				transports++
			}
		}
		if transports != 1 {
			return fmt.Errorf("config: gateway server %q requires exactly one of command, url, or openapi", s.Name)
		}
		if s.ForwardTenant && s.Command == "" {
			return fmt.Errorf("config: gateway server %q: forward_tenant requires a stdio (command:) upstream", s.Name)
		}
		if s.BaseURL != "" && s.OpenAPI == "" {
			return fmt.Errorf("config: gateway server %q: base_url is only valid with openapi", s.Name)
		}
		if len(s.Operations) > 0 && s.OpenAPI == "" {
			return fmt.Errorf("config: gateway server %q: operations is only valid with openapi", s.Name)
		}
		for _, p := range s.Operations {
			if err := validateOperationPattern(p); err != nil {
				return fmt.Errorf("config: gateway server %q: %w", s.Name, err)
			}
		}
		if s.CredSecret != "" || s.CredHeader != "" {
			if s.Command != "" {
				return fmt.Errorf("config: gateway server %q: cred_secret/cred_header not allowed on stdio upstreams", s.Name)
			}
			if s.CredSecret == "" || s.CredHeader == "" {
				return fmt.Errorf("config: gateway server %q: cred_secret and cred_header must both be set", s.Name)
			}
		}
		if len(s.Enrich) > 0 {
			if s.OpenAPI == "" {
				return fmt.Errorf("config: gateway server %q: enrich is only valid with openapi", s.Name)
			}
			for claim, header := range s.Enrich {
				switch claim {
				case "tenant", "subject", "role":
				default:
					return fmt.Errorf("config: gateway server %q: unknown enrich claim %q (want tenant|subject|role)", s.Name, claim)
				}
				if header == "" {
					return fmt.Errorf("config: gateway server %q: enrich claim %q has empty header name", s.Name, claim)
				}
				// Collision with the credential header or any static header is a
				// load error (no runtime precedence ambiguity).
				if s.CredHeader != "" && http.CanonicalHeaderKey(header) == http.CanonicalHeaderKey(s.CredHeader) {
					return fmt.Errorf("config: gateway server %q: enrich header %q collides with cred_header", s.Name, header)
				}
				for hk := range s.Headers {
					if http.CanonicalHeaderKey(hk) == http.CanonicalHeaderKey(header) {
						return fmt.Errorf("config: gateway server %q: enrich header %q collides with a static header", s.Name, header)
					}
				}
				if !strings.HasPrefix(http.CanonicalHeaderKey(header), "X-Runtime-") {
					fmt.Fprintf(os.Stderr, "config: gateway server %q: enrich header %q does not use the reserved X-Runtime- prefix (allowed, but the convention marks platform-set claims)\n", s.Name, header)
				}
			}
		}
		if err := expandEnvMap(s.Headers, "gateway server "+s.Name+" headers"); err != nil {
			return err
		}
		if err := expandEnvMap(s.Env, "gateway server "+s.Name+" env"); err != nil {
			return err
		}
		if err := expandEnvScalar(&s.OpenAPI, "gateway server "+s.Name+" openapi"); err != nil {
			return err
		}
		if err := expandEnvScalar(&s.BaseURL, "gateway server "+s.Name+" base_url"); err != nil {
			return err
		}
	}
	if err := expandEnvMap(c.Gateway.AgentKeys, "gateway agent_keys"); err != nil {
		return err
	}
	for i := range c.Agents {
		if c.Agents[i].Gateway.Enabled() && !c.Gateway.Enabled() {
			return fmt.Errorf("config: agent %q has gateway: %s but no gateway.servers are configured", c.Agents[i].ID, c.Agents[i].Gateway)
		}
	}
	// Gateway quotas (P2.3): an empty quotas: block is valid. Each entry needs
	// non-empty keys (either may be "*") and a positive rate.
	for i, q := range c.Quotas {
		if q.Tenant == "" || q.Upstream == "" {
			return fmt.Errorf("config: quota[%d]: tenant and upstream are required", i)
		}
		if q.RatePerMin <= 0 {
			return fmt.Errorf("config: quota[%d]: rate_per_min must be > 0", i)
		}
	}
	return nil
}

// validateOperationPattern accepts a bare operationId (no space) or
// "METHOD /glob" where METHOD is an uppercase HTTP verb and glob is
// path.Match syntax.
func validateOperationPattern(p string) error {
	if p == "" {
		return fmt.Errorf("operations entry must not be empty")
	}
	method, rest, found := strings.Cut(p, " ")
	if !found {
		return nil // bare operationId
	}
	switch method {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS":
	default:
		return fmt.Errorf("operations entry %q: method must be an uppercase HTTP verb", p)
	}
	if !strings.HasPrefix(rest, "/") {
		return fmt.Errorf("operations entry %q: path must start with /", p)
	}
	if _, err := path.Match(rest, "/probe"); err != nil {
		return fmt.Errorf("operations entry %q: bad glob: %w", p, err)
	}
	return nil
}

// expandEnvMap expands ${VAR} references in every value of m from the operator
// environment, in place. An unset (or empty) variable is a hard error — silent
// empty-string expansion would send a malformed credential downstream. The
// $VAR form (no braces) is also expanded, matching os.Expand semantics.
// Values cannot contain a literal $ (os.Expand has no escape); operators
// should put such values in an env var and reference it with ${VAR}.
func expandEnvMap(m map[string]string, what string) error {
	for k, v := range m {
		expanded, missing := expandEnvValue(v)
		if len(missing) > 0 {
			return fmt.Errorf("config: %s %q references unset or empty env var(s) %v", what, k, missing)
		}
		m[k] = expanded
	}
	return nil
}

// expandEnvScalar is expandEnvMap for a single string field, with identical
// semantics: ${VAR}/$VAR expansion, unset-or-empty variable is a hard error,
// no escape for a literal $.
func expandEnvScalar(s *string, what string) error {
	expanded, missing := expandEnvValue(*s)
	if len(missing) > 0 {
		return fmt.Errorf("config: %s references unset or empty env var(s) %v", what, missing)
	}
	*s = expanded
	return nil
}

// expandEnvValue expands ${VAR}/$VAR references in v from the operator
// environment, collecting the names of unset-or-empty variables.
func expandEnvValue(v string) (expanded string, missing []string) {
	expanded = os.Expand(v, func(name string) string {
		val, ok := os.LookupEnv(name)
		if !ok || val == "" {
			missing = append(missing, name)
		}
		return val
	})
	return expanded, missing
}

// TokenMap returns token→label for all configured tokens. Empty when none.
func (c *Config) TokenMap() map[string]string {
	m := make(map[string]string, len(c.Tokens))
	for _, tk := range c.Tokens {
		if tk.Token == "" {
			continue // never authenticate an empty token (Validate also rejects these)
		}
		m[tk.Token] = tk.Label
	}
	return m
}

// AgentTenants returns agentID→tenantID for all agents (tenant defaulted to
// "default" by Validate). Used to build the identity Authorizer.
func (c *Config) AgentTenants() map[string]string {
	m := make(map[string]string, len(c.Agents))
	for _, a := range c.Agents {
		t := a.Tenant
		if t == "" {
			t = "default"
		}
		m[a.ID] = t
	}
	return m
}

// ReplicaAddr returns the derived host:base_port+i listen address for replica i
// of a local agent. Errors if the base listen_addr has no parseable numeric port
// or the derived port falls outside 1..65535.
func (a AgentConfig) ReplicaAddr(i int) (string, error) {
	host, portStr, err := net.SplitHostPort(a.ListenAddr)
	if err != nil {
		return "", fmt.Errorf("agent %q listen_addr %q: %w", a.ID, a.ListenAddr, err)
	}
	base, err := strconv.Atoi(portStr)
	if err != nil {
		return "", fmt.Errorf("agent %q listen_addr %q: port not numeric: %w", a.ID, a.ListenAddr, err)
	}
	port := base + i
	if base < 1 || port < 1 || port > 65535 {
		return "", fmt.Errorf("agent %q: derived replica port %d (base %d + index %d) out of range (1-65535)", a.ID, port, base, i)
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

// ReplicaAddrs returns the derived listen addresses for a local agent's STATIC
// pool: replica i listens on base_host:base_port+i. Replicas <= 0 means 1. Not
// meaningful for remote agents (Validate rejects replicas there).
func (a AgentConfig) ReplicaAddrs() ([]string, error) {
	n := a.Replicas
	if n <= 0 {
		n = 1
	}
	out := make([]string, n)
	for i := 0; i < n; i++ {
		addr, err := a.ReplicaAddr(i)
		if err != nil {
			return nil, err
		}
		out[i] = addr
	}
	return out, nil
}

// remoteOrdinalPlaceholder is the literal substring in a remote agent's url:
// that RemoteReplicaURL replaces with the 0-based ordinal. Required iff the
// remote agent runs a pool (replicas > 1); forbidden for a single remote.
const remoteOrdinalPlaceholder = "{i}"

// RemotePoolSize is the number of ordinals a remote agent attaches to: replicas
// when > 1, else 1. Meaningful only for remote (url:) agents.
func (a AgentConfig) RemotePoolSize() int {
	if a.Replicas > 1 {
		return a.Replicas
	}
	return 1
}

// RemoteReplicaURL returns the dial URL for ordinal i of a remote agent,
// substituting "{i}" with i. For a single remote (no placeholder) it returns
// the url unchanged for i==0. Errors if i is out of [0,RemotePoolSize).
func (a AgentConfig) RemoteReplicaURL(i int) (string, error) {
	n := a.RemotePoolSize()
	if i < 0 || i >= n {
		return "", fmt.Errorf("agent %q: remote ordinal %d out of range [0,%d)", a.ID, i, n)
	}
	if !strings.Contains(a.URL, remoteOrdinalPlaceholder) {
		return a.URL, nil
	}
	return strings.ReplaceAll(a.URL, remoteOrdinalPlaceholder, strconv.Itoa(i)), nil
}

// reservedAddrs returns the set of derived listen addresses to reserve in the
// dial-uniqueness map for a local agent. An autoscaled agent reserves its WHOLE
// max range (so a grown replica always finds a free, non-colliding port); a
// static agent reserves only its Replicas addresses.
func (a AgentConfig) reservedAddrs() ([]string, error) {
	if a.Autoscale != nil {
		out := make([]string, a.Autoscale.Max)
		for i := 0; i < a.Autoscale.Max; i++ {
			addr, err := a.ReplicaAddr(i)
			if err != nil {
				return nil, err
			}
			out[i] = addr
		}
		return out, nil
	}
	return a.ReplicaAddrs()
}
