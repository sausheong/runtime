package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

const (
	browserLabel = "runtime.browser"
	browserUID   = 1000
	cdpPort      = "9222"
)

// DockerConfig is the container posture for real browser sandboxes.
type DockerConfig struct {
	Image     string
	MemMB     int64
	CPUs      float64
	ProfileMB int
	Runtime   string
	// Network attaches browser containers to a private engine network and
	// makes CDP reachable only inside that network. Empty retains the
	// loopback-published host mode for direct host installations.
	Network string
	// ProxyHost is browserd's hostname as seen from Network (for example the
	// Compose service name "runtimed").
	ProxyHost string
	// NoSandboxForTests disables Chromium's process sandbox only in live tests
	// on Docker engines that block user namespaces. Production callers must
	// leave it false and provide a sandbox-capable engine/runtime.
	NoSandboxForTests bool
}

type dockerBackend struct {
	cli *client.Client
	cfg DockerConfig
}

// networkInfo is the subset of a Docker network inspection the egress
// boundary depends on.
type networkInfo struct {
	name     string
	exists   bool
	internal bool
}

// validateContainerNetwork fails closed unless the configured browser network
// can enforce egress below the application layer. Chromium honours
// --proxy-server, but a compromised or misconfigured browser process need
// not; an internal Docker network has no default route, so non-proxy egress
// fails at layer 3 regardless.
func validateContainerNetwork(n networkInfo) error {
	if !n.exists {
		return fmt.Errorf("browser network %q does not exist", n.name)
	}
	if !n.internal {
		return fmt.Errorf("browser network %q is not internal: a routable network "+
			"lets a browser process bypass the egress proxy; declare it `internal: true`", n.name)
	}
	return nil
}

// inspectContainerNetwork reads the engine's view of name. Any inspect failure
// — a missing network, an unreachable daemon, a permission error — is returned
// as an error rather than folded into networkInfo.exists, so the caller fails
// closed on all of them and the operator sees the daemon's own diagnosis. The
// exists=false arm of validateContainerNetwork therefore guards the pure
// decision function, not this path.
func inspectContainerNetwork(ctx context.Context, cli *client.Client, name string) (networkInfo, error) {
	res, err := cli.NetworkInspect(ctx, name, client.NetworkInspectOptions{})
	if err != nil {
		return networkInfo{}, fmt.Errorf("inspect browser network %q: %w", name, err)
	}
	return networkInfo{name: name, exists: true, internal: res.Network.Internal}, nil
}

// NewDockerBackend connects to the engine (DOCKER_HOST or default socket).
// When cfg.Network is set, the network is inspected and REJECTED unless it is
// internal — the private-network mode's whole point is a layer-3 boundary, and
// a routable network silently removes it. cfg.Network == "" is the documented
// direct-host install (CDP published on loopback, no private network), which
// has no network to validate and is left unchanged.
func NewDockerBackend(cfg DockerConfig) (Backend, error) {
	if cfg.Image == "" {
		cfg.Image = "runtime-browser:latest"
	}
	if cfg.MemMB <= 0 {
		cfg.MemMB = 1024
	}
	if cfg.CPUs <= 0 {
		cfg.CPUs = 1.0
	}
	if cfg.ProfileMB <= 0 {
		cfg.ProfileMB = 256
	}
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	if cfg.Network != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		info, err := inspectContainerNetwork(ctx, cli, cfg.Network)
		if err != nil {
			return nil, err
		}
		if err := validateContainerNetwork(info); err != nil {
			return nil, err
		}
	}
	return &dockerBackend{cli: cli, cfg: cfg}, nil
}

// containerProxyAddr rewrites a proxy address the BROWSERD process listens on
// into one the CONTAINER can dial. A loopback/wildcard host (127.0.0.1,
// localhost, 0.0.0.0, ::1, ::, or empty) becomes host.docker.internal (mapped
// to the host gateway via ExtraHosts); any other host (an explicit routable IP
// set by the operator) is passed through unchanged. The IPv6 wildcard "::" is
// what a dual-stack 0.0.0.0:0 listener reports from Addr().String() (as
// "[::]:port"), so it MUST be covered — otherwise Chrome is handed
// --proxy-server=http://[::]:port and fails with ERR_PROXY_CONNECTION_FAILED.
func containerProxyAddr(addr string) string {
	return containerProxyAddrForHost(addr, "")
}

