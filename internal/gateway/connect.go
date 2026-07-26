// Package gateway federates upstream MCP servers into one tenant-filtered MCP
// endpoint on the control plane. The Manager owns upstream lifecycle
// (connect, degrade, reconnect); server.go exposes the federated tool set as
// per-tenant MCP servers over Streamable HTTP.
package gateway

import (
	"context"
	"maps"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/sausheong/harness/tool"
	hmcp "github.com/sausheong/harness/tools/mcp"

	"github.com/sausheong/runtime/internal/config"
	"github.com/sausheong/runtime/internal/netpolicy"
)

// upstreamConn is the connected form of one upstream: its adapted tools, a
// liveness probe, and a closer. Satisfied by *hmcp.Client in production and
// by fakes in tests.
type upstreamConn interface {
	Tools() []tool.Tool
	// Ping verifies the session is still alive (MCP ping). A non-nil error
	// means the connection is unusable and should be marked down.
	Ping(ctx context.Context) error
	Close() error
}

// dialFunc connects one configured upstream. Swapped in tests.
type dialFunc func(ctx context.Context, s config.GatewayServer) (upstreamConn, error)

// dialProduction routes each transport: openapi: → REST adapter,
// command:/url: → harness MCP.
func dialProduction(ctx context.Context, s config.GatewayServer) (upstreamConn, error) {
	if s.OpenAPI != "" {
		return dialOpenAPI(ctx, s)
	}
	return dialHarness(ctx, s)
}

// dialHarness is the harness-MCP dialFunc: it maps config.GatewayServer onto
// harness mcp.ServerConfig and Connects (stdio or Streamable HTTP).
func dialHarness(ctx context.Context, s config.GatewayServer) (upstreamConn, error) {
	var client *http.Client
	if s.RestrictOutbound && s.URL != "" {
		client = &http.Client{Transport: netpolicy.PublicTransport(), Timeout: 30 * time.Second}
	}
	env := maps.Clone(s.Env)
	if s.Command != "" {
		env = stdioEnv(env)
	}
	return hmcp.Connect(ctx, hmcp.ServerConfig{
		Name:       s.Name,
		Command:    s.Command,
		Args:       s.Args,
		Env:        env,
		URL:        s.URL,
		Headers:    s.Headers,
		HTTPClient: client,
	})
}

// stdioSubsystemPrefixes are inherited variables a stdio child legitimately
// needs: the sandbox and browser subsystems are configured entirely through
// the environment, and runtimed spawns sandboxd and browserd as stdio servers.
//
// Blanking these was a real defect rather than a conservative choice. With
// RUNTIME_BROWSER_NETWORK cleared, browserd skips the internal-network branch
// altogether — no network validation, no private network, and CDP published on
// a host interface — so the egress boundary silently evaporated in exactly the
// deployment that configures it. RUNTIME_*_SCOPE cleared likewise downgraded
// session isolation to tenant scope while the operator guide called session
// scope the shipped default, and RUNTIME_*_RUNTIME cleared dropped gVisor.
//
// These carry subsystem configuration, not control-plane authority: no DSN, no
// provider key, no identity or keyring material has this shape. The browser
// proxy token does, but it is browserd's own process-internal credential
// (browserd serves the proxy that demands it and answers the challenge itself),
// and forwarding it keeps an explicitly pinned value working instead of being
// silently replaced by a generated one.
var stdioSubsystemPrefixes = []string{"RUNTIME_SANDBOX_", "RUNTIME_BROWSER_"}

// stdioEnv neutralizes every inherited parent variable except a small
// process-runtime allowlist. The harness MCP client merges this map over
// os.Environ, so empty values are required to prevent unrelated provider,
// database, identity, and keyring credentials reaching an operator-configured
// stdio server. An explicit server env entry remains authoritative.
func stdioEnv(explicit map[string]string) map[string]string {
	env := maps.Clone(explicit)
	if env == nil {
		env = map[string]string{}
	}
	allowed := map[string]struct{}{
		"HOME": {}, "LANG": {}, "LC_ALL": {}, "LC_CTYPE": {},
		"LOGNAME": {}, "NODE_EXTRA_CA_CERTS": {}, "PATH": {}, "SHELL": {},
		"SSL_CERT_DIR": {}, "SSL_CERT_FILE": {}, "TMPDIR": {}, "TZ": {}, "USER": {},
		"OTEL_EXPORTER_OTLP_ENDPOINT": {}, "RUNTIME_LOG_FORMAT": {},
		"RUNTIME_TRACE_SAMPLE_RATIO": {}, "RUNTIME_TRACING_ENABLED": {},
		// The container-engine endpoint sandboxd and browserd dial. Not a
		// credential; without it they fall back to the default socket, which
		// is wrong wherever the operator pointed the runtime elsewhere.
		"DOCKER_HOST": {},
	}
	for _, entry := range os.Environ() {
		name, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if _, keep := allowed[name]; keep {
			continue
		}
		if hasAnyPrefix(name, stdioSubsystemPrefixes) {
			continue
		}
		if _, configured := env[name]; !configured {
			env[name] = ""
		}
	}
	return env
}

func hasAnyPrefix(name string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}
