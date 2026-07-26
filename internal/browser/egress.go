// Package browser implements the headless-browser sandbox sessions served by
// cmd/browserd as MCP tools behind the platform gateway. Chrome runs in a
// locked-down container; its entire network stack is forced through the egress
// proxy in this package via --proxy-server, which allows or denies by hostname.
// The agent can only drive Chrome over CDP, so the proxy adjudicates all of the
// agent's reachable traffic. (The container itself sits on a docker bridge so it
// can dial the proxy; a network-level egress boundary that also contains a
// hypothetical non-proxy-respecting process is recorded as follow-on hardening.)
package browser

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sausheong/runtime/internal/netpolicy"
)

// Egress modes.
const (
	ModeDenyAll        = "deny-all"
	ModeAllowList      = "allow-list"
	ModeAllowAllPublic = "allow-all-public"
)

// ValidateProxyListener rejects an exposed unauthenticated proxy. Loopback
// listeners are private to browserd's host. Any other bind requires a strong
// operator-provided token because policy controls destinations, not callers.
func ValidateProxyListener(addr, token string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("browser proxy address: %w", err)
	}
	private := host == "localhost"
	if ip := net.ParseIP(host); ip != nil {
		private = ip.IsLoopback()
	}
	if !private && len(token) < 32 {
		return fmt.Errorf("non-loopback browser proxy requires RUNTIME_BROWSER_PROXY_TOKEN of at least 32 characters")
	}
	return nil
}

// Policy decides whether the browser may reach a given host. It is the egress
// control for all of Chrome's traffic: every connection Chrome opens (top-level,
// subresource, fetch, redirect, websocket) goes through --proxy-server and is
// decided here, and since the agent can only drive Chrome over CDP this covers
// the agent's full reachable surface. Construction validates the mode; an unknown
// mode is an error, never a silent allow.
type Policy struct {
	mode   string
	allow  []string // hostname globs, lowercased (allow-list mode)
	lookup func(ctx context.Context, host string) ([]net.IP, error)
}

// NewPolicy builds a Policy. allow globs are only meaningful for allow-list
// mode. An unrecognized mode is rejected (fail-closed at construction).
func NewPolicy(mode string, allow []string) (*Policy, error) {
	switch mode {
	case ModeDenyAll, ModeAllowList, ModeAllowAllPublic:
	default:
		return nil, fmt.Errorf("unknown egress mode %q (want %s|%s|%s)",
			mode, ModeDenyAll, ModeAllowList, ModeAllowAllPublic)
	}
	low := make([]string, len(allow))
	for i, g := range allow {
		low[i] = strings.ToLower(strings.TrimSpace(g))
	}
	return &Policy{
		mode:  mode,
		allow: low,
		lookup: func(ctx context.Context, host string) ([]net.IP, error) {
			addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			ips := make([]net.IP, 0, len(addrs))
			for _, addr := range addrs {
				ips = append(ips, addr.IP)
			}
			return ips, nil
		},
	}, nil
}

// Decide returns nil if the host is allowed, or an error (the deny reason) if
// not. The internal-address block is UNCONDITIONAL across every mode and is
// checked against the RESOLVED IPs (DNS-rebind defense), so an allowlisted name
// pointing at a private address is still denied.
func (p *Policy) Decide(host string) error {
	_, err := p.resolveAllowed(context.Background(), host)
	return err
}

// authorizeHost applies the hostname/mode portion of the policy. Address
// safety is intentionally decided later, immediately before connect.
func (p *Policy) authorizeHost(host string) (string, error) {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return "", fmt.Errorf("egress denied: empty host")
	}
	// Strip a port if present (CONNECT targets carry host:port).
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}

	// Mode gate first (cheap, no DNS for deny-all / allow-list misses).
	switch p.mode {
	case ModeDenyAll:
		return "", fmt.Errorf("egress denied: deny-all policy blocks %q", host)
	case ModeAllowList:
		if !p.matchAllow(host) {
			return "", fmt.Errorf("egress denied: %q not in allow-list", host)
		}
	case ModeAllowAllPublic:
		// Address safety is checked at dial time below.
	}
	return host, nil
}

