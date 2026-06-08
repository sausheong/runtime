// Package config loads and validates the runtime.yaml agent list.
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// AgentConfig is one agent entry in runtime.yaml.
type AgentConfig struct {
	ID         string `yaml:"id"`
	Name       string `yaml:"name"`
	Model      string `yaml:"model"`
	ListenAddr string `yaml:"listen_addr"`
}

// TokenConfig is one control-plane API token. Label is for log attribution.
type TokenConfig struct {
	Token string `yaml:"token"`
	Label string `yaml:"label"`
}

// Config is the parsed runtime.yaml.
type Config struct {
	Agents []AgentConfig `yaml:"agents"`
	Tokens []TokenConfig `yaml:"tokens"`
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

// Validate checks required fields and uniqueness.
func (c *Config) Validate() error {
	if len(c.Agents) == 0 {
		return fmt.Errorf("config: at least one agent is required")
	}
	ids := map[string]bool{}
	addrs := map[string]bool{}
	for i, a := range c.Agents {
		if a.ID == "" || a.Name == "" || a.Model == "" || a.ListenAddr == "" {
			return fmt.Errorf("config: agent[%d] requires id, name, model, listen_addr", i)
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
	return nil
}

// TokenMap returns token→label for all configured tokens. Empty when none.
func (c *Config) TokenMap() map[string]string {
	m := make(map[string]string, len(c.Tokens))
	for _, tk := range c.Tokens {
		m[tk.Token] = tk.Label
	}
	return m
}
