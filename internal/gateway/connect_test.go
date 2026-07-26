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

// TestStdioEnvForwardsSubsystemConfiguration is the regression for a defect
// that silently disabled shipped security controls. runtimed spawns sandboxd
// and browserd as stdio servers, and both are configured entirely through the
// environment. Blanking those variables meant RUNTIME_BROWSER_NETWORK never
// arrived, so browserd skipped the internal-network branch — no validation, no
// private network, CDP on a host interface — in exactly the deployment that
// configures the boundary. Session scope and gVisor were dropped the same way.
func TestStdioEnvForwardsSubsystemConfiguration(t *testing.T) {
	forwarded := map[string]string{
		"RUNTIME_BROWSER_NETWORK":    "runtime_browser-control",
		"RUNTIME_BROWSER_PROXY_HOST": "runtimed",
		"RUNTIME_BROWSER_SCOPE":      "session",
		"RUNTIME_SANDBOX_SCOPE":      "session",
		"RUNTIME_SANDBOX_RUNTIME":    "runsc",
		"DOCKER_HOST":                "unix:///var/run/docker.sock",
	}
	for k, v := range forwarded {
		t.Setenv(k, v)
	}
	env := stdioEnv(nil)
	for k := range forwarded {
		if got, blanked := env[k]; blanked {
			t.Errorf("%s was blanked (child would see %q); subsystem config must reach the stdio child", k, got)
		}
	}
}

// TestStdioEnvStillBlocksControlPlaneCredentials guards the other half of the
// contract: widening the allowlist for subsystem configuration must not let
// control-plane authority reach an operator-configured stdio server.
func TestStdioEnvStillBlocksControlPlaneCredentials(t *testing.T) {
	secrets := []string{
		"RUNTIME_PG_DSN", "RUNTIME_AGENT_PG_DSN", "RUNTIME_SECRETS_KEYS",
		"RUNTIME_ADMIN_BOOTSTRAP", "RUNTIME_IDENTITY_SIGNING_PRIVATE_KEY",
		"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "AWS_SECRET_ACCESS_KEY",
	}
	for _, k := range secrets {
		t.Setenv(k, "leaked-"+k)
	}
	env := stdioEnv(nil)
	for _, k := range secrets {
		if env[k] != "" {
			t.Errorf("%s leaked to stdio child: %q", k, env[k])
		}
	}
}
