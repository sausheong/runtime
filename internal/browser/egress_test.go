package browser

import (
	"net"
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
}
