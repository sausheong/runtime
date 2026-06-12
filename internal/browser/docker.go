package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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

// Create starts one locked-down Chromium container: egress only via the proxy
// at proxyAddr, read-only rootfs, tmpfs profile, all caps dropped, non-root,
// bounded cpu/mem/pids. Chrome listens for CDP on cdpPort, published to the host.
func (d *dockerBackend) Create(ctx context.Context, tenant, proxyAddr string) (BrowserHandle, error) {
	pids := int64(512)
	port := nat.Port(cdpPort + "/tcp")
	cmd := []string{
		"chromium", "--headless=new", "--no-sandbox", "--disable-gpu",
		"--remote-debugging-address=0.0.0.0",
		"--remote-debugging-port=" + cdpPort,
		"--proxy-server=http://" + proxyAddr,
		"--disable-blink-features=AutomationControlled",
		"--user-data-dir=/profile",
		"--no-first-run", "--no-default-browser-check",
		"about:blank",
	}
	created, err := d.cli.ContainerCreate(ctx,
		&container.Config{
			Image:        d.cfg.Image,
			Cmd:          cmd,
			User:         strconv.Itoa(browserUID),
			Env:          []string{"HTTP_PROXY=http://" + proxyAddr, "HTTPS_PROXY=http://" + proxyAddr, "NO_PROXY="},
			Labels:       map[string]string{browserLabel: "1", browserLabel + ".tenant": tenant},
			ExposedPorts: nat.PortSet{port: struct{}{}},
		},
		&container.HostConfig{
			ReadonlyRootfs:  true,
			Tmpfs:           map[string]string{"/profile": fmt.Sprintf("size=%dm,mode=1777", d.cfg.ProfileMB), "/tmp": "size=64m,mode=1777"},
			CapDrop:         []string{"ALL"},
			SecurityOpt:     []string{"no-new-privileges"},
			Runtime:         d.cfg.Runtime,
			PortBindings:    nat.PortMap{port: []nat.PortBinding{{HostIP: "127.0.0.1"}}},
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
// rewrites its host to 127.0.0.1:<hostport> (Chrome reports 0.0.0.0/its own
// hostname there, which the host can't dial).
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
		"http://127.0.0.1:"+hostPort+"/json/version", nil)
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
	u.Host = "127.0.0.1:" + hostPort
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
