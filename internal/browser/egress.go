// Package browser implements the headless-browser sandbox sessions served by
// cmd/browserd as MCP tools behind the platform gateway. Chrome runs in a
// locked-down container with no direct network; all traffic is forced through
// the egress proxy in this package, which allows or denies by hostname.
package browser

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

// Egress modes.
const (
	ModeDenyAll        = "deny-all"
	ModeAllowList      = "allow-list"
	ModeAllowAllPublic = "allow-all-public"
)

// Policy decides whether the browser may reach a given host. It is the sole
// egress control: the container has no direct route out, so every connection
// Chrome opens (top-level, subresource, fetch, redirect, websocket) is decided
// here. Construction validates the mode; an unknown mode is an error, never a
// silent allow.
type Policy struct {
	mode   string
	allow  []string // hostname globs, lowercased (allow-list mode)
	lookup func(host string) ([]net.IP, error)
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
	return &Policy{mode: mode, allow: low, lookup: net.LookupIP}, nil
}

// Decide returns nil if the host is allowed, or an error (the deny reason) if
// not. The internal-address block is UNCONDITIONAL across every mode and is
// checked against the RESOLVED IPs (DNS-rebind defense), so an allowlisted name
// pointing at a private address is still denied.
func (p *Policy) Decide(host string) error {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return fmt.Errorf("egress denied: empty host")
	}
	// Strip a port if present (CONNECT targets carry host:port).
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}

	// Mode gate first (cheap, no DNS for deny-all / allow-list misses).
	switch p.mode {
	case ModeDenyAll:
		return fmt.Errorf("egress denied: deny-all policy blocks %q", host)
	case ModeAllowList:
		if !p.matchAllow(host) {
			return fmt.Errorf("egress denied: %q not in allow-list", host)
		}
	case ModeAllowAllPublic:
		// fall through to the unconditional internal check below.
	}

	// Unconditional internal-address block (DNS-rebind defense): resolve, then
	// reject any private/loopback/link-local result. Applies to allow-list AND
	// allow-all-public — no mode can reach an internal address.
	if err := p.blockInternal(host); err != nil {
		return err
	}
	return nil
}

// matchAllow reports whether host matches any configured glob. A glob's "*"
// spans one or more leading labels: "*.x.org" matches "a.x.org" and
// "a.b.x.org" but NOT "x.org" or "xx.org". An exact glob (no "*") matches the
// host verbatim. Matching is label-wise on the dotted suffix — never substring.
func (p *Policy) matchAllow(host string) bool {
	for _, g := range p.allow {
		if g == host {
			return true
		}
		if suffix, ok := strings.CutPrefix(g, "*."); ok {
			// host must END with ".suffix" (at least one extra label).
			if strings.HasSuffix(host, "."+suffix) {
				return true
			}
		}
	}
	return false
}

// blockInternal resolves host and denies if any resolved IP is private,
// loopback, or link-local. A resolution failure is itself a denial (cannot
// prove the target is safe). A host that is already an IP literal is checked
// directly.
func (p *Policy) blockInternal(host string) error {
	if ip := net.ParseIP(host); ip != nil {
		if isInternalIP(ip) {
			return fmt.Errorf("egress denied: internal address %s", ip)
		}
		return nil
	}
	name := strings.TrimSuffix(host, ".")
	if name == "metadata" || name == "metadata.google.internal" {
		return fmt.Errorf("egress denied: cloud metadata endpoint")
	}
	ips, err := p.lookup(host)
	if err != nil {
		return fmt.Errorf("egress denied: cannot resolve %q: %w", host, err)
	}
	for _, ip := range ips {
		if isInternalIP(ip) {
			return fmt.Errorf("egress denied: %q resolves to internal address %s", host, ip)
		}
	}
	return nil
}

// internalNets is the private/loopback/link-local set, mirroring
// harness/tools/web/ssrf.go.
var internalNets []*net.IPNet

func init() {
	for _, cidr := range []string{
		"127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
		"169.254.0.0/16", "::1/128", "fc00::/7", "fe80::/10",
	} {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			panic(fmt.Sprintf("browser: bad internal CIDR %q: %v", cidr, err))
		}
		internalNets = append(internalNets, n)
	}
}

func isInternalIP(ip net.IP) bool {
	for _, n := range internalNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// Proxy is the forced egress proxy. The Chrome container points HTTP_PROXY /
// HTTPS_PROXY / --proxy-server at it and has no other route out, so every
// request passes through ServeHTTP, where Policy adjudicates the host. Plain
// HTTP is forwarded; HTTPS arrives as CONNECT and is blind-tunneled (the body
// stays encrypted — host-level control only, by design).
type Proxy struct {
	policy *Policy
	client *http.Client
	// onDecision is called for every allow/deny (host, allowed). Wired to the
	// egress metric by cmd/browserd; nil in tests.
	onDecision func(host string, allowed bool)
}

// NewProxy builds a Proxy over policy.
func NewProxy(policy *Policy) *Proxy {
	return &Proxy{
		policy: policy,
		client: &http.Client{
			Timeout: 60 * time.Second,
			// Do not auto-follow redirects: each hop is a fresh request the
			// browser issues and the proxy re-adjudicates. Return the 3xx as-is.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// OnDecision sets a callback invoked for every egress decision (host, allowed).
func (p *Proxy) OnDecision(fn func(host string, allowed bool)) { p.onDecision = fn }

func (p *Proxy) decide(host string) bool {
	err := p.policy.Decide(host)
	allowed := err == nil
	if p.onDecision != nil {
		p.onDecision(host, allowed)
	}
	if allowed {
		slog.Debug("egress allow", "host", host)
	} else {
		slog.Info("egress deny", "host", host, "reason", err)
	}
	return allowed
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}
	p.handleForward(w, r)
}

// handleForward proxies a plain-HTTP request after an allow decision.
func (p *Proxy) handleForward(w http.ResponseWriter, r *http.Request) {
	if !p.decide(r.Host) {
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
	_, _ = io.Copy(w, resp.Body)
}

// handleConnect blind-tunnels HTTPS after an allow decision on the CONNECT
// target host.
func (p *Proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	if !p.decide(r.Host) {
		http.Error(w, "egress denied by policy", http.StatusForbidden)
		return
	}
	dst, err := net.DialTimeout("tcp", r.Host, 30*time.Second)
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
	go func() { _, _ = io.Copy(dst, src); _ = dst.Close() }()
	go func() { _, _ = io.Copy(src, dst); _ = src.Close() }()
}

// copyHeader copies HTTP headers, skipping the hop-by-hop Connection headers.
func copyHeader(dst, src http.Header) {
	for k, vs := range src {
		if k == "Proxy-Connection" || k == "Connection" {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}
