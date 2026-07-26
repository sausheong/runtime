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
//	RUNTIME_BROWSER_NETWORK         private Docker network for browser/CDP traffic;
//	                                MUST be declared internal (no default route) —
//	                                startup inspects it and refuses a routable one
//	RUNTIME_BROWSER_PROXY_HOST      browserd hostname on that private network
//	RUNTIME_BROWSER_EGRESS_MODE     deny-all | allow-list | allow-all-public (default deny-all)
//	RUNTIME_BROWSER_EGRESS_ALLOW    comma-separated hostname globs (allow-list mode)
//	RUNTIME_BROWSER_PROXY_ADDR      host:port the egress proxy listens on. Default
//	                                127.0.0.1:0, or 0.0.0.0:0 when
//	                                RUNTIME_BROWSER_NETWORK is set (a loopback bind
//	                                is unreachable from a private network)
//	RUNTIME_BROWSER_PROXY_TOKEN     required (minimum 32 characters) for every non-loopback
//	                                proxy bind, hence for every private-network deployment
//	RUNTIME_BROWSER_ALLOW_DIRECT    "1" ⇒ accept calls without the gateway's __rt_tenant key
//	RUNTIME_BROWSER_SCOPE           "session" ⇒ key browsers by (tenant, session,
//	                                id) so a handle is invisible to other sessions
//	                                of the same tenant, and close_session reaps a
//	                                session's browsers at session end. Any other
//	                                value (default) ⇒ tenant-scoped (today's behavior).
//	RUNTIME_BROWSER_FAKE            "1" ⇒ in-memory fake backend (tests only)
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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

// randomProxyToken mints an ephemeral egress-proxy credential. 32 bytes of
// crypto/rand rendered as 64 hex characters, comfortably past
// ValidateProxyListener's 32-character floor.
func randomProxyToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
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
	if err != nil || f <= 0 {
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
	if err != nil || d <= 0 {
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
	//
	// An explicit RUNTIME_BROWSER_PROXY_ADDR always wins. Otherwise the right
	// default depends on the deployment shape:
	//
	//   - No private network (direct-host install): browser containers reach
	//     the proxy over the host gateway, so 127.0.0.1:0 is both reachable
	//     and the tightest possible bind.
	//   - Private network set: a loopback bind is NOT reachable from that
	//     network — containerProxyAddrForHost would advertise
	//     RUNTIME_BROWSER_PROXY_HOST:<port> pointing at a listener bound to
	//     another container's loopback, and Chromium fails to connect. Bind
	//     the wildcard instead.
	//
	// The wildcard bind is safe in the private-network shape precisely because
	// it is not unauthenticated: ValidateProxyListener below rejects any
	// non-loopback bind without a >=32-character token. The credential reaches
	// Chromium over CDP (Fetch.authChallenge), never via container env.
	browserNetwork := os.Getenv("RUNTIME_BROWSER_NETWORK")
	proxyAddr := os.Getenv("RUNTIME_BROWSER_PROXY_ADDR")
	if proxyAddr == "" {
		proxyAddr = "127.0.0.1:0"
		if browserNetwork != "" {
			proxyAddr = "0.0.0.0:0"
		}
	}
	// This token is process-internal: browserd serves the proxy that demands it
	// and answers that demand itself over CDP. Nothing outside this process ever
	// needs the value, so requiring an operator to supply one would be busywork
	// with an upgrade hazard attached. Generate one per start when the bind
	// needs it and none was given; a fresh token per process is also strictly
	// better than a static one on disk. RUNTIME_BROWSER_PROXY_TOKEN remains an
	// override for deployments that want to pin the value.
	proxyToken := os.Getenv("RUNTIME_BROWSER_PROXY_TOKEN")
	if proxyToken == "" {
		if err := browser.ValidateProxyListener(proxyAddr, proxyToken); err != nil {
			generated, genErr := randomProxyToken()
			if genErr != nil {
				slog.Error("browserd: cannot generate egress proxy token", "err", genErr)
				os.Exit(1)
			}
			proxyToken = generated
			slog.Info("browserd: generated an ephemeral egress proxy token",
				"addr", proxyAddr,
				"reason", "non-loopback bind requires authentication and none was configured")
		}
	}
	if err := browser.ValidateProxyListener(proxyAddr, proxyToken); err != nil {
		slog.Error("browserd: insecure egress proxy listener", "addr", proxyAddr, "err", err)
		os.Exit(1)
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
	proxy := browser.NewProxyWithConfig(policy, browser.ProxyConfig{
		AuthToken:      proxyToken,
		MaxRequests:    envInt("RUNTIME_BROWSER_PROXY_MAX_REQUESTS", 128),
		MaxTunnels:     envInt("RUNTIME_BROWSER_PROXY_MAX_TUNNELS", 64),
		ResponseBytes:  int64(envInt("RUNTIME_BROWSER_PROXY_RESPONSE_MB", 32)) << 20,
		TunnelIdle:     envDur("RUNTIME_BROWSER_PROXY_TUNNEL_IDLE", 2*time.Minute),
		TunnelLifetime: envDur("RUNTIME_BROWSER_PROXY_TUNNEL_LIFETIME", 30*time.Minute),
	})
	proxyServer := &http.Server{
		Handler:           proxy,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    32 << 10,
	}
	go func() {
		if err := proxyServer.Serve(ln); err != nil && err != http.ErrServerClosed {
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
			Network:   browserNetwork,
			ProxyHost: os.Getenv("RUNTIME_BROWSER_PROXY_HOST"),
		})
		if err != nil {
			slog.Error("browserd: docker backend init failed", "err", err)
			os.Exit(1)
		}
	}

	m := browser.NewManager(be, browser.Config{
		MaxPerTenant:  envInt("RUNTIME_BROWSER_MAX_PER_TENANT", 5),
		IdleTTL:       envDur("RUNTIME_BROWSER_IDLE_TTL", 10*time.Minute),
		MaxLifetime:   envDur("RUNTIME_BROWSER_MAX_LIFETIME", time.Hour),
		ProxyAddr:     actualProxyAddr,
		ProxyToken:    proxyToken,
		SessionScoped: os.Getenv("RUNTIME_BROWSER_SCOPE") == "session",
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
