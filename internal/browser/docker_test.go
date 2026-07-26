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
		{"[::1]:9222", "host.docker.internal:9222"},    // IPv6 loopback
		{"172.20.0.1:3128", "172.20.0.1:3128"},         // explicit routable IP — passthrough
		{"proxy.internal:3128", "proxy.internal:3128"}, // explicit host — passthrough
		{"not-host-port", "not-host-port"},             // unparseable — passthrough
	}
	if got := containerProxyAddrForHost("[::]:54673", "runtimed"); got != "runtimed:54673" {
		t.Errorf("private-network proxy address = %q, want runtimed:54673", got)
	}
	for _, c := range cases {
		if got := containerProxyAddr(c.in); got != c.want {
			t.Errorf("containerProxyAddr(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestValidateContainerNetwork rejects a network that cannot enforce the
// layer-3 egress boundary. A non-internal network leaves the container a
// default route, so a process that ignores --proxy-server reaches the
// internet directly.
func TestValidateContainerNetwork(t *testing.T) {
	if err := validateContainerNetwork(networkInfo{name: "b", exists: true, internal: true}); err != nil {
		t.Fatalf("internal network rejected: %v", err)
	}
	if err := validateContainerNetwork(networkInfo{name: "b", exists: true, internal: false}); err == nil {
		t.Fatal("non-internal network accepted: layer-3 egress boundary is not enforced")
	}
	if err := validateContainerNetwork(networkInfo{name: "b", exists: false}); err == nil {
		t.Fatal("missing network accepted")
	}
}

func TestCDPDialHost(t *testing.T) {
	t.Setenv("RUNTIME_BROWSER_CDP_DIAL_HOST", "")
	if got := cdpDialHost(); got != "127.0.0.1" {
		t.Errorf("default cdpDialHost = %q, want 127.0.0.1", got)
	}
	t.Setenv("RUNTIME_BROWSER_CDP_DIAL_HOST", "host.docker.internal")
	if got := cdpDialHost(); got != "host.docker.internal" {
		t.Errorf("override cdpDialHost = %q, want host.docker.internal", got)
	}
}

func TestCDPPublishHost(t *testing.T) {
	t.Setenv("RUNTIME_BROWSER_CDP_PUBLISH_HOST", "")
	if got := cdpPublishHost(); got != "127.0.0.1" {
		t.Errorf("default cdpPublishHost = %q, want 127.0.0.1", got)
	}
	t.Setenv("RUNTIME_BROWSER_CDP_PUBLISH_HOST", "0.0.0.0")
	if got := cdpPublishHost(); got != "127.0.0.1" {
		t.Errorf("unsafe override cdpPublishHost = %q, want loopback", got)
	}
}
