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
}

type dockerBackend struct {
	cli *client.Client
	cfg DockerConfig
}

// NewDockerBackend connects to the engine (DOCKER_HOST or default socket).
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
	created, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image: d.cfg.Image,
			User:  strconv.Itoa(browserUID),
			Env: []string{
				"RUNTIME_CHROME_PROXY=http://" + cp,
				"HTTP_PROXY=http://" + cp,
				"HTTPS_PROXY=http://" + cp,
				"NO_PROXY=",
			},
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
	dialHost, dialPort := "", cdpPort
	if networkName != "" {
		endpoint, ok := insp.Container.NetworkSettings.Networks[networkName]
		if !ok || endpoint == nil || !endpoint.IPAddress.IsValid() {
			return "", fmt.Errorf("no address on private browser network yet")
		}
		dialHost = endpoint.IPAddress.String()
	} else {
		bindings := insp.Container.NetworkSettings.Ports[network.MustParsePort(cdpPort+"/tcp")]
		if len(bindings) == 0 || bindings[0].HostPort == "" {
			return "", fmt.Errorf("no host port yet")
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
