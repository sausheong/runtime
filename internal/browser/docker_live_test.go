//go:build live

package browser

import (
	"bytes"
	"context"
	"encoding/base64"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
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
	if _, err := probe.Ping(ctx, client.PingOptions{}); err != nil {
		t.Skipf("docker daemon unreachable: %v", err)
	}
	if _, err := probe.ImageInspect(ctx, "runtime-browser:latest"); err != nil {
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
	const proxyToken = "0123456789abcdef0123456789abcdef"
	ctx := context.Background()
	requireLiveDocker(t, ctx)
	pol, err := NewPolicy(ModeAllowList, []string{"example.com", "www.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	proxy := NewProxyWithConfig(pol, ProxyConfig{AuthToken: proxyToken})
	ps := httptest.NewServer(proxy)
	defer ps.Close()
	proxyAddr := strings.TrimPrefix(ps.URL, "http://")

	be, err := NewDockerBackend(DockerConfig{
		NoSandboxForTests: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(be, Config{
		MaxPerTenant: 2,
		ProxyAddr:    proxyAddr,
		ProxyToken:   proxyToken,
	})
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

// TestLiveInternalNetworkEgressTopology exercises the DEPLOYED shape (the
// turnkey Compose profile) rather than the host-loopback shape: browser
// containers sit on an INTERNAL Docker network, and their only path off it is
// the egress proxy running on a dual-homed neighbour (runtimed in Compose).
//
// It asserts three things against a real daemon:
//
//  1. NewDockerBackend REJECTS a non-internal network, and rejects a network
//     that does not exist. This is the point of validateContainerNetwork: a
//     routable network hands the container a default route and the layer-3
//     boundary silently disappears.
//  2. Direct, non-proxy egress from inside a container on the internal network
//     FAILS at layer 3. Proven by exec'ing socat straight at a public address —
//     a path that ignores --proxy-server entirely, which is precisely the
//     "compromised or misconfigured browser process" the internal network
//     exists to contain. The same probe on a ROUTABLE network is run as a
//     control and MUST succeed, so a green result cannot be an artefact of the
//     probe or of the sandbox having no outbound connectivity at all.
//  3. The token-authenticated egress proxy is reachable over the internal
//     network and still enforces policy there: an allow-listed public site is
//     fetched successfully, an unauthenticated request is refused with 407, and
//     a non-allow-listed host is refused with 403 — all from a container whose
//     ONLY route is the internal network.
//
// Why claim 3 does not drive Chromium: on Docker Desktop the test host cannot
// dial a container IP on ANY user-defined network (verified for both internal
// and routable networks — it is macOS VM isolation, not a property of
// `internal: true`). Manager.Create must fetch /json/version from the host, so
// it cannot complete against a private network from a macOS host. In the real
// Compose deployment browserd runs INSIDE runtimed, which is itself attached to
// browser-control, so it dials CDP from on-network and this does not arise.
// The proxy leg — the part this task changed — is therefore exercised directly
// over the internal network with real HTTP, which is the assertion that
// actually covers the fix.
func TestLiveInternalNetworkEgressTopology(t *testing.T) {
	const proxyToken = "0123456789abcdef0123456789abcdef"
	ctx := context.Background()
	requireLiveDocker(t, ctx)

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}

	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	internalNet := "rt-live-internal-" + suffix
	routableNet := "rt-live-routable-" + suffix
	for _, n := range []struct {
		name     string
		internal bool
	}{{internalNet, true}, {routableNet, false}} {
		if _, err := cli.NetworkCreate(ctx, n.name, client.NetworkCreateOptions{Internal: n.internal}); err != nil {
			t.Fatalf("create network %s: %v", n.name, err)
		}
		name := n.name
		t.Cleanup(func() { _, _ = cli.NetworkRemove(context.Background(), name, client.NetworkRemoveOptions{}) })
	}

	// ---- Claim 1: construction fails closed on a network that cannot enforce
	// the boundary, before any container is ever placed on it.
	if _, err := NewDockerBackend(DockerConfig{Network: routableNet}); err == nil {
		t.Fatal("NewDockerBackend accepted a non-internal network: the layer-3 egress boundary is not enforced")
	} else if !strings.Contains(err.Error(), "not internal") {
		t.Fatalf("rejection should name the internal requirement, got: %v", err)
	}
	if _, err := NewDockerBackend(DockerConfig{Network: "rt-live-absent-" + suffix}); err == nil {
		t.Fatal("NewDockerBackend accepted a nonexistent network")
	}
	// The internal network is accepted.
	if _, err := NewDockerBackend(DockerConfig{Network: internalNet, NoSandboxForTests: true}); err != nil {
		t.Fatalf("NewDockerBackend rejected a valid internal network: %v", err)
	}

	// ---- Claim 2: no default route off the internal network, with a routable
	// control proving the probe itself works.
	// Dial a public resolver by IP so DNS is not in play. </dev/null is
	// load-bearing: without it socat's stdin never reaches EOF and the probe
	// never exits on a network where the connection actually succeeds.
	const probe = "socat -T4 - TCP:1.1.1.1:53 </dev/null"

	ctrlCode, _, ctrlErr := runProbeOnNetwork(t, cli, routableNet, probe)
	if ctrlCode != 0 {
		t.Skipf("control: direct dial from a ROUTABLE network also failed (exit %d: %s) — "+
			"this host has no outbound connectivity, so the internal-network denial "+
			"below would prove nothing", ctrlCode, strings.TrimSpace(ctrlErr))
	}

	code, _, stderr := runProbeOnNetwork(t, cli, internalNet, probe)
	if code == 0 {
		t.Fatal("direct non-proxy dial SUCCEEDED from a container on the internal " +
			"network: layer-3 egress is not contained")
	}
	if !strings.Contains(stderr, "unreachable") {
		t.Logf("direct dial denied with exit %d but not the expected routing error; stderr=%q", code, stderr)
	}
	t.Logf("layer-3 boundary holds: routable control reached the internet (exit 0), "+
		"internal-network container did not (exit %d: %s)", code, strings.TrimSpace(stderr))

	// ---- Claim 3: the real egress proxy, bound wildcard with a token exactly
	// as ValidateProxyListener demands of a private-network deployment, is
	// reachable over the internal network and still adjudicates there.
	if err := ValidateProxyListener("0.0.0.0:0", proxyToken); err != nil {
		t.Fatalf("wildcard bind with token should be accepted: %v", err)
	}
	if err := ValidateProxyListener("0.0.0.0:0", ""); err == nil {
		t.Fatal("wildcard bind WITHOUT a token must be rejected")
	}
	pol, err := NewPolicy(ModeAllowList, []string{"example.com", "www.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	proxySrv := &http.Server{
		Handler:           NewProxyWithConfig(pol, ProxyConfig{AuthToken: proxyToken}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = proxySrv.Serve(ln) }()
	t.Cleanup(func() { _ = proxySrv.Close() })
	_, proxyPort, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	// Dual-homed relay = runtimed's position in Compose (networks: [default,
	// browser-control]). Plain socat: it moves bytes and does not adjudicate
	// them, so every decision below is still the Go proxy's.
	relayIP := startRelay(t, cli, internalNet, proxyPort)

	auth := base64.StdEncoding.EncodeToString([]byte("runtime:" + proxyToken))
	for _, tc := range []struct {
		name, host, authHdr, wantStatus string
	}{
		{"allow-listed host with credentials", "example.com", auth, "200"},
		{"missing credentials", "example.com", "", "407"},
		{"non-allow-listed host with credentials", "www.iana.org", auth, "403"},
	} {
		got := proxyRequestFromInternalNet(t, cli, internalNet, relayIP, tc.host, tc.authHdr)
		if !strings.Contains(got, " "+tc.wantStatus+" ") {
			t.Errorf("%s: want HTTP %s over the internal network, got status line %q",
				tc.name, tc.wantStatus, got)
		}
	}

	t.Log("NOT COVERED: (1) this test drives the proxy leg with raw HTTP rather " +
		"than Chromium, because a macOS Docker Desktop host cannot dial container " +
		"IPs on a user-defined network and so cannot complete Manager.Create's CDP " +
		"handshake; TestLiveBrowseAndEgress covers the Chromium+CDP path on the " +
		"host-loopback topology. (2) The internal network is a shared segment: the " +
		"relay (runtimed in Compose) and any sibling container the operator attaches " +
		"to browser-control remain reachable from the browser at layer 3. That " +
		"residual surface is bounded by the proxy's policy and by deployment " +
		"hygiene, not by this network boundary.")
}

// startIdleContainer runs a browser-image container that just sleeps, attached
// to exactly one network, so liveExec can probe its routing.
func startIdleContainer(t *testing.T, cli *client.Client, networkName string) string {
	t.Helper()
	ctx := context.Background()
	created, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image:      "runtime-browser:latest",
			Entrypoint: []string{"sleep"},
			Cmd:        []string{"300"},
			Labels:     map[string]string{browserLabel + ".livetest": "1"},
		},
		HostConfig: &container.HostConfig{NetworkMode: container.NetworkMode(networkName)},
	})
	if err != nil {
		t.Fatalf("create probe container on %s: %v", networkName, err)
	}
	t.Cleanup(func() {
		_, _ = cli.ContainerRemove(context.Background(), created.ID, client.ContainerRemoveOptions{Force: true})
	})
	if _, err := cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		t.Fatalf("start probe container on %s: %v", networkName, err)
	}
	return created.ID
}

// startRelay starts a socat forwarder attached to BOTH the default bridge (so
// it can reach the host-run proxy via the host-gateway mapping) and the
// internal network, and returns its address on the internal network.
func startRelay(t *testing.T, cli *client.Client, internalNet, proxyPort string) string {
	t.Helper()
	ctx := context.Background()
	created, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image:      "runtime-browser:latest",
			Entrypoint: []string{"socat"},
			Cmd:        []string{"tcp-listen:3128,fork,reuseaddr", "tcp:host.docker.internal:" + proxyPort},
			Labels:     map[string]string{browserLabel + ".livetest": "1"},
		},
		HostConfig: &container.HostConfig{ExtraHosts: []string{"host.docker.internal:host-gateway"}},
	})
	if err != nil {
		t.Fatalf("create relay: %v", err)
	}
	t.Cleanup(func() {
		_, _ = cli.ContainerRemove(context.Background(), created.ID, client.ContainerRemoveOptions{Force: true})
	})
	if _, err := cli.NetworkConnect(ctx, internalNet, client.NetworkConnectOptions{Container: created.ID}); err != nil {
		t.Fatalf("connect relay to %s: %v", internalNet, err)
	}
	if _, err := cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		t.Fatalf("start relay: %v", err)
	}
	insp, err := cli.ContainerInspect(ctx, created.ID, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("inspect relay: %v", err)
	}
	endpoint, ok := insp.Container.NetworkSettings.Networks[internalNet]
	if !ok || endpoint == nil || !endpoint.IPAddress.IsValid() {
		t.Fatalf("relay has no address on %s", internalNet)
	}
	return endpoint.IPAddress.String()
}