func containerProxyAddrForHost(addr, advertisedHost string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr // not host:port — pass through
	}
	switch host {
	case "127.0.0.1", "localhost", "0.0.0.0", "::1", "::", "":
		host = advertisedHost
		if host == "" {
			host = "host.docker.internal"
		}
	}
	return net.JoinHostPort(host, port)
}

// cdpDialHost is the host browserd dials to reach a started browser
// container's published CDP port. Default 127.0.0.1 (browserd and the engine
// share a host). When browserd runs INSIDE a container (e.g. the turnkey
// compose, where it is spawned by a containerized runtimed), the published
// port lives on the host, reachable via host.docker.internal — set
// RUNTIME_BROWSER_CDP_DIAL_HOST=host.docker.internal there.
func cdpDialHost() string {
	if h := os.Getenv("RUNTIME_BROWSER_CDP_DIAL_HOST"); h != "" {
		return h
	}
	return "127.0.0.1"
}

// cdpPublishHost is the loopback interface used only by direct-host installs
// that do not configure a private browser network. CDP is unauthenticated, so
// a non-loopback override is deliberately ignored.
func cdpPublishHost() string {
	if h := os.Getenv("RUNTIME_BROWSER_CDP_PUBLISH_HOST"); h != "" {
		if h == "localhost" {
			return "127.0.0.1"
		}
		if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
			return h
		}
	}
	return "127.0.0.1"
}

// Create starts one locked-down Chromium container: egress only via the proxy
// at proxyAddr, read-only rootfs, tmpfs profile, all caps dropped, non-root,
// bounded cpu/mem/pids. With cfg.Network set, CDP is reachable only within that
// private network. Host installations without a network publish to loopback.
func (d *dockerBackend) Create(ctx context.Context, tenant, proxyAddr string) (BrowserHandle, error) {
	pids := int64(512)
	port := network.MustParsePort(cdpPort + "/tcp")
	cp := containerProxyAddrForHost(proxyAddr, d.cfg.ProxyHost)
	proxyURL := "http://" + cp
	hostConfig := &container.HostConfig{
		ReadonlyRootfs: true,
		ExtraHosts:     []string{"host.docker.internal:host-gateway"},
		Tmpfs:          map[string]string{"/profile": fmt.Sprintf("size=%dm,mode=1777", d.cfg.ProfileMB), "/tmp": "size=64m,mode=1777", "/home/browser": "size=64m,mode=1777"},
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges"},
		Runtime:        d.cfg.Runtime,
		Resources: container.Resources{
			NanoCPUs:  int64(d.cfg.CPUs * 1e9),
			Memory:    d.cfg.MemMB << 20,
			PidsLimit: &pids,
		},
	}
	if d.cfg.Network != "" {
		hostConfig.NetworkMode = container.NetworkMode(d.cfg.Network)
	} else {
		hostConfig.PortBindings = network.PortMap{
			port: []network.PortBinding{{HostIP: netip.MustParseAddr(cdpPublishHost())}},
		}
	}
	env := []string{
		"RUNTIME_CHROME_PROXY=" + proxyURL,
		"HTTP_PROXY=" + proxyURL,
		"HTTPS_PROXY=" + proxyURL,
		"NO_PROXY=",
	}
	if d.cfg.NoSandboxForTests {
		env = append(env, "RUNTIME_CHROME_NO_SANDBOX=1")
	}
	created, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image:        d.cfg.Image,
			User:         strconv.Itoa(browserUID),
			Env:          env,
			Labels:       map[string]string{browserLabel: "1", browserLabel + ".tenant": tenant},
			ExposedPorts: network.PortSet{port: struct{}{}},
		},
		HostConfig: hostConfig,
	})
	if err != nil {
		return BrowserHandle{}, err
	}
	if _, err := d.cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		_, _ = d.cli.ContainerRemove(ctx, created.ID, client.ContainerRemoveOptions{Force: true})
		return BrowserHandle{}, err
	}
	endpoint, err := d.waitForCDP(ctx, created.ID)
	if err != nil {
		_, _ = d.cli.ContainerRemove(ctx, created.ID, client.ContainerRemoveOptions{Force: true})
		return BrowserHandle{}, fmt.Errorf("CDP never became ready: %w", err)
	}
	return BrowserHandle{ContainerID: created.ID, Endpoint: endpoint}, nil
}