// resolveAllowed authorizes host, resolves it once, rejects the entire answer
// set if any address is non-public, and returns the exact addresses the caller
// must dial. This prevents a second resolver lookup from changing the target.
func (p *Policy) resolveAllowed(ctx context.Context, host string) ([]net.IP, error) {
	host, err := p.authorizeHost(host)
	if err != nil {
		return nil, err
	}
	if ip := net.ParseIP(host); ip != nil {
		if !netpolicy.IsPublicIP(ip) {
			return nil, fmt.Errorf("egress denied: private or reserved address %s", ip)
		}
		return []net.IP{ip}, nil
	}
	name := strings.TrimSuffix(host, ".")
	if name == "localhost" || strings.HasSuffix(name, ".localhost") ||
		name == "metadata" || name == "metadata.google.internal" ||
		strings.HasSuffix(name, ".internal") {
		return nil, fmt.Errorf("egress denied: private or local hostname %q", host)
	}
	ips, err := p.lookup(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("egress denied: cannot resolve %q: %w", host, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("egress denied: %q has no addresses", host)
	}
	for _, ip := range ips {
		if !netpolicy.IsPublicIP(ip) {
			return nil, fmt.Errorf("egress denied: %q resolves to private or reserved address %s", host, ip)
		}
	}
	return ips, nil
}

// matchAllow reports whether host matches any configured glob. A glob's "*"
// spans one or more leading labels.
func (p *Policy) matchAllow(host string) bool {
	for _, g := range p.allow {
		if g == host {
			return true
		}
		if suffix, ok := strings.CutPrefix(g, "*."); ok && strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

// Proxy is the forced egress proxy. Chrome points HTTP_PROXY / HTTPS_PROXY /
// --proxy-server at it, so every request Chrome makes passes through ServeHTTP,
// where Policy adjudicates the host. Plain HTTP is forwarded; HTTPS arrives as
// CONNECT and is blind-tunneled (the body stays encrypted — host-level control
// only, by design).
type Proxy struct {
	policy *Policy
	client *http.Client
	cfg    ProxyConfig

	requests chan struct{}
	tunnels  chan struct{}
	dialRaw  func(ctx context.Context, network, address string) (net.Conn, error)
	// onDecision is called for every allow/deny (host, allowed). Reserved for a
	// future egress metric; unused in M2 (decisions are surfaced via slog).
	onDecision func(host string, allowed bool)
}

// ProxyConfig bounds proxy resources. Zero values receive secure defaults.
type ProxyConfig struct {
	AuthToken      string
	MaxRequests    int
	MaxTunnels     int
	ResponseBytes  int64
	DialTimeout    time.Duration
	TunnelIdle     time.Duration
	TunnelLifetime time.Duration
}

// NewProxy builds a Proxy over policy.
func NewProxy(policy *Policy) *Proxy {
	return NewProxyWithConfig(policy, ProxyConfig{})
}

// NewProxyWithConfig builds a bounded Proxy over policy.
func NewProxyWithConfig(policy *Policy, cfg ProxyConfig) *Proxy {
	if cfg.MaxRequests <= 0 {
		cfg.MaxRequests = 128
	}
	if cfg.MaxTunnels <= 0 {
		cfg.MaxTunnels = 64
	}
	if cfg.ResponseBytes <= 0 {
		cfg.ResponseBytes = 32 << 20
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 10 * time.Second
	}
	if cfg.TunnelIdle <= 0 {
		cfg.TunnelIdle = 2 * time.Minute
	}
	if cfg.TunnelLifetime <= 0 {
		cfg.TunnelLifetime = 30 * time.Minute
	}
	dialer := &net.Dialer{Timeout: cfg.DialTimeout, KeepAlive: 30 * time.Second}
	p := &Proxy{
		policy:   policy,
		cfg:      cfg,
		requests: make(chan struct{}, cfg.MaxRequests),
		tunnels:  make(chan struct{}, cfg.MaxTunnels),
		dialRaw:  dialer.DialContext,
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = p.dialAllowed
	p.client = &http.Client{
		Transport: transport,
		Timeout:   60 * time.Second,
		// Do not auto-follow redirects: each hop is a fresh request the
		// browser issues and the proxy re-adjudicates. Return the 3xx as-is.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return p
}

func (p *Proxy) dialAllowed(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("invalid outbound address: %w", err)
	}
	ips, err := p.policy.resolveAllowed(ctx, host)
	if err != nil {
		p.recordDecision(host, false, err)
		return nil, err
	}
	var lastErr error
	for _, ip := range ips {
		conn, dialErr := p.dialRaw(ctx, network, net.JoinHostPort(ip.String(), port))
		if dialErr == nil {
			p.recordDecision(host, true, nil)
			return conn, nil
		}
		lastErr = dialErr
	}
	return nil, fmt.Errorf("dial validated target %q: %w", host, lastErr)
}

// OnDecision sets a callback invoked for every egress decision (host, allowed).
// Reserved for a future egress metric; unused in M2 (decisions are surfaced via
// slog).
func (p *Proxy) OnDecision(fn func(host string, allowed bool)) { p.onDecision = fn }

func (p *Proxy) recordDecision(host string, allowed bool, err error) {
	if p.onDecision != nil {
		p.onDecision(host, allowed)
	}
	if allowed {
		slog.Debug("egress allow", "host", host)
	} else {
		slog.Info("egress deny", "host", host, "reason", err)
	}
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if p.cfg.AuthToken != "" {
		user, pass, ok := proxyBasicAuth(r.Header.Get("Proxy-Authorization"))
		if !ok || subtle.ConstantTimeCompare([]byte(user), []byte("runtime")) != 1 ||
			subtle.ConstantTimeCompare([]byte(pass), []byte(p.cfg.AuthToken)) != 1 {
			w.Header().Set("Proxy-Authenticate", `Basic realm="runtime-browser"`)
			http.Error(w, "proxy authentication required", http.StatusProxyAuthRequired)
			return
		}
	}
	select {
	case p.requests <- struct{}{}:
		defer func() { <-p.requests }()
	default:
		http.Error(w, "proxy at capacity", http.StatusServiceUnavailable)
		return
	}
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}
	p.handleForward(w, r)
}

func proxyBasicAuth(value string) (string, string, bool) {
	const prefix = "Basic "
	if len(value) < len(prefix) || !strings.EqualFold(value[:len(prefix)], prefix) {
		return "", "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value[len(prefix):]))
	if err != nil {
		return "", "", false
	}
	user, pass, ok := strings.Cut(string(decoded), ":")
	return user, pass, ok
}

// handleForward proxies a plain-HTTP request after an allow decision.
func (p *Proxy) handleForward(w http.ResponseWriter, r *http.Request) {
	if _, err := p.policy.authorizeHost(r.Host); err != nil {
		p.recordDecision(r.Host, false, err)
		http.Error(w, "egress denied by policy", http.StatusForbidden)
		return
	}
	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, r.URL.String(), r.Body)
	if err != nil {
		http.Error(w, "bad proxied request", http.StatusBadGateway)
		return
	}
	copyHeader(outReq.Header, r.Header)
	resp, err := p.client.Do(outReq)
	if err != nil {
		http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	n, err := io.Copy(w, io.LimitReader(resp.Body, p.cfg.ResponseBytes))
	if err != nil {
		slog.Warn("browser proxy response copy failed", "err", err)
	}
	_ = n
}

// handleConnect blind-tunnels HTTPS after an allow decision on the CONNECT
// target host.
func (p *Proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	if _, err := p.policy.authorizeHost(r.Host); err != nil {
		p.recordDecision(r.Host, false, err)
		http.Error(w, "egress denied by policy", http.StatusForbidden)
		return
	}
	select {
	case p.tunnels <- struct{}{}:
		defer func() { <-p.tunnels }()
	default:
		http.Error(w, "proxy tunnel capacity reached", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), p.cfg.DialTimeout)
	defer cancel()
	dst, err := p.dialAllowed(ctx, "tcp", r.Host)
	if err != nil {
		http.Error(w, "dial upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = dst.Close()
		http.Error(w, "proxy: hijack unsupported", http.StatusInternalServerError)
		return
	}
	src, _, err := hj.Hijack()
	if err != nil {
		_ = dst.Close()
		return
	}
	_, _ = src.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	done := make(chan struct{}, 2)
	var closeOnce sync.Once
	closeBoth := func() {
		closeOnce.Do(func() {
			_ = src.Close()
			_ = dst.Close()
		})
	}
	go func() { _ = copyTunnel(dst, src, p.cfg.TunnelIdle); done <- struct{}{} }()
	go func() { _ = copyTunnel(src, dst, p.cfg.TunnelIdle); done <- struct{}{} }()
	timer := time.NewTimer(p.cfg.TunnelLifetime)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	case <-r.Context().Done():
	}
	closeBoth()
	<-done
}

func copyTunnel(dst, src net.Conn, idle time.Duration) error {
	buf := make([]byte, 32<<10)
	for {
		if err := src.SetReadDeadline(time.Now().Add(idle)); err != nil {
			return err
		}
		n, readErr := src.Read(buf)
		if n > 0 {
			if err := dst.SetWriteDeadline(time.Now().Add(idle)); err != nil {
				return err
			}
			if _, err := dst.Write(buf[:n]); err != nil {
				return err
			}
		}
		if readErr != nil {
			return readErr
		}
	}
}

// hopHeaders are the hop-by-hop headers a proxy must not forward (RFC 7230 §6.1).
var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// copyHeader copies HTTP headers minus hop-by-hop headers: the standard set
// plus any header named in the Connection header (RFC 7230 §6.1). It mutates
// src (deleting hop-by-hop entries); callers pass a request/response header
// not reused afterward.
func copyHeader(dst, src http.Header) {
	for _, f := range src["Connection"] {
		for _, name := range strings.Split(f, ",") {
			if name = strings.TrimSpace(name); name != "" {
				src.Del(name)
			}
		}
	}
	for _, h := range hopHeaders {
		src.Del(h)
	}
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}
