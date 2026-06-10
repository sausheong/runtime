package main

import (
	"strings"
	"testing"

	"github.com/sausheong/runtime/internal/config"
)

func TestValidateGatewayKeys(t *testing.T) {
	cases := []struct {
		name    string
		cfg     *config.Config
		wantErr string // substrings that must appear; "" ⇒ nil error
	}{
		{
			name: "missing key errors naming agent and tenant",
			cfg: &config.Config{
				Agents:  []config.AgentConfig{{ID: "a1", Tenant: "acme", Gateway: config.GatewayFull}},
				Gateway: config.GatewayConfig{AgentKeys: map[string]string{}},
			},
			wantErr: `"a1"`,
		},
		{
			name: "key present is fine",
			cfg: &config.Config{
				Agents:  []config.AgentConfig{{ID: "a1", Tenant: "acme", Gateway: config.GatewayFull}},
				Gateway: config.GatewayConfig{AgentKeys: map[string]string{"acme": "sk-key"}},
			},
		},
		{
			name: "non-gateway agent without key is fine",
			cfg: &config.Config{
				Agents:  []config.AgentConfig{{ID: "a1", Tenant: "acme", Gateway: config.GatewayOff}},
				Gateway: config.GatewayConfig{AgentKeys: map[string]string{}},
			},
		},
		{
			name: "defaulted tenant uses the 'default' key",
			// config.Load rewrites Tenant "" to "default" before validation, so
			// the helper sees the post-default value.
			cfg: &config.Config{
				Agents:  []config.AgentConfig{{ID: "a1", Tenant: "default", Gateway: config.GatewayFull}},
				Gateway: config.GatewayConfig{AgentKeys: map[string]string{"default": "sk-key"}},
			},
		},
		{
			name: "defaulted tenant without key errors",
			cfg: &config.Config{
				Agents:  []config.AgentConfig{{ID: "a2", Tenant: "default", Gateway: config.GatewayFull}},
				Gateway: config.GatewayConfig{AgentKeys: nil},
			},
			wantErr: `"default"`,
		},
		{
			name: "first offender is named",
			cfg: &config.Config{
				Agents: []config.AgentConfig{
					{ID: "ok", Tenant: "acme", Gateway: config.GatewayFull},
					{ID: "bad", Tenant: "globex", Gateway: config.GatewayFull},
				},
				Gateway: config.GatewayConfig{AgentKeys: map[string]string{"acme": "sk"}},
			},
			wantErr: `"bad"`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateGatewayKeys(c.cfg)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("want nil, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("want error, got nil")
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("error %q does not name %s", err, c.wantErr)
			}
		})
	}

	// The error must name both the agent and its tenant.
	err := validateGatewayKeys(&config.Config{
		Agents: []config.AgentConfig{{ID: "a1", Tenant: "acme", Gateway: config.GatewayFull}},
	})
	if err == nil || !strings.Contains(err.Error(), `"a1"`) || !strings.Contains(err.Error(), `"acme"`) {
		t.Fatalf("error must name agent and tenant, got %v", err)
	}
}

func TestValidateGatewaySearch(t *testing.T) {
	mk := func(mode config.GatewayMode) *config.Config {
		return &config.Config{
			Agents: []config.AgentConfig{
				{ID: "a", Name: "A", Model: "m", ListenAddr: "127.0.0.1:1", Gateway: mode, Tenant: "default"},
			},
			Gateway: config.GatewayConfig{Servers: []config.GatewayServer{{Name: "fs", Command: "x"}}},
		}
	}
	t.Run("search agent without embeddings is an error", func(t *testing.T) {
		if err := validateGatewaySearch(mk(config.GatewaySearch), false); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("search agent with embeddings ok", func(t *testing.T) {
		if err := validateGatewaySearch(mk(config.GatewaySearch), true); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("full agent without embeddings ok", func(t *testing.T) {
		if err := validateGatewaySearch(mk(config.GatewayFull), false); err != nil {
			t.Fatal(err)
		}
	})
}
