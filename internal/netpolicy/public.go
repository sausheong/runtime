// Package netpolicy contains the outbound-network policy used for URLs that a
// tenant can register at runtime. It rejects local/private/reserved targets at
// validation time where possible and again after DNS resolution at connect
// time, preventing ordinary SSRF and DNS-rebinding bypasses.
package netpolicy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var blockedPrefixes = mustPrefixes(
	"0.0.0.0/8",
	"10.0.0.0/8",
	"100.64.0.0/10",
	"127.0.0.0/8",
	"169.254.0.0/16",
	"172.16.0.0/12",
	"192.0.0.0/24",
	"192.0.2.0/24",
	"192.168.0.0/16",
	"198.18.0.0/15",
	"198.51.100.0/24",
	"203.0.113.0/24",
	"224.0.0.0/4",
	"240.0.0.0/4",
	"::/128",
	"::1/128",
	"64:ff9b:1::/48",
	"2001:db8::/32",
	"fc00::/7",
	"fe80::/10",
	"ff00::/8",
)

func mustPrefixes(values ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(values))
	for _, value := range values {
		_, prefix, err := net.ParseCIDR(value)
		if err != nil {
			panic(err)
		}
		out = append(out, prefix)
	}
	return out
}

// ValidatePublicHTTPURL performs the non-network portion of tenant URL
// validation. DNS is intentionally rechecked by PublicTransport immediately
// before each connection, which is the authoritative rebinding-safe decision.
func ValidatePublicHTTPURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("must be an absolute http(s) URL")
	}
	if u.User != nil {
		return fmt.Errorf("URL userinfo is not allowed")
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return fmt.Errorf("URL hostname is required")
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") ||
		host == "metadata.google.internal" || strings.HasSuffix(host, ".internal") {
		return fmt.Errorf("private or local URL targets are not allowed")
	}
	if ip := net.ParseIP(host); ip != nil && !IsPublicIP(ip) {
		return fmt.Errorf("private or reserved URL targets are not allowed")
	}
	return nil
}

// IsPublicIP reports whether ip is suitable for a tenant-controlled outbound
// connection. Documentation, benchmark, multicast and otherwise reserved
// ranges are denied along with the usual private/loopback/link-local ranges.
func IsPublicIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	for _, prefix := range blockedPrefixes {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

// PublicTransport returns an HTTP transport whose connect path resolves the
// requested hostname itself, rejects any unsafe answer, and dials one of the
// already-verified IPs. Environment HTTP proxies are disabled so they cannot
// move the SSRF decision to an unverified intermediary.
func PublicTransport() *http.Transport {
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = nil
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	base.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("outbound address: %w", err)
		}
		ips, err := resolvePublic(ctx, host)
		if err != nil {
			return nil, err
		}
		var errs []error
		for _, ip := range ips {
			conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if dialErr == nil {
				return conn, nil
			}
			errs = append(errs, dialErr)
		}
		return nil, errors.Join(errs...)
	}
	return base
}

func resolvePublic(ctx context.Context, host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		if !IsPublicIP(ip) {
			return nil, fmt.Errorf("outbound target resolved to a private or reserved address")
		}
		return []net.IP{ip}, nil
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve outbound target: %w", err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("outbound target has no addresses")
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, addr := range addrs {
		if !IsPublicIP(addr.IP) {
			// Reject the whole result set rather than selecting a public answer
			// from a mixed public/private response.
			return nil, fmt.Errorf("outbound target resolved to a private or reserved address")
		}
		ips = append(ips, addr.IP)
	}
	return ips, nil
}