// proxyRequestFromInternalNet issues one absolute-form HTTP request to the
// proxy from a throwaway container whose ONLY network is internalNet, and
// returns the response status line.
//
// The trailing `sleep 6` matters: socat half-closes the connection when its
// stdin hits EOF, and the proxy's outbound request is bound to the inbound
// request context — a half-close cancels the upstream fetch and turns a
// legitimate 200 into "502 upstream error: context canceled". Holding stdin
// open until the response arrives is a property of this shell probe, not of
// the proxy (a real client keeps the socket open).
func proxyRequestFromInternalNet(t *testing.T, cli *client.Client, internalNet, relayIP, host, authHdr string) string {
	t.Helper()
	req := "GET http://" + host + "/ HTTP/1.0\\r\\nHost: " + host + "\\r\\n"
	if authHdr != "" {
		req += "Proxy-Authorization: Basic " + authHdr + "\\r\\n"
	}
	req += "\\r\\n"
	script := "{ printf '" + req + "'; sleep 6; } | socat -T12 - TCP:" + relayIP + ":3128"

	id := startShellContainer(t, cli, internalNet, script)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	wait := cli.ContainerWait(ctx, id, client.ContainerWaitOptions{})
	select {
	case err := <-wait.Error:
		t.Fatalf("wait for proxy probe (%s): %v", host, err)
	case <-wait.Result:
	case <-ctx.Done():
		t.Fatalf("proxy probe (%s) did not exit: %v", host, ctx.Err())
	}
	logs, err := cli.ContainerLogs(ctx, id, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		t.Fatalf("proxy probe logs (%s): %v", host, err)
	}
	defer logs.Close()
	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, logs); err != nil {
		t.Fatalf("demux proxy probe logs (%s): %v", host, err)
	}
	out := stdout.String()
	line, _, _ := strings.Cut(out, "\r\n")
	if line == "" {
		t.Fatalf("no response from proxy over the internal network for %s (stderr=%q)", host, stderr.String())
	}
	return line
}

