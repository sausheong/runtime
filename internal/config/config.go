// Package config loads and validates the runtime.yaml agent list.
package config

import (
	"fmt"
	"os"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

// AgentConfig is one agent entry in runtime.yaml.
type AgentConfig struct {
	ID         string   `yaml:"id"`
	Name       string   `yaml:"name"`
	Model      string   `yaml:"model"`
	ListenAddr string   `yaml:"listen_addr"`
	Kind       string   `yaml:"kind"`    // optional; "" ⇒ testagent. Resolved by agentd's kind registry.
	Command    []string `yaml:"command"` // optional; when set, the supervisor execs this instead of the agentd binary (polyglot/foreign agents). argv form.
	WorkDir    string   `yaml:"workdir"` // optional working directory for Command (e.g. a Python shim project root).
	Tenant     string   `yaml:"tenant"`  // optional; "" ⇒ "default" tenant. Owns this agent for access control.
	Memory     bool     `yaml:"memory"` // optional; opt-in to the per-tenant Postgres memory tool. Default false.

	// Gateway opts the agent into the platform MCP gateway (env-injected
	// URL+key). Optional; off (default) | full (true) | search.
	Gateway GatewayMode `yaml:"gateway"`
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
}

// GatewayConfig is the optional top-level gateway: section.
type GatewayConfig struct {
	Servers   []GatewayServer   `yaml:"servers"`
	AgentKeys map[string]string `yaml:"agent_keys"` // tenant → service key injected into gateway:true agents
	SelfURL   string            `yaml:"self_url"`   // optional base URL agents use to reach the gateway
}

// Enabled reports whether any upstream is configured.
func (g GatewayConfig) Enabled() bool { return len(g.Servers) > 0 }

// Config is the parsed runtime.yaml.
type Config struct {
	Agents  []AgentConfig `yaml:"agents"`
	Tokens  []TokenConfig `yaml:"tokens"`
	Gateway GatewayConfig `yaml:"gateway"`
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
	addrs := map[string]bool{}
	for i := range c.Agents {
		a := &c.Agents[i]
		if a.ID == "" || a.Name == "" || a.Model == "" || a.ListenAddr == "" {
			return fmt.Errorf("config: agent[%d] requires id, name, model, listen_addr", i)
		}
		if a.Tenant == "" {
			a.Tenant = "default"
		}
		if ids[a.ID] {
			return fmt.Errorf("config: duplicate agent id %q", a.ID)
		}
		if addrs[a.ListenAddr] {
			return fmt.Errorf("config: duplicate listen_addr %q", a.ListenAddr)
		}
		ids[a.ID] = true
		addrs[a.ListenAddr] = true
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
