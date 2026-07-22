//go:build live

package browser

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/docker/docker/client"
)

// requireLiveDocker skips unless a daemon is reachable and the bundled browser
// image exists (run `make browser-image` to build it). Mirrors the sandbox
// package's guard so live tests skip cleanly when Docker is absent.
func requireLiveDocker(t *testing.T, ctx context.Context) {
	t.Helper()
	probe, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Skipf("docker client init failed: %v", err)
	}
	if _, err := probe.Ping(ctx); err != nil {
		t.Skipf("docker daemon unreachable: %v", err)
	}
	if _, _, err := probe.ImageInspectWithRaw(ctx, "runtime-browser:latest"); err != nil {
		t.Skipf("image runtime-browser:latest missing (run `make browser-image`): %v", err)
	}
}

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

	s, err := m.Create(ctx, "acme", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer m.Close(ctx, "acme", "", s.ID)

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

// TestLiveSessionScopedIsolation proves, against a REAL Docker daemon, that a
// SessionScoped browser Manager hides one session's browser from another
// session of the same tenant (Lookup), and that CloseSession removes the real
// container (ListLeftovers). Mirrors the sandbox live isolation test; browser
// has no exec, so isolation is asserted via Lookup and container removal.
func TestLiveSessionScopedIsolation(t *testing.T) {
	ctx := context.Background()
	requireLiveDocker(t, ctx)

	pol, err := NewPolicy(ModeAllowList, []string{"example.com"})
	if err != nil {
		t.Fatal(err)
	}
	ps := httptest.NewServer(NewProxy(pol))
	defer ps.Close()
	proxyAddr := strings.TrimPrefix(ps.URL, "http://")

	be, err := NewDockerBackend(DockerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(be, Config{MaxPerTenant: 2, ProxyAddr: proxyAddr, SessionScoped: true})
	// Reap any real containers this test leaves behind.
	t.Cleanup(func() { _ = m.ReapStartup(context.Background()) })

	s, err := m.Create(ctx, "acme", "sessA")
	if err != nil {
		t.Fatalf("Create sessA: %v", err)
	}

	// Same-session lookup works.
	if _, err := m.Lookup("acme", "sessA", s.ID); err != nil {
		t.Fatalf("same-session Lookup: %v", err)
	}
	// Cross-session: a foreign session sees the id as nonexistent (hidden).
	if _, err := m.Lookup("acme", "sessB", s.ID); err == nil {
		t.Fatal("cross-session Lookup should fail (sessA's browser hidden from sessB)")
	}

	// The real container exists before teardown.
	before, err := be.ListLeftovers(ctx)
	if err != nil {
		t.Fatalf("ListLeftovers before: %v", err)
	}
	if !containsStr(before, s.ContainerID) {
		t.Fatalf("container %s not present before CloseSession: %v", s.ContainerID, before)
	}

	// Session teardown removes the real container.
	if err := m.CloseSession(ctx, "acme", "sessA"); err != nil {
		t.Fatalf("CloseSession sessA: %v", err)
	}

	// The browser is gone: lookup now fails for its own session too.
	if _, err := m.Lookup("acme", "sessA", s.ID); err == nil {
		t.Fatal("Lookup should fail after CloseSession (browser gone)")
	}
	// And the real container no longer appears among live containers.
	after, err := be.ListLeftovers(ctx)
	if err != nil {
		t.Fatalf("ListLeftovers after: %v", err)
	}
	if containsStr(after, s.ContainerID) {
		t.Fatalf("container %s still present after CloseSession: %v", s.ContainerID, after)
	}
}

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
