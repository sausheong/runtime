// Package config loads and validates the runtime.yaml agent list.
package config

import (
	"fmt"
	"os"

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

// GatewayServer is one upstream MCP server the gateway federates. Exactly one
// of Command (stdio) or URL (Streamable HTTP) must be set. Headers, Env, and
// (in GatewayConfig) AgentKeys values support ${VAR} expansion from the
// operator environment at load time so secrets stay out of the YAML file.
type GatewayServer struct {
	Name    string            `yaml:"name"`    // required, unique; namespaces tools as <name>__<tool>
	Command string            `yaml:"command"` // stdio transport: argv[0]
	Args    []string          `yaml:"args"`
	Env     map[string]string `yaml:"env"`     // extra env for the stdio child
	URL     string            `yaml:"url"`     // Streamable HTTP transport
	Headers map[string]string `yaml:"headers"` // static headers (auth) for HTTP
	Tenants []string          `yaml:"tenants"` // nil/empty ⇒ visible to ALL tenants
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
		names[s.Name] = true
		if (s.Command == "") == (s.URL == "") {
			return fmt.Errorf("config: gateway server %q requires exactly one of command or url", s.Name)
		}
		if err := expandEnvMap(s.Headers, "gateway server "+s.Name+" headers"); err != nil {
			return err
		}
		if err := expandEnvMap(s.Env, "gateway server "+s.Name+" env"); err != nil {
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

// expandEnvMap expands ${VAR} references in every value of m from the operator
// environment, in place. An unset (or empty) variable is a hard error — silent
// empty-string expansion would send a malformed credential downstream. The
// $VAR form (no braces) is also expanded, matching os.Expand semantics.
// Values cannot contain a literal $ (os.Expand has no escape); operators
// should put such values in an env var and reference it with ${VAR}.
func expandEnvMap(m map[string]string, what string) error {
	for k, v := range m {
		var missing []string
		expanded := os.Expand(v, func(name string) string {
			val, ok := os.LookupEnv(name)
			if !ok || val == "" {
				missing = append(missing, name)
			}
			return val
		})
		if len(missing) > 0 {
			return fmt.Errorf("config: %s %q references unset or empty env var(s) %v", what, k, missing)
		}
		m[k] = expanded
	}
	return nil
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
