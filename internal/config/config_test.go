package config

import (
	"os"
	"path/filepath"
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