// startShellContainer runs `sh -c script` on exactly one network.
func startShellContainer(t *testing.T, cli *client.Client, networkName, script string) string {
	t.Helper()
	ctx := context.Background()
	created, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image:      "runtime-browser:latest",
			Entrypoint: []string{"sh"},
			Cmd:        []string{"-c", script},
			Labels:     map[string]string{browserLabel + ".livetest": "1"},
		},
		HostConfig: &container.HostConfig{NetworkMode: container.NetworkMode(networkName)},
	})
	if err != nil {
		t.Fatalf("create shell container: %v", err)
	}
	t.Cleanup(func() {
		_, _ = cli.ContainerRemove(context.Background(), created.ID, client.ContainerRemoveOptions{Force: true})
	})
	if _, err := cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		t.Fatalf("start shell container: %v", err)
	}
	return created.ID
}

// runProbeOnNetwork runs a shell probe in a throwaway container attached to
// exactly one network and returns its exit code, stdout and stderr.
//
// This uses run-to-completion plus container logs rather than docker exec: on
// Docker Desktop, ExecInspect on a short-lived exec intermittently blocks past
// any reasonable deadline, which would make a routing assertion look like a
// timeout. Waiting on the container's own exit is unambiguous.
func runProbeOnNetwork(t *testing.T, cli *client.Client, networkName, script string) (int, string, string) {
	t.Helper()
	id := startShellContainer(t, cli, networkName, script)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	wait := cli.ContainerWait(ctx, id, client.ContainerWaitOptions{})
	var code int
	select {
	case err := <-wait.Error:
		t.Fatalf("wait for probe on %s: %v", networkName, err)
	case res := <-wait.Result:
		code = int(res.StatusCode)
	case <-ctx.Done():
		t.Fatalf("probe on %s did not exit: %v", networkName, ctx.Err())
	}

	logs, err := cli.ContainerLogs(ctx, id, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		t.Fatalf("probe logs on %s: %v", networkName, err)
	}
	defer logs.Close()
	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, logs); err != nil {
		t.Fatalf("demux probe logs on %s: %v", networkName, err)
	}
	return code, stdout.String(), stderr.String()
}

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
