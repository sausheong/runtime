package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTmp(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "runtime.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad_Valid(t *testing.T) {
	p := writeTmp(t, `
agents:
  - id: support
    name: Support
    model: test/scripted
    listen_addr: 127.0.0.1:8101
  - id: research
    name: Research
    model: test/scripted
    listen_addr: 127.0.0.1:8102
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Agents) != 2 {
		t.Fatalf("agents = %d, want 2", len(cfg.Agents))
	}
	if cfg.Agents[0].ID != "support" || cfg.Agents[0].ListenAddr != "127.0.0.1:8101" {
		t.Fatalf("bad first agent: %+v", cfg.Agents[0])
	}
}

func TestLoadKind(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	os.WriteFile(p, []byte("agents:\n  - {id: n, name: N, model: openai/gpt, kind: nutrition, listen_addr: 127.0.0.1:8201}\n"), 0o644)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Agents[0].Kind != "nutrition" {
		t.Errorf("kind not parsed: %q", c.Agents[0].Kind)
	}
}

func TestLoadCommandWorkdir(t *testing.T) {
	p := writeTmp(t, `
agents:
  - id: openai
    name: OpenAI SDK Agent
    model: openai/gpt-5.4
    listen_addr: 127.0.0.1:8301
    workdir: /tmp/shim
    command: ["uv", "run", "python", "main.py"]
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	a := cfg.Agents[0]
	if a.WorkDir != "/tmp/shim" {
		t.Errorf("workdir = %q, want /tmp/shim", a.WorkDir)
	}
	if len(a.Command) != 4 || a.Command[0] != "uv" || a.Command[3] != "main.py" {
		t.Errorf("command = %v, want [uv run python main.py]", a.Command)
	}
}

func TestLoad_DuplicateID(t *testing.T) {
	p := writeTmp(t, `
agents:
  - {id: a, name: A, model: m, listen_addr: 127.0.0.1:8101}
  - {id: a, name: A2, model: m, listen_addr: 127.0.0.1:8102}
`)
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for duplicate id")
	}
}

func TestLoad_DuplicateAddr(t *testing.T) {
	p := writeTmp(t, `
agents:
  - {id: a, name: A, model: m, listen_addr: 127.0.0.1:8101}
  - {id: b, name: B, model: m, listen_addr: 127.0.0.1:8101}
`)
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for duplicate listen_addr")
	}
}

func TestLoad_MissingFields(t *testing.T) {
	p := writeTmp(t, `
agents:
  - {id: a, name: A, model: m}
`)
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for missing listen_addr")
	}
}

func TestLoad_NoAgents(t *testing.T) {
	p := writeTmp(t, `agents: []`)
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for empty agents list")
	}
}

func TestLoad_WithTokens(t *testing.T) {
	p := writeTmp(t, `
agents:
  - {id: a, name: A, model: m, listen_addr: 127.0.0.1:8101}
tokens:
  - {token: "abc", label: "ci"}
  - {token: "xyz", label: "ops"}
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Tokens) != 2 {
		t.Fatalf("tokens = %d, want 2", len(cfg.Tokens))
	}
	tm := cfg.TokenMap()
	if tm["abc"] != "ci" || tm["xyz"] != "ops" {
		t.Fatalf("TokenMap wrong: %+v", tm)
	}
}

func TestLoad_NoTokensIsValid(t *testing.T) {
	p := writeTmp(t, `
agents:
  - {id: a, name: A, model: m, listen_addr: 127.0.0.1:8101}
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Tokens) != 0 || len(cfg.TokenMap()) != 0 {
		t.Fatalf("expected no tokens")
	}
}

func TestLoad_DuplicateToken(t *testing.T) {
	p := writeTmp(t, `
agents:
  - {id: a, name: A, model: m, listen_addr: 127.0.0.1:8101}
tokens:
  - {token: "dup", label: "one"}
  - {token: "dup", label: "two"}
`)
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for duplicate token")
	}
}