// waitForCDP polls the published CDP port until Chrome answers, returning the
// webSocketDebuggerUrl chromedp connects to. Bounded wait.
func (d *dockerBackend) waitForCDP(ctx context.Context, containerID string) (string, error) {
	deadline := time.Now().Add(20 * time.Second)
	var lastErr error
	for {
		ep, err := cdpEndpointFromInspect(ctx, d.cli, containerID, d.cfg.Network)
		if err == nil && ep != "" {
			return ep, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return "", fmt.Errorf("timeout: %v", lastErr)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// cdpEndpointFromInspect inspects the container for the host port mapped to
// cdpPort, fetches webSocketDebuggerUrl from Chrome's /json/version, and
// rewrites its host to <cdpDialHost>:<hostport> (default 127.0.0.1;
// host.docker.internal when browserd is containerized) (Chrome reports
// 0.0.0.0/its own hostname there, which the host can't dial).
func cdpEndpointFromInspect(ctx context.Context, cli *client.Client, containerID, networkName string) (string, error) {
	insp, err := cli.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		return "", err
	}
	if insp.Container.NetworkSettings == nil {
		return "", fmt.Errorf("no network settings yet")
	}
	if insp.Container.State != nil && !insp.Container.State.Running {
		return "", fmt.Errorf("browser container stopped: status=%s exit=%d error=%s",
			insp.Container.State.Status, insp.Container.State.ExitCode, insp.Container.State.Error)
	}
	dialHost, dialPort := "", cdpPort
	if networkName != "" {
		endpoint, ok := insp.Container.NetworkSettings.Networks[networkName]
		if !ok || endpoint == nil || !endpoint.IPAddress.IsValid() {
			return "", fmt.Errorf("no address on private browser network yet")
		}
		dialHost = endpoint.IPAddress.String()
	} else {
		var bindings []network.PortBinding
		// Port contains an interned protocol handle in the current Moby API.
		// Compare its stable string form rather than constructing a second map
		// key, which does not round-trip reliably across API decoding.
		for port, candidates := range insp.Container.NetworkSettings.Ports {
			if port.String() == cdpPort+"/tcp" {
				bindings = candidates
				break
			}
		}
		if len(bindings) == 0 || bindings[0].HostPort == "" {
			return "", fmt.Errorf("no host port yet (reported ports: %v)", insp.Container.NetworkSettings.Ports)
		}
		dialHost, dialPort = cdpDialHost(), bindings[0].HostPort
	}

	reqCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet,
		"http://"+net.JoinHostPort(dialHost, dialPort)+"/json/version", nil)
	if err != nil {
		return "", err
	}
	hc := &http.Client{Timeout: 2 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	var ver struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.Unmarshal(body, &ver); err != nil {
		return "", fmt.Errorf("parse /json/version: %w", err)
	}
	if ver.WebSocketDebuggerURL == "" {
		return "", fmt.Errorf("no webSocketDebuggerUrl in /json/version")
	}
	u, err := url.Parse(ver.WebSocketDebuggerURL)
	if err != nil {
		return "", fmt.Errorf("parse ws url: %w", err)
	}
	u.Host = net.JoinHostPort(dialHost, dialPort)
	return u.String(), nil
}

// Remove force-removes the container.
func (d *dockerBackend) Remove(ctx context.Context, containerID string) error {
	_, err := d.cli.ContainerRemove(ctx, containerID, client.ContainerRemoveOptions{Force: true})
	return err
}

// ListLeftovers returns every container carrying the browser label.
func (d *dockerBackend) ListLeftovers(ctx context.Context) ([]string, error) {
	list, err := d.cli.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: make(client.Filters).Add("label", browserLabel+"=1"),
	})
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(list.Items))
	for _, c := range list.Items {
		ids = append(ids, c.ID)
	}
	return ids, nil
}
