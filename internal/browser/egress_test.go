package browser

import (
	"bufio"
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestPolicyDecide(t *testing.T) {
	cases := []struct {
		name    string
		mode    string
		allow   []string
		host    string
		wantErr bool // true = denied
	}{
		{"deny-all denies public", "deny-all", nil, "example.com", true},
		{"deny-all denies all", "deny-all", []string{"*.x.org"}, "a.x.org", true},
		{"allow-list match label", "allow-list", []string{"*.wikipedia.org"}, "en.wikipedia.org", false},
		{"allow-list miss", "allow-list", []string{"*.wikipedia.org"}, "example.com", true},
		{"allow-list exact", "allow-list", []string{"api.github.com"}, "api.github.com", false},
		{"allow-list strips port", "allow-list", []string{"api.github.com"}, "api.github.com:443", false},
		{"bare star matches nothing", "allow-list", []string{"*"}, "example.com", true},
		{"allow-list no substring leak", "allow-list", []string{"*.x.org"}, "xx.org", true},
		{"allow-list case-insensitive", "allow-list", []string{"*.X.ORG"}, "a.x.org", false},
		{"allow-all-public allows", "allow-all-public", nil, "example.com", false},
		{"unknown mode fails closed", "bogus", nil, "example.com", true},
		{"malformed host fails closed", "allow-all-public", nil, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := NewPolicy(c.mode, c.allow)
			if c.mode == "bogus" {
				if err == nil {
					t.Fatal("unknown mode should error at construction")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			// Stub the resolver for cases that reach the internal-block lookup,
			// so the table stays hermetic. The empty-host case never resolves.
			if c.mode == "allow-list" || c.mode == "allow-all-public" {
				p.lookup = func(context.Context, string) ([]net.IP, error) {
					return []net.IP{net.ParseIP("8.8.8.8")}, nil
				}
			}
			err = p.Decide(c.host)
			if (err != nil) != c.wantErr {
				t.Fatalf("Decide(%q) err=%v, wantErr=%v", c.host, err, c.wantErr)
			}
		})
	}
}

func TestPolicyDNSRebindDefense(t *testing.T) {
	p, err := NewPolicy(ModeAllowList, []string{"*.evil.test"})
	if err != nil {
		t.Fatal(err)
	}
	p.lookup = func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("10.1.2.3")}, nil
	}
	if err := p.Decide("inner.evil.test"); err == nil {
		t.Fatal("allowlisted host resolving to a private IP must be denied")
	}
	pub, _ := NewPolicy(ModeAllowAllPublic, nil)
	if err := pub.Decide("192.168.0.5"); err == nil {
		t.Fatal("literal private IP must be denied in allow-all-public")
	}
	// IPv4-mapped IPv6 literal must not bypass the internal block.
	if err := pub.Decide("::ffff:10.0.0.1"); err == nil {
		t.Fatal("IPv4-mapped IPv6 of a private addr must be denied")
	}
	// A literal private IP carrying a port (CONNECT-style) must be denied.
	if err := pub.Decide("10.0.0.1:443"); err == nil {
		t.Fatal("private IP with port must be denied")
	}
}

func TestPolicyRejectsEveryNonPublicAddressClassAndMixedAnswers(t *testing.T) {
	p, err := NewPolicy(ModeAllowAllPublic, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		"0.1.2.3", "100.64.0.1", "192.0.2.1", "198.18.0.1",
		"198.51.100.1", "203.0.113.1", "224.0.0.1", "240.0.0.1",
		"2001:db8::1", "ff02::1",
	} {
		if err := p.Decide(raw); err == nil {
			t.Errorf("reserved address %s was allowed", raw)
		}
	}
	p.lookup = func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("8.8.8.8"), net.ParseIP("10.0.0.1")}, nil
	}
	if err := p.Decide("mixed.example"); err == nil {
		t.Fatal("mixed public/private DNS answer set was allowed")
	}
}

func TestDialUsesOnlyTheValidatedResolution(t *testing.T) {
	p, _ := NewPolicy(ModeAllowAllPublic, nil)
	lookups := 0
	p.lookup = func(context.Context, string) ([]net.IP, error) {
		lookups++
		if lookups > 1 {
			return []net.IP{net.ParseIP("127.0.0.1")}, nil
		}
		return []net.IP{net.ParseIP("8.8.8.8")}, nil
	}
	proxy := NewProxy(p)
	var dialed string
	proxy.dialRaw = func(_ context.Context, _, address string) (net.Conn, error) {
		dialed = address
		left, right := net.Pipe()
		_ = right.Close()
		return left, nil
	}
	conn, err := proxy.dialAllowed(context.Background(), "tcp", "rebind.example:443")
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if lookups != 1 {
		t.Fatalf("resolver called %d times, want exactly once", lookups)
	}
	if dialed != "8.8.8.8:443" {
		t.Fatalf("dialed %q, want validated address", dialed)
	}
}

func TestValidateProxyListener(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:0", "[::1]:0", "localhost:8080"} {
		if err := ValidateProxyListener(addr, ""); err != nil {
			t.Fatalf("private listener %s rejected: %v", addr, err)
		}
	}
	if err := ValidateProxyListener("0.0.0.0:0", ""); err == nil {
		t.Fatal("unauthenticated wildcard listener accepted")
	}
	if err := ValidateProxyListener("10.0.0.2:8080", "short"); err == nil {
		t.Fatal("weak token accepted for non-loopback listener")
	}
	if err := ValidateProxyListener("0.0.0.0:0", "0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatalf("authenticated wildcard listener rejected: %v", err)
	}
}

