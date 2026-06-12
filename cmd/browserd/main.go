// Command browserd is the Sandboxes M2 MCP server: isolated headless-browser
// sandboxes (one locked-down Chromium container per session) exposed as MCP
// tools over stdio, designed to run as a gateway upstream with
// forward_tenant: true. Chrome's entire network stack is forced through the
// in-process egress proxy via --proxy-server; the agent can only drive Chrome
// over CDP, so the proxy adjudicates all reachable traffic by hostname.
//
// Run exactly one browserd per host (or per DOCKER_HOST): reap-on-start removes
// ALL runtime.browser=1 containers.
//
// Env:
//
//	RUNTIME_BROWSER_IMAGE           container image (default runtime-browser:latest)
//	RUNTIME_BROWSER_MAX_PER_TENANT  concurrent browsers per tenant (default 5)
//	RUNTIME_BROWSER_IDLE_TTL        idle close, Go duration (default 10m)
//	RUNTIME_BROWSER_MAX_LIFETIME    hard close, Go duration (default 1h)
//	RUNTIME_BROWSER_MEM_MB          memory limit (default 1024)
//	RUNTIME_BROWSER_CPUS            cpu limit (default 1.0)
//	RUNTIME_BROWSER_PROFILE_MB      tmpfs profile size (default 256)
//	RUNTIME_BROWSER_RUNTIME         engine runtime, e.g. runsc
//	RUNTIME_BROWSER_EGRESS_MODE     deny-all | allow-list | allow-all-public (default deny-all)
//	RUNTIME_BROWSER_EGRESS_ALLOW    comma-separated hostname globs (allow-list mode)
//	RUNTIME_BROWSER_PROXY_ADDR      host:port the egress proxy listens on (default 0.0.0.0:0 → ephemeral, all interfaces). Binding all interfaces is safe: the proxy is Policy-gated (deny-all by default) so it only ever grants policy-permitted egress, and the container reaches it via host.docker.internal. Pin to a specific IP (e.g. the docker bridge gateway) to tighten the bind.
//	RUNTIME_BROWSER_ALLOW_DIRECT    "1" ⇒ accept calls without the gateway's __rt_tenant key
//	RUNTIME_BROWSER_FAKE            "1" ⇒ in-memory fake backend (tests only)
package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sausheong/runtime/internal/browser"
)

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		slog.Warn("browserd: bad integer env, using default", "key", key, "value", v, "default", def)
		return def
	}
	return n
}

func envFloat(key string, def float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		slog.Warn("browserd: bad float env, using default", "key", key, "value", v, "default", def)
		return def
	}
	return f
}

func envDur(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		slog.Warn("browserd: bad duration env, using default", "key", key, "value", v, "default", def)
		return def
	}
	return d
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	// Egress policy.
	mode := os.Getenv("RUNTIME_BROWSER_EGRESS_MODE")
	if mode == "" {
		mode = browser.ModeDenyAll
	}
	var allow []string
	if a := os.Getenv("RUNTIME_BROWSER_EGRESS_ALLOW"); a != "" {
		for _, g := range strings.Split(a, ",") {
			if g = strings.TrimSpace(g); g != "" {
				allow = append(allow, g)
			}
		}
	}
	policy, err := browser.NewPolicy(mode, allow)
	if err != nil {
		slog.Error("browserd: bad egress policy", "err", err)
		os.Exit(1)
	}

	// Start the egress proxy on a listener the containers can reach.
	proxyAddr := os.Getenv("RUNTIME_BROWSER_PROXY_ADDR")
	if proxyAddr == "" {
		proxyAddr = "0.0.0.0:0"
	}
	ln, err := net.Listen("tcp", proxyAddr)
	if err != nil {
		slog.Error("browserd: egress proxy listen failed", "addr", proxyAddr, "err", err)
		os.Exit(1)
	}
	// The egress proxy runs for the life of the process. If it ever exits, we
	// log but keep serving MCP: with no proxy, every container's egress fails
	// closed (deny), which is safe — degrade-don't-crash, like sandboxd's
	// per-call backend degradation. A loopback TCP listener dying mid-run is
	// near-impossible in practice.
	proxy := browser.NewProxy(policy)
	go func() {
		if err := http.Serve(ln, proxy); err != nil {
			slog.Error("browserd: egress proxy exited", "err", err)
		}
	}()
	actualProxyAddr := ln.Addr().String()
	slog.Info("browserd: egress proxy listening", "addr", actualProxyAddr, "mode", mode)

	var be browser.Backend
	if os.Getenv("RUNTIME_BROWSER_FAKE") == "1" {
		slog.Warn("browserd: RUNTIME_BROWSER_FAKE=1 — in-memory fake backend (tests only)")
		be = browser.NewFakeBackend()
	} else {
		be, err = browser.NewDockerBackend(browser.DockerConfig{
			Image:     os.Getenv("RUNTIME_BROWSER_IMAGE"),
			MemMB:     int64(envInt("RUNTIME_BROWSER_MEM_MB", 1024)),
			CPUs:      envFloat("RUNTIME_BROWSER_CPUS", 1.0),
			ProfileMB: envInt("RUNTIME_BROWSER_PROFILE_MB", 256),
			Runtime:   os.Getenv("RUNTIME_BROWSER_RUNTIME"),
		})
		if err != nil {
			slog.Error("browserd: docker backend init failed", "err", err)
			os.Exit(1)
		}
	}

	m := browser.NewManager(be, browser.Config{
		MaxPerTenant: envInt("RUNTIME_BROWSER_MAX_PER_TENANT", 5),
		IdleTTL:      envDur("RUNTIME_BROWSER_IDLE_TTL", 10*time.Minute),
		MaxLifetime:  envDur("RUNTIME_BROWSER_MAX_LIFETIME", time.Hour),
		ProxyAddr:    actualProxyAddr,
	})

	ctx := context.Background()
	if err := m.ReapStartup(ctx); err != nil {
		slog.Warn("browserd: startup reap failed", "err", err)
	}
	m.StartReaper(ctx, time.Minute)

	// No SIGTERM handler by design: if browserd dies without cleanup, the
	// leftover runtime.browser=1 containers are recovered by reap-on-start
	// above (same rationale as sandboxd).
	allowDirect := os.Getenv("RUNTIME_BROWSER_ALLOW_DIRECT") == "1"
	srv := browser.NewServer(m, allowDirect)
	if err := srv.Run(ctx, &sdk.StdioTransport{}); err != nil {
		slog.Error("browserd: server exited", "err", err)
		os.Exit(1)
	}
}
