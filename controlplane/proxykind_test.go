package controlplane

import (
	"testing"

	"github.com/sausheong/runtime/internal/obs"
)

// TestProxyKind covers the classification of (already prefix-stripped) agent
// request method+path into the obs.Proxy* kind labels used by ProxyCall.
func TestProxyKind(t *testing.T) {
	cases := []struct {
		method, path, want string
	}{
		{"POST", "/sessions", obs.ProxyNewSession},
		{"POST", "/sessions/ses-abc/messages", obs.ProxyMessage},
		{"GET", "/sessions/ses-abc/stream", obs.ProxyStream}, // r.URL.Path excludes the ?since query
		{"GET", "/sessions/ses-abc", obs.ProxyOther},         // session get
		{"GET", "/sessions", obs.ProxyOther},                 // list (GET, not POST)
		{"GET", "/healthz", obs.ProxyOther},
		{"GET", "/meta", obs.ProxyOther},
		{"GET", "/", obs.ProxyOther},
	}
	for _, c := range cases {
		if got := proxyKind(c.method, c.path); got != c.want {
			t.Errorf("proxyKind(%q,%q) = %q, want %q", c.method, c.path, got, c.want)
		}
	}
}