func TestProxyAuthentication(t *testing.T) {
	p, _ := NewPolicy(ModeDenyAll, nil)
	proxy := NewProxyWithConfig(p, ProxyConfig{AuthToken: "0123456789abcdef0123456789abcdef"})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)
	if rec.Code != http.StatusProxyAuthRequired {
		t.Fatalf("missing auth status=%d, want 407", rec.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	req.Header.Set("Proxy-Authorization", "Basic cnVudGltZTp3cm9uZw==")
	rec = httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)
	if rec.Code != http.StatusProxyAuthRequired {
		t.Fatalf("invalid auth status=%d, want 407", rec.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	req.Header.Set("Proxy-Authorization", "Basic "+
		base64.StdEncoding.EncodeToString([]byte("runtime:0123456789abcdef0123456789abcdef")))
	rec = httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("valid auth status=%d, want policy decision 403", rec.Code)
	}
}

func TestProxyCapacityBoundsRequestsAndTunnels(t *testing.T) {
	p, _ := NewPolicy(ModeAllowAllPublic, nil)
	proxy := NewProxyWithConfig(p, ProxyConfig{MaxRequests: 1, MaxTunnels: 1})

	proxy.requests <- struct{}{}
	req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)
	<-proxy.requests
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("request saturation status=%d, want 503", rec.Code)
	}

	proxy.tunnels <- struct{}{}
	req = httptest.NewRequest(http.MethodConnect, "http://public.test:443", nil)
	req.Host = "public.test:443"
	rec = httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)
	<-proxy.tunnels
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("tunnel saturation status=%d, want 503", rec.Code)
	}
}

func TestCopyTunnelEnforcesIdleDeadline(t *testing.T) {
	src, srcPeer := net.Pipe()
	dst, dstPeer := net.Pipe()
	defer srcPeer.Close()
	defer dstPeer.Close()
	start := time.Now()
	err := copyTunnel(dst, src, 20*time.Millisecond)
	if err == nil {
		t.Fatal("idle tunnel returned no error")
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("idle tunnel remained open for %v", elapsed)
	}
	if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
		t.Fatalf("idle tunnel error=%v, want timeout", err)
	}
}

func TestProxyEnforcesAbsoluteTunnelLifetime(t *testing.T) {
	p, _ := NewPolicy(ModeAllowList, []string{"public.test"})
	p.lookup = func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("8.8.8.8")}, nil
	}
	proxy := NewProxyWithConfig(p, ProxyConfig{
		MaxRequests:    2,
		MaxTunnels:     1,
		TunnelIdle:     time.Second,
		TunnelLifetime: 30 * time.Millisecond,
	})
	var upstreamPeer net.Conn
	proxy.dialRaw = func(context.Context, string, string) (net.Conn, error) {
		dst, peer := net.Pipe()
		upstreamPeer = peer
		return dst, nil
	}
	server := httptest.NewServer(proxy)
	defer server.Close()
	defer func() {
		if upstreamPeer != nil {
			_ = upstreamPeer.Close()
		}
	}()

	proxyURL, _ := url.Parse(server.URL)
	client, err := net.Dial("tcp", proxyURL.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := io.WriteString(client,
		"CONNECT public.test:443 HTTP/1.1\r\nHost: public.test:443\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(client)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status=%d, want 200", response.StatusCode)
	}
	if err := client.SetReadDeadline(time.Now().Add(500 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	var one [1]byte
	_, err = reader.Read(one[:])
	if err == nil {
		t.Fatal("tunnel remained open beyond absolute lifetime")
	}
	if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		t.Fatal("tunnel lifetime did not close the connection before test deadline")
	}
}

func TestProxyForwardAllowDeny(t *testing.T) {
	// An upstream the proxy will forward to.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "hello from upstream")
	}))
	defer upstream.Close()

	// Address the upstream by a synthetic public name. The resolver and raw
	// dial seams keep this test hermetic while proving the validated address,
	// rather than a second DNS answer, is handed to the dialer.
	upURL, _ := url.Parse(upstream.URL)
	allowedURL := "http://public.test:" + upURL.Port() + "/"

	p, err := NewPolicy(ModeAllowList, []string{"public.test"})
	if err != nil {
		t.Fatal(err)
	}
	p.lookup = func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("8.8.8.8")}, nil // pretend public
	}
	proxy := NewProxy(p)
	dialer := &net.Dialer{}
	proxy.dialRaw = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, network, upURL.Host)
	}
	ps := httptest.NewServer(proxy)
	defer ps.Close()

	// Client whose transport routes through the proxy.
	proxyURL, _ := url.Parse(ps.URL)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}

	resp, err := client.Get(allowedURL)
	if err != nil {
		t.Fatalf("allowed GET through proxy: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "hello from upstream" {
		t.Fatalf("body = %q", body)
	}

	// A denied host: deny-all policy → 403 through the proxy.
	deny, _ := NewPolicy(ModeDenyAll, nil)
	dps := httptest.NewServer(NewProxy(deny))
	defer dps.Close()
	dpu, _ := url.Parse(dps.URL)
	dclient := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(dpu)}}
	dresp, err := dclient.Get(upstream.URL)
	if err != nil {
		t.Fatalf("denied GET (transport): %v", err)
	}
	defer dresp.Body.Close()
	if dresp.StatusCode != http.StatusForbidden {
		t.Fatalf("denied GET status = %d, want 403", dresp.StatusCode)
	}
}
