// Command sandboxd is the Sandboxes M1 MCP server: an isolated, stateful
// code interpreter (one locked-down Docker container per sandbox session)
// exposed as MCP tools over stdio. It is designed to run as a gateway
// upstream (gateway.servers: command: with forward_tenant: true) — the
// gateway injects the calling principal's tenant as __rt_tenant.
//
// Env:
//
//	RUNTIME_SANDBOX_IMAGE           container image (default runtime-sandbox:latest)
//	RUNTIME_SANDBOX_MAX_PER_TENANT  concurrent sandboxes per tenant (default 5)
//	RUNTIME_SANDBOX_IDLE_TTL        idle close, Go duration (default 10m)
//	RUNTIME_SANDBOX_MAX_LIFETIME    hard close, Go duration (default 1h)
//	RUNTIME_SANDBOX_WORKSPACE_MB    tmpfs /workspace size (default 64)
//	RUNTIME_SANDBOX_MEM_MB          memory limit (default 512)
//	RUNTIME_SANDBOX_CPUS            cpu limit (default 1.0)
//	RUNTIME_SANDBOX_RUNTIME         engine runtime, e.g. runsc (default engine default)
//	RUNTIME_SANDBOX_FAKE            "1" ⇒ in-memory fake backend (tests only)
package main

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sausheong/runtime/internal/sandbox"
)

// envInt reads an integer env var, warning and returning def on bad values.
func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		slog.Warn("sandboxd: bad integer env, using default", "key", key, "value", v, "default", def)
		return def
	}
	return n
}

// envFloat reads a float env var, warning and returning def on bad values.
func envFloat(key string, def float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		slog.Warn("sandboxd: bad float env, using default", "key", key, "value", v, "default", def)
		return def
	}
	return f
}

// envDur reads a Go-duration env var, warning and returning def on bad values.
func envDur(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		slog.Warn("sandboxd: bad duration env, using default", "key", key, "value", v, "default", def)
		return def
	}
	return d
}

func main() {
	// stdout carries the MCP stdio transport; all logging goes to stderr.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	var be sandbox.Backend
	if os.Getenv("RUNTIME_SANDBOX_FAKE") == "1" {
		slog.Warn("sandboxd: RUNTIME_SANDBOX_FAKE=1 — using in-memory fake backend (tests only)")
		be = sandbox.NewFakeBackend()
	} else {
		var err error
		be, err = sandbox.NewDockerBackend(sandbox.DockerConfig{
			Image:       os.Getenv("RUNTIME_SANDBOX_IMAGE"),
			WorkspaceMB: envInt("RUNTIME_SANDBOX_WORKSPACE_MB", 64),
			MemMB:       int64(envInt("RUNTIME_SANDBOX_MEM_MB", 512)),
			CPUs:        envFloat("RUNTIME_SANDBOX_CPUS", 1.0),
			Runtime:     os.Getenv("RUNTIME_SANDBOX_RUNTIME"),
		})
		if err != nil {
			slog.Error("sandboxd: docker backend init failed", "err", err)
			os.Exit(1)
		}
	}

	m := sandbox.NewManager(be, sandbox.Config{
		MaxPerTenant: envInt("RUNTIME_SANDBOX_MAX_PER_TENANT", 5),
		IdleTTL:      envDur("RUNTIME_SANDBOX_IDLE_TTL", 10*time.Minute),
		MaxLifetime:  envDur("RUNTIME_SANDBOX_MAX_LIFETIME", time.Hour),
	})

	ctx := context.Background()
	// Crash recovery: remove leftover sandbox containers from a previous run.
	// A down daemon is not fatal — it degrades per-call instead.
	if err := m.ReapStartup(ctx); err != nil {
		slog.Warn("sandboxd: startup reap failed", "err", err)
	}
	m.StartReaper(ctx, time.Minute)

	srv := sandbox.NewServer(m)
	if err := srv.Run(ctx, &sdk.StdioTransport{}); err != nil {
		slog.Error("sandboxd: server exited", "err", err)
		os.Exit(1)
	}
}
