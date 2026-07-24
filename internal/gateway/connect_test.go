package gateway

import "testing"

func TestStdioEnvNeutralizesParentSecrets(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "operator-secret")
	t.Setenv("RUNTIME_PG_DSN", "control-dsn")
	t.Setenv("PATH", "/safe/path")

	env := stdioEnv(map[string]string{"UPSTREAM_TOKEN": "scoped", "OPENAI_API_KEY": "explicit"})
	if env["RUNTIME_PG_DSN"] != "" {
		t.Fatalf("control-plane DSN leaked: %q", env["RUNTIME_PG_DSN"])
	}
	if env["OPENAI_API_KEY"] != "explicit" {
		t.Fatalf("explicit upstream env was not preserved: %q", env["OPENAI_API_KEY"])
	}
	if env["UPSTREAM_TOKEN"] != "scoped" {
		t.Fatalf("scoped upstream token was not preserved: %q", env["UPSTREAM_TOKEN"])
	}
	if _, shadowed := env["PATH"]; shadowed {
		t.Fatal("safe process variable should remain inherited")
	}
}
