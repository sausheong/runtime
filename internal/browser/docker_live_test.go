//go:build live

package browser

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestLiveBrowseAndEgress is the real-Chrome proof: a container browses an
// allowed local server, extraction returns its text, and a denied host is
// blocked by the egress proxy. Requires Docker + the runtime-browser image
// (make browser-image) and runs only under -tags live.
//
// Requires Docker with a host-gateway route (Docker Desktop, or Linux with
// ExtraHosts host-gateway). The host-side httptest server and proxy are reached
// from the container via host.docker.internal.
func TestLiveBrowseAndEgress(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "<html><body><main><h1>Live OK</h1></main></body></html>")
	}))
	defer site.Close()

	host := strings.TrimPrefix(site.URL, "http://")
	hostOnly := host[:strings.IndexByte(host, ':')]
	pol, err := NewPolicy(ModeAllowList, []string{hostOnly, "host.docker.internal"})
	if err != nil {
		t.Fatal(err)
	}
	pol.lookup = func(string) ([]net.IP, error) { return []net.IP{net.ParseIP("8.8.8.8")}, nil }
	proxy := NewProxy(pol)
	ps := httptest.NewServer(proxy)
	defer ps.Close()
	proxyAddr := strings.TrimPrefix(ps.URL, "http://")

	be, err := NewDockerBackend(DockerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(be, Config{MaxPerTenant: 2, ProxyAddr: proxyAddr})
	ctx := context.Background()
	t.Cleanup(func() { _ = m.ReapStartup(ctx) })

	s, err := m.Create(ctx, "acme")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer m.Close(ctx, "acme", s.ID)

	// The httptest server binds 127.0.0.1 on the host; the container reaches it
	// via host.docker.internal (allow-listed above).
	siteURL := site.URL // http://127.0.0.1:PORT
	sitePort := siteURL[strings.LastIndexByte(siteURL, ':')+1:]
	containerSiteURL := "http://host.docker.internal:" + sitePort
	if _, err := Navigate(ctx, s, containerSiteURL, "h1", 0); err != nil {
		t.Fatalf("navigate allowed: %v", err)
	}
	txt, err := GetHTML(ctx, s, "body")
	if err != nil {
		t.Fatalf("get_text: %v", err)
	}
	if !strings.Contains(ExtractText(txt), "Live OK") {
		t.Fatalf("extract missing content: %q", txt)
	}
	if _, err := Navigate(ctx, s, "https://example.com", "", 0); err == nil {
		t.Fatal("navigate to non-allowlisted host should fail (egress blocked)")
	}
	shot, err := Screenshot(ctx, s)
	if err != nil || len(shot) == 0 {
		t.Fatalf("screenshot: err=%v len=%d", err, len(shot))
	}
}