func TestLoad_EmptyTokenString(t *testing.T) {
	p := writeTmp(t, `
agents:
  - {id: a, name: A, model: m, listen_addr: 127.0.0.1:8101}
tokens:
  - {token: "", label: "x"}
`)
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for empty token")
	}
}

func TestLoad_TenantField(t *testing.T) {
	p := writeTmp(t, `
agents:
  - {id: a, name: A, model: m, listen_addr: 127.0.0.1:8101, tenant: alpha}
  - {id: b, name: B, model: m, listen_addr: 127.0.0.1:8102}
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agents[0].Tenant != "alpha" {
		t.Errorf("agent a tenant = %q, want alpha", cfg.Agents[0].Tenant)
	}
	// Absent tenant defaults to "default".
	if cfg.Agents[1].Tenant != "default" {
		t.Errorf("agent b tenant = %q, want default", cfg.Agents[1].Tenant)
	}
}

func TestLoad_MemoryFlag(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime.yaml")
	body := `agents:
  - id: a1
    name: A1
    model: test/scripted
    listen_addr: "127.0.0.1:9101"
    memory: true
  - id: a2
    name: A2
    model: test/scripted
    listen_addr: "127.0.0.1:9102"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Agents[0].Memory {
		t.Fatal("agent a1 should have memory enabled")
	}
	if cfg.Agents[1].Memory {
		t.Fatal("agent a2 should default memory to false")
	}
}

func TestAgentTenants(t *testing.T) {
	p := writeTmp(t, `
agents:
  - {id: a, name: A, model: m, listen_addr: 127.0.0.1:8101, tenant: alpha}
  - {id: b, name: B, model: m, listen_addr: 127.0.0.1:8102, tenant: beta}
`)
	cfg, _ := Load(p)
	m := cfg.AgentTenants()
	if m["a"] != "alpha" || m["b"] != "beta" {
		t.Fatalf("AgentTenants = %+v", m)
	}
}

func TestGatewayConfigValidation(t *testing.T) {
	base := func() *Config {
		return &Config{Agents: []AgentConfig{
			{ID: "a", Name: "A", Model: "m", ListenAddr: "127.0.0.1:1"},
		}}
	}

	t.Run("valid stdio and http servers", func(t *testing.T) {
		c := base()
		c.Gateway = GatewayConfig{Servers: []GatewayServer{
			{Name: "fs", Command: "npx", Args: []string{"-y", "server-fs"}},
			{Name: "web", URL: "https://example.com/mcp"},
		}}
		if err := c.Validate(); err != nil {
			t.Fatalf("expected valid, got %v", err)
		}
	})

	t.Run("empty gateway section is fine", func(t *testing.T) {
		c := base()
		if err := c.Validate(); err != nil {
			t.Fatalf("expected valid, got %v", err)
		}
	})

	t.Run("server requires name", func(t *testing.T) {
		c := base()
		c.Gateway = GatewayConfig{Servers: []GatewayServer{{URL: "https://x/mcp"}}}
		if err := c.Validate(); err == nil {
			t.Fatal("expected error for missing name")
		}
	})

	t.Run("duplicate server names rejected", func(t *testing.T) {
		c := base()
		c.Gateway = GatewayConfig{Servers: []GatewayServer{
			{Name: "fs", URL: "https://a/mcp"},
			{Name: "fs", URL: "https://b/mcp"},
		}}
		if err := c.Validate(); err == nil {
			t.Fatal("expected error for duplicate name")
		}
	})

	t.Run("command and url mutually exclusive", func(t *testing.T) {
		c := base()
		c.Gateway = GatewayConfig{Servers: []GatewayServer{
			{Name: "fs", Command: "npx", URL: "https://x/mcp"},
		}}
		if err := c.Validate(); err == nil {
			t.Fatal("expected error for both command and url")
		}
	})

	t.Run("one of command or url required", func(t *testing.T) {
		c := base()
		c.Gateway = GatewayConfig{Servers: []GatewayServer{{Name: "fs"}}}
		if err := c.Validate(); err == nil {
			t.Fatal("expected error for neither command nor url")
		}
	})
}

func TestGatewayEnvExpansion(t *testing.T) {
	t.Setenv("GW_TEST_TOKEN", "sekrit")

	t.Run("expands ${VAR} in headers env and agent_keys", func(t *testing.T) {
		c := &Config{
			Agents: []AgentConfig{{ID: "a", Name: "A", Model: "m", ListenAddr: "127.0.0.1:1"}},
			Gateway: GatewayConfig{
				Servers: []GatewayServer{{
					Name: "web", URL: "https://x/mcp",
					Headers: map[string]string{"Authorization": "Bearer ${GW_TEST_TOKEN}"},
					Env:     map[string]string{"TOKEN": "${GW_TEST_TOKEN}"},
				}},
				AgentKeys: map[string]string{"default": "${GW_TEST_TOKEN}"},
			},
		}
		if err := c.Validate(); err != nil {
			t.Fatalf("expected valid, got %v", err)
		}
		if got := c.Gateway.Servers[0].Headers["Authorization"]; got != "Bearer sekrit" {
			t.Fatalf("header not expanded: %q", got)
		}
		if got := c.Gateway.Servers[0].Env["TOKEN"]; got != "sekrit" {
			t.Fatalf("env not expanded: %q", got)
		}
		if got := c.Gateway.AgentKeys["default"]; got != "sekrit" {
			t.Fatalf("agent key not expanded: %q", got)
		}
	})

	t.Run("unset var is a load error", func(t *testing.T) {
		c := &Config{
			Agents: []AgentConfig{{ID: "a", Name: "A", Model: "m", ListenAddr: "127.0.0.1:1"}},
			Gateway: GatewayConfig{Servers: []GatewayServer{{
				Name: "web", URL: "https://x/mcp",
				Headers: map[string]string{"Authorization": "Bearer ${GW_UNSET_VAR_XYZ}"},
			}}},
		}
		if err := c.Validate(); err == nil {
			t.Fatal("expected error for unset env var")
		}
	})

	t.Run("expands ${VAR} in openapi and base_url", func(t *testing.T) {
		t.Setenv("SPEC_URL", "https://api.example.com/openapi.json")
		t.Setenv("API_BASE", "https://api.example.com/v2")
		c := &Config{
			Agents: []AgentConfig{{ID: "a", Name: "A", Model: "m", ListenAddr: "127.0.0.1:1"}},
			Gateway: GatewayConfig{Servers: []GatewayServer{{
				Name:    "orders",
				OpenAPI: "${SPEC_URL}",
				BaseURL: "${API_BASE}",
			}}},
		}
		if err := c.Validate(); err != nil {
			t.Fatalf("expected valid, got %v", err)
		}
		if got := c.Gateway.Servers[0].OpenAPI; got != "https://api.example.com/openapi.json" {
			t.Fatalf("openapi not expanded: %q", got)
		}
		if got := c.Gateway.Servers[0].BaseURL; got != "https://api.example.com/v2" {
			t.Fatalf("base_url not expanded: %q", got)
		}
	})

	t.Run("unset var in openapi is a load error", func(t *testing.T) {
		c := &Config{
			Agents: []AgentConfig{{ID: "a", Name: "A", Model: "m", ListenAddr: "127.0.0.1:1"}},
			Gateway: GatewayConfig{Servers: []GatewayServer{{
				Name:    "orders",
				OpenAPI: "${GW_UNSET_VAR_XYZ}",
			}}},
		}
		err := c.Validate()
		if err == nil {
			t.Fatal("expected error for unset env var in openapi")
		}
		if !strings.Contains(err.Error(), "GW_UNSET_VAR_XYZ") {
			t.Fatalf("error should name the missing var: %v", err)
		}
	})

	t.Run("unset var in base_url is a load error", func(t *testing.T) {
		t.Setenv("SPEC_URL", "https://api.example.com/openapi.json")
		c := &Config{
			Agents: []AgentConfig{{ID: "a", Name: "A", Model: "m", ListenAddr: "127.0.0.1:1"}},
			Gateway: GatewayConfig{Servers: []GatewayServer{{
				Name:    "orders",
				OpenAPI: "${SPEC_URL}",
				BaseURL: "${GW_UNSET_VAR_XYZ}",
			}}},
		}
		err := c.Validate()
		if err == nil {
			t.Fatal("expected error for unset env var in base_url")
		}
		if !strings.Contains(err.Error(), "GW_UNSET_VAR_XYZ") {
			t.Fatalf("error should name the missing var: %v", err)
		}
	})

	t.Run("literal values pass through", func(t *testing.T) {
		c := &Config{
			Agents: []AgentConfig{{ID: "a", Name: "A", Model: "m", ListenAddr: "127.0.0.1:1"}},
			Gateway: GatewayConfig{Servers: []GatewayServer{{
				Name: "web", URL: "https://x/mcp",
				Headers: map[string]string{"X-Plain": "no-vars-here"},
			}}},
		}
		if err := c.Validate(); err != nil {
			t.Fatalf("expected valid, got %v", err)
		}
		if got := c.Gateway.Servers[0].Headers["X-Plain"]; got != "no-vars-here" {
			t.Fatalf("literal mangled: %q", got)
		}
	})
}

func TestAgentConfigGatewayFlag(t *testing.T) {
	c := &Config{
		Agents: []AgentConfig{
			{ID: "a", Name: "A", Model: "m", ListenAddr: "127.0.0.1:1", Gateway: GatewayFull},
		},
		Gateway: GatewayConfig{Servers: []GatewayServer{{Name: "fs", Command: "x"}}},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("expected valid, got %v", err)
	}
	if c.Agents[0].Gateway != GatewayFull {
		t.Fatal("gateway flag lost")
	}
}

func TestGatewayModeYAML(t *testing.T) {
	load := func(t *testing.T, gatewayVal string) (*Config, error) {
		t.Helper()
		dir := t.TempDir()
		p := dir + "/runtime.yaml"
		// A servers entry is present so gateway-enabled agents pass the
		// agents-require-servers validation; it is inert for the off cases.
		y := "agents:\n  - {id: a, name: A, model: m, listen_addr: 127.0.0.1:1" + gatewayVal + "}\n" +
			"gateway:\n  servers:\n    - {name: fs, command: x}\n"
		if err := os.WriteFile(p, []byte(y), 0o644); err != nil {
			t.Fatal(err)
		}
		return Load(p)
	}

	t.Run("absent means off", func(t *testing.T) {
		c, err := load(t, "")
		if err != nil {
			t.Fatal(err)
		}
		if c.Agents[0].Gateway != GatewayOff {
			t.Fatalf("want off, got %v", c.Agents[0].Gateway)
		}
		if c.Agents[0].Gateway.Enabled() {
			t.Fatal("off must not be enabled")
		}
	})

	t.Run("true means full", func(t *testing.T) {
		c, err := load(t, ", gateway: true")
		if err != nil {
			t.Fatal(err)
		}
		if c.Agents[0].Gateway != GatewayFull {
			t.Fatalf("want full, got %v", c.Agents[0].Gateway)
		}
		if !c.Agents[0].Gateway.Enabled() {
			t.Fatal("full must be enabled")
		}
	})

	t.Run("false means off", func(t *testing.T) {
		c, err := load(t, ", gateway: false")
		if err != nil {
			t.Fatal(err)
		}
		if c.Agents[0].Gateway != GatewayOff {
			t.Fatalf("want off, got %v", c.Agents[0].Gateway)
		}
	})

	t.Run("search string", func(t *testing.T) {
		c, err := load(t, ", gateway: search")
		if err != nil {
			t.Fatal(err)
		}
		if c.Agents[0].Gateway != GatewaySearch {
			t.Fatalf("want search, got %v", c.Agents[0].Gateway)
		}
		if !c.Agents[0].Gateway.Enabled() {
			t.Fatal("search must be enabled")
		}
	})

	t.Run("full string", func(t *testing.T) {
		c, err := load(t, ", gateway: full")
		if err != nil {
			t.Fatal(err)
		}
		if c.Agents[0].Gateway != GatewayFull {
			t.Fatalf("want full, got %v", c.Agents[0].Gateway)
		}
	})

	t.Run("invalid string rejected at load", func(t *testing.T) {
		if _, err := load(t, ", gateway: banana"); err == nil {
			t.Fatal("expected load error for invalid gateway mode")
		}
	})
}

func TestGatewayForwardTenantParsesAndValidates(t *testing.T) {
	p := writeTmp(t, `
agents:
  - {id: a, name: A, model: m, listen_addr: 127.0.0.1:8101}
gateway:
  servers:
    - {name: sandbox, command: sandboxd, forward_tenant: true}
    - {name: fs, command: npx}
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Gateway.Servers[0].ForwardTenant {
		t.Error("sandbox: forward_tenant: true should parse as ForwardTenant == true")
	}
	if cfg.Gateway.Servers[1].ForwardTenant {
		t.Error("fs: absent forward_tenant should default to false")
	}
}

func TestGatewayForwardTenantRejectsHTTPUpstream(t *testing.T) {
	p := writeTmp(t, `
agents:
  - {id: a, name: A, model: m, listen_addr: 127.0.0.1:8101}
gateway:
  servers:
    - {name: web, url: "https://example.com/mcp", forward_tenant: true}
`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error: forward_tenant on an HTTP (url:) upstream")
	}
	if !strings.Contains(err.Error(), "forward_tenant") {
		t.Fatalf("error should mention forward_tenant, got: %v", err)
	}
}

// TestGatewayServerNameRejectsDoubleUnderscore: tool names are
// <server>__<tool> and the gateway resolves the owning server by cutting at
// the FIRST "__". A server name containing "__" (e.g. "a__b") would alias
// against server "a" and could silently disable tenant forwarding.
func TestGatewayServerNameRejectsDoubleUnderscore(t *testing.T) {
	p := writeTmp(t, `
agents:
  - {id: a, name: A, model: m, listen_addr: 127.0.0.1:8101}
gateway:
  servers:
    - {name: a__b, command: x}
`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error: gateway server name containing \"__\"")
	}
	if !strings.Contains(err.Error(), "__") {
		t.Fatalf("error should mention \"__\", got: %v", err)
	}
}

func TestGatewayOpenAPIServerParses(t *testing.T) {
	p := writeTmp(t, `
agents:
  - {id: a, name: A, model: m, listen_addr: 127.0.0.1:8101}
gateway:
  servers:
    - name: orders
      openapi: ./specs/orders.yaml
      base_url: https://orders.internal
      operations:
        - listOrders
        - "GET /orders/*"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s := cfg.Gateway.Servers[0]
	if s.OpenAPI != "./specs/orders.yaml" {
		t.Errorf("OpenAPI = %q, want ./specs/orders.yaml", s.OpenAPI)
	}
	if s.BaseURL != "https://orders.internal" {
		t.Errorf("BaseURL = %q, want https://orders.internal", s.BaseURL)
	}
	if len(s.Operations) != 2 || s.Operations[0] != "listOrders" || s.Operations[1] != "GET /orders/*" {
		t.Errorf("Operations = %v, want [listOrders, GET /orders/*]", s.Operations)
	}
}

func TestGatewayTransportExactlyOne(t *testing.T) {
	base := func(srv GatewayServer) *Config {
		srv.Name = "s"
		return &Config{
			Agents:  []AgentConfig{{ID: "a", Name: "A", Model: "m", ListenAddr: "127.0.0.1:1"}},
			Gateway: GatewayConfig{Servers: []GatewayServer{srv}},
		}
	}
	cases := []struct {
		name    string
		srv     GatewayServer
		wantErr bool
	}{
		{"openapi only", GatewayServer{OpenAPI: "spec.yaml"}, false},
		{"command only", GatewayServer{Command: "npx"}, false},
		{"url only", GatewayServer{URL: "https://x/mcp"}, false},
		{"openapi and url", GatewayServer{OpenAPI: "spec.yaml", URL: "https://x/mcp"}, true},
		{"openapi and command", GatewayServer{OpenAPI: "spec.yaml", Command: "npx"}, true},
		{"none", GatewayServer{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := base(tc.srv).Validate()
			if tc.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected valid, got %v", err)
			}
			if tc.wantErr && !strings.Contains(err.Error(), "requires exactly one of command, url, or openapi") {
				t.Fatalf("error should mention exactly-one-of rule, got: %v", err)
			}
		})
	}
}

func TestGatewayOpenAPIFieldRules(t *testing.T) {
	base := func(srv GatewayServer) *Config {
		srv.Name = "s"
		return &Config{
			Agents:  []AgentConfig{{ID: "a", Name: "A", Model: "m", ListenAddr: "127.0.0.1:1"}},
			Gateway: GatewayConfig{Servers: []GatewayServer{srv}},
		}
	}
	cases := []struct {
		name    string
		srv     GatewayServer
		errPart string // "" ⇒ expect valid
	}{
		{"forward_tenant with openapi rejected",
			GatewayServer{OpenAPI: "spec.yaml", ForwardTenant: true}, "forward_tenant"},
		{"base_url without openapi rejected",
			GatewayServer{URL: "https://x/mcp", BaseURL: "https://api.example.com"}, "base_url"},
		{"operations without openapi rejected",
			GatewayServer{Command: "npx", Operations: []string{"listOrders"}}, "operations"},
		{"lowercase method pattern rejected",
			GatewayServer{OpenAPI: "spec.yaml", Operations: []string{"get /x"}}, "operations"},
		{"forward_tenant with command still OK",
			GatewayServer{Command: "sandboxd", ForwardTenant: true}, ""},
		{"base_url and operations with openapi OK",
			GatewayServer{OpenAPI: "spec.yaml", BaseURL: "https://api.example.com", Operations: []string{"GET /orders/*"}}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := base(tc.srv).Validate()
			if tc.errPart == "" {
				if err != nil {
					t.Fatalf("expected valid, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.errPart) {
				t.Fatalf("error should contain %q, got: %v", tc.errPart, err)
			}
		})
	}
}

func TestValidateOperationPattern(t *testing.T) {
	valid := []string{"listOrders", "GET /orders/*", "DELETE /a/{id}"}
	for _, p := range valid {
		if err := validateOperationPattern(p); err != nil {
			t.Errorf("pattern %q should be valid, got: %v", p, err)
		}
	}
	invalid := []string{"", "get /x", "GET orders", "GET /[x"}
	for _, p := range invalid {
		if err := validateOperationPattern(p); err == nil {
			t.Errorf("pattern %q should be rejected", p)
		}
	}
	// Bad glob through full config validation carries the server name.
	c := &Config{
		Agents: []AgentConfig{{ID: "a", Name: "A", Model: "m", ListenAddr: "127.0.0.1:1"}},
		Gateway: GatewayConfig{Servers: []GatewayServer{
			{Name: "orders", OpenAPI: "spec.yaml", Operations: []string{"GET /[x"}},
		}},
	}
	err := c.Validate()
	if err == nil {
		t.Fatal("expected error for bad glob in operations")
	}
	if !strings.Contains(err.Error(), "orders") {
		t.Fatalf("error should name the server, got: %v", err)
	}
}

func TestGatewayAgentRequiresServers(t *testing.T) {
	c := &Config{Agents: []AgentConfig{
		{ID: "a", Name: "A", Model: "m", ListenAddr: "127.0.0.1:1", Gateway: GatewayFull},
	}}
	if err := c.Validate(); err == nil {
		t.Fatal("expected error: gateway agent without gateway.servers")
	}
	c.Gateway = GatewayConfig{Servers: []GatewayServer{{Name: "fs", Command: "x"}}}
	if err := c.Validate(); err != nil {
		t.Fatalf("with servers should validate: %v", err)
	}
}
