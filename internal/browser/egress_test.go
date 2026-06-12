package browser

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
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
				p.lookup = func(string) ([]net.IP, error) {
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
	p.lookup = func(host string) ([]net.IP, error) {
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

func TestProxyForwardAllowDeny(t *testing.T) {
	// An upstream the proxy will forward to.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "hello from upstream")
	}))
	defer upstream.Close()

	// Address the upstream by name ("localhost") rather than the 127.0.0.1
	// literal so the proxy's Policy reaches the resolver hook (an IP literal
	// is checked directly and would trip the unconditional internal block).
	// localhost resolves to 127.0.0.1 for the real forward, while the hook
	// reports a public IP so the internal-block passes on the allowed path.
	upURL, _ := url.Parse(upstream.URL)
	allowedURL := "http://localhost:" + upURL.Port() + "/"

	// Allow-list contains the upstream's host (localhost); neutralize the
	// internal-block for the loopback test host via the resolver hook so the
	// allowed path isn't denied for being private.
	p, err := NewPolicy(ModeAllowList, []string{"localhost"})
	if err != nil {
		t.Fatal(err)
	}
	p.lookup = func(host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("8.8.8.8")}, nil // pretend public
	}
	proxy := NewProxy(p)
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
