package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
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
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr // not host:port — pass through
	}
	switch host {
	case "127.0.0.1", "localhost", "0.0.0.0", "::1", "::", "":
		host = "host.docker.internal"
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

// cdpPublishHost is the host interface the browser container's CDP port is
// published on. Default 127.0.0.1 (loopback-only — safest; correct when
// browserd and the engine share a host). When browserd runs INSIDE a container
// and dials the published port via host.docker.internal (the bridge gateway),
// the port must be published on a bridge-reachable interface — set
// RUNTIME_BROWSER_CDP_PUBLISH_HOST=0.0.0.0 there. SECURITY: 0.0.0.0 exposes the
// unauthenticated CDP port on the host's published (ephemeral) port; acceptable
// only on a single-node trusted self-host (same trust posture as mounting the
// docker socket). The browser's network egress is still proxy-gated (deny-all
// by default), so this does not widen the browser's own reach.
func cdpPublishHost() string {
	if h := os.Getenv("RUNTIME_BROWSER_CDP_PUBLISH_HOST"); h != "" {
		return h
	}
	return "127.0.0.1"
}

// Create starts one locked-down Chromium container: egress only via the proxy
// at proxyAddr, read-only rootfs, tmpfs profile, all caps dropped, non-root,
// bounded cpu/mem/pids. Chrome listens for CDP on cdpPort, published to the host.
func (d *dockerBackend) Create(ctx context.Context, tenant, proxyAddr string) (BrowserHandle, error) {
	pids := int64(512)
	port := nat.Port(cdpPort + "/tcp")
	cp := containerProxyAddr(proxyAddr)
	created, err := d.cli.ContainerCreate(ctx,
		&container.Config{
			Image: d.cfg.Image,
			User:  strconv.Itoa(browserUID),
			Env: []string{
				"RUNTIME_CHROME_PROXY=http://" + cp,
				"HTTP_PROXY=http://" + cp,
				"HTTPS_PROXY=http://" + cp,
				"NO_PROXY=",
			},
			Labels:       map[string]string{browserLabel: "1", browserLabel + ".tenant": tenant},
			ExposedPorts: nat.PortSet{port: struct{}{}},
		},
		&container.HostConfig{
			ReadonlyRootfs: true,
			ExtraHosts:     []string{"host.docker.internal:host-gateway"},
			Tmpfs:          map[string]string{"/profile": fmt.Sprintf("size=%dm,mode=1777", d.cfg.ProfileMB), "/tmp": "size=64m,mode=1777", "/home/browser": "size=64m,mode=1777"},
			CapDrop:        []string{"ALL"},
			SecurityOpt:    []string{"no-new-privileges"},
			Runtime:        d.cfg.Runtime,
			PortBindings:   nat.PortMap{port: []nat.PortBinding{{HostIP: cdpPublishHost()}}},
			Resources: container.Resources{
				NanoCPUs:  int64(d.cfg.CPUs * 1e9),
				Memory:    d.cfg.MemMB << 20,
				PidsLimit: &pids,
			},
		},
		nil, nil, "")
	if err != nil {
		return BrowserHandle{}, err
	}
	if err := d.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		_ = d.cli.ContainerRemove(ctx, created.ID, container.RemoveOptions{Force: true})
		return BrowserHandle{}, err
	}
	endpoint, err := d.waitForCDP(ctx, created.ID)
	if err != nil {
		_ = d.cli.ContainerRemove(ctx, created.ID, container.RemoveOptions{Force: true})
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
		ep, err := cdpEndpointFromInspect(ctx, d.cli, containerID)
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
func cdpEndpointFromInspect(ctx context.Context, cli *client.Client, containerID string) (string, error) {
	insp, err := cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return "", err
	}
	if insp.NetworkSettings == nil {
		return "", fmt.Errorf("no network settings yet")
	}
	bindings := insp.NetworkSettings.Ports[nat.Port(cdpPort+"/tcp")]
	if len(bindings) == 0 || bindings[0].HostPort == "" {
		return "", fmt.Errorf("no host port yet")
	}
	hostPort := bindings[0].HostPort

	reqCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet,
		"http://"+cdpDialHost()+":"+hostPort+"/json/version", nil)
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
	u.Host = cdpDialHost() + ":" + hostPort
	return u.String(), nil
}

// Remove force-removes the container.
func (d *dockerBackend) Remove(ctx context.Context, containerID string) error {
	return d.cli.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
}

// ListLeftovers returns every container carrying the browser label.
func (d *dockerBackend) ListLeftovers(ctx context.Context) ([]string, error) {
	list, err := d.cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", browserLabel+"=1")),
	})
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(list))
	for _, c := range list {
		ids = append(ids, c.ID)
	}
	return ids, nil
}
