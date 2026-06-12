// Package browser implements the headless-browser sandbox sessions served by
// cmd/browserd as MCP tools behind the platform gateway. Chrome runs in a
// locked-down container with no direct network; all traffic is forced through
// the egress proxy in this package, which allows or denies by hostname.
package browser

import (
	"fmt"
	"net"
	"strings"
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
	if host == "metadata" || host == "metadata.google.internal" {
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
		if _, n, err := net.ParseCIDR(cidr); err == nil {
			internalNets = append(internalNets, n)
		}
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
