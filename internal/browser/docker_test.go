package browser

import "testing"

// TestContainerProxyAddr pins the host-rewrite that lets the in-container
// Chrome reach the browserd-run egress proxy. The "::" case is a regression
// from the live proof: a dual-stack 0.0.0.0:0 listener reports its address as
// "[::]:port", which must rewrite to host.docker.internal — otherwise Chrome
// is handed --proxy-server=http://[::]:port and fails with
// ERR_PROXY_CONNECTION_FAILED.
func TestContainerProxyAddr(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"127.0.0.1:3128", "host.docker.internal:3128"},
		{"localhost:8080", "host.docker.internal:8080"},
		{"0.0.0.0:54673", "host.docker.internal:54673"},
		{"[::]:54673", "host.docker.internal:54673"},   // dual-stack wildcard (live-proof regression)
		{"[::1]:9222", "host.docker.internal:9222"},     // IPv6 loopback
		{"172.20.0.1:3128", "172.20.0.1:3128"},          // explicit routable IP — passthrough
		{"proxy.internal:3128", "proxy.internal:3128"},  // explicit host — passthrough
		{"not-host-port", "not-host-port"},              // unparseable — passthrough
	}
	for _, c := range cases {
		if got := containerProxyAddr(c.in); got != c.want {
			t.Errorf("containerProxyAddr(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
