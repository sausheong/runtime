// Package gateway federates upstream MCP servers into one tenant-filtered MCP
// endpoint on the control plane. The Manager owns upstream lifecycle
// (connect, degrade, reconnect); server.go exposes the federated tool set as
// per-tenant MCP servers over Streamable HTTP.
package gateway

import (
	"context"

	"github.com/sausheong/harness/tool"
	hmcp "github.com/sausheong/harness/tools/mcp"

	"github.com/sausheong/runtime/internal/config"
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

// dialHarness is the production dialFunc: it maps config.GatewayServer onto
// harness mcp.ServerConfig and Connects (stdio or Streamable HTTP).
func dialHarness(ctx context.Context, s config.GatewayServer) (upstreamConn, error) {
	return hmcp.Connect(ctx, hmcp.ServerConfig{
		Name:    s.Name,
		Command: s.Command,
		Args:    s.Args,
		Env:     s.Env,
		URL:     s.URL,
		Headers: s.Headers,
	})
}
