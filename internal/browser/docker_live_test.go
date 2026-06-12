//go:build live

package browser

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestLiveBrowseAndEgress is the real-Chrome proof: a container browses an
// allow-listed PUBLIC site through the egress proxy, a non-allowlisted host is
// blocked, and a screenshot is captured. Requires Docker + the runtime-browser
// image (make browser-image) + outbound network, and runs only under -tags live.
// The host-run egress proxy is reached from the container via host.docker.internal
// (Docker Desktop, or Linux with the ExtraHosts host-gateway mapping the backend adds).
func TestLiveBrowseAndEgress(t *testing.T) {
	pol, err := NewPolicy(ModeAllowList, []string{"example.com", "www.example.com"})
	if err != nil {
		t.Fatal(err)
	}
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

	// Allowed public site loads.
	title, err := Navigate(ctx, s, "https://example.com", "", 0)
	if err != nil {
		t.Fatalf("navigate allowed (example.com): %v", err)
	}
	if !strings.Contains(title, "Example") {
		t.Logf("unexpected title %q (continuing — egress is the assertion)", title)
	}
	html, err := GetHTML(ctx, s, "body")
	if err != nil {
		t.Fatalf("get_text: %v", err)
	}
	if !strings.Contains(ExtractText(html), "Example Domain") {
		t.Fatalf("extract missing expected content: %q", ExtractText(html))
	}

	// Non-allowlisted host is blocked by egress (navigation fails).
	if _, err := Navigate(ctx, s, "https://www.iana.org", "", 0); err == nil {
		t.Fatal("navigate to non-allowlisted host should fail (egress blocked)")
	}

	// Screenshot returns bytes.
	shot, err := Screenshot(ctx, s)
	if err != nil || len(shot) == 0 {
		t.Fatalf("screenshot: err=%v len=%d", err, len(shot))
	}
}
