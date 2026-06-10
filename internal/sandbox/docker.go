package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

// sandboxLabel marks every container sandboxd creates, for reap-on-start.
const sandboxLabel = "runtime.sandbox"

// sandboxUID is the uid of the non-root `sandbox` user baked into the
// bundled image (deploy/sandbox.Dockerfile).
const sandboxUID = 1000

// DockerConfig is the container posture for real sandboxes.
type DockerConfig struct {
	Image       string  // default runtime-sandbox:latest
	WorkspaceMB int     // tmpfs /workspace size (default 64)
	MemMB       int64   // memory limit (default 512)
	CPUs        float64 // cpu limit (default 1.0)
	Runtime     string  // optional engine runtime, e.g. "runsc" (gVisor)
}

// dockerBackend implements Backend over the Docker Engine API.
type dockerBackend struct {
	cli *client.Client
	cfg DockerConfig
}

// NewDockerBackend connects to the engine (DOCKER_HOST or default socket).
// The connection is lazy — a dead daemon surfaces on first use, which the
// Manager reports per-call (degrade-don't-fail).
func NewDockerBackend(cfg DockerConfig) (Backend, error) {
	if cfg.Image == "" {
		cfg.Image = "runtime-sandbox:latest"
	}
	if cfg.WorkspaceMB <= 0 {
		cfg.WorkspaceMB = 64
	}
	if cfg.MemMB <= 0 {
		cfg.MemMB = 512
	}
	if cfg.CPUs <= 0 {
		cfg.CPUs = 1.0
	}
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	return &dockerBackend{cli: cli, cfg: cfg}, nil
}

// Create starts one locked-down container: no network, read-only rootfs,
// tmpfs /workspace and /tmp, all capabilities dropped, non-root user,
// bounded cpu/memory/pids.
func (d *dockerBackend) Create(ctx context.Context, tenant string) (string, error) {
	pids := int64(128)
	created, err := d.cli.ContainerCreate(ctx,
		&container.Config{
			Image:      d.cfg.Image,
			Cmd:        []string{"sleep", "infinity"},
			User:       strconv.Itoa(sandboxUID),
			WorkingDir: workspace,
			Labels: map[string]string{
				sandboxLabel:             "1",
				sandboxLabel + ".tenant": tenant,
			},
		},
		&container.HostConfig{
			NetworkMode:    "none",
			ReadonlyRootfs: true,
			Tmpfs: map[string]string{
				workspace: fmt.Sprintf("size=%dm,mode=1777", d.cfg.WorkspaceMB),
				"/tmp":    "size=16m,mode=1777",
			},
			CapDrop:     []string{"ALL"},
			SecurityOpt: []string{"no-new-privileges"},
			Runtime:     d.cfg.Runtime,
			Resources: container.Resources{
				NanoCPUs:  int64(d.cfg.CPUs * 1e9),
				Memory:    d.cfg.MemMB << 20,
				PidsLimit: &pids,
			},
		},
		nil, nil, "")
	if err != nil {
		return "", err
	}
	if err := d.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		// Don't leave a created-but-never-started container behind.
		_ = d.cli.ContainerRemove(ctx, created.ID, container.RemoveOptions{Force: true})
		return "", err
	}
	return created.ID, nil
}

// Exec runs argv under coreutils `timeout` so the wall-clock limit kills the
// process tree, never the container. Exit 124 (TERM) / 137 (KILL after
// --kill-after) at or past the deadline reports TimedOut.
func (d *dockerBackend) Exec(ctx context.Context, containerID string, argv []string, timeout time.Duration) (ExecResult, error) {
	return d.runExec(ctx, containerID, argv, nil, timeout)
}

// execStdin is Exec with bytes fed to the process's stdin (used for file
// writes via `tee` — argv-only, content never touches a shell).
func (d *dockerBackend) execStdin(ctx context.Context, containerID string, argv []string, stdin []byte, timeout time.Duration) (ExecResult, error) {
	return d.runExec(ctx, containerID, argv, stdin, timeout)
}

// runExec is the shared exec plumbing behind Exec and execStdin: timeout-wrap
// argv with coreutils `timeout`, attach (optionally with stdin), copy output
// in a goroutine raced against ctx, inspect with a fresh context.
func (d *dockerBackend) runExec(ctx context.Context, containerID string, argv []string, stdin []byte, timeout time.Duration) (ExecResult, error) {
	cmd := append([]string{
		"timeout", "--kill-after=5", strconv.Itoa(int(timeout.Seconds())),
	}, argv...)

	// Headroom past the in-container timeout so `timeout` itself gets to
	// report 124/137 before we abandon the attach.
	ctx, cancel := context.WithTimeout(ctx, timeout+15*time.Second)
	defer cancel()

	start := time.Now()
	exec, err := d.cli.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		Cmd:          cmd,
		WorkingDir:   workspace,
		AttachStdin:  stdin != nil,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return ExecResult{}, err
	}
	attach, err := d.cli.ContainerExecAttach(ctx, exec.ID, container.ExecAttachOptions{})
	if err != nil {
		return ExecResult{}, err
	}
	defer attach.Close()

	if stdin != nil {
		// Feed stdin, then half-close so the process sees EOF. Write errors
		// surface as a non-zero exit / short file from the process side; the
		// inspect below reports them.
		if _, err := attach.Conn.Write(stdin); err != nil {
			return ExecResult{}, fmt.Errorf("write exec stdin: %w", err)
		}
		if err := attach.CloseWrite(); err != nil {
			return ExecResult{}, fmt.Errorf("close exec stdin: %w", err)
		}
	}

	// The hijacked connection's read is NOT ctx-cancelable (ctx only governed
	// the dial), so a process that setsid()-escapes the `timeout` process
	// group while keeping fd 1/2 open would block StdCopy forever. Copy in a
	// goroutine and race it against ctx; on expiry, Close() the attach to
	// unblock the read.
	var stdout, stderr bytes.Buffer
	copyDone := make(chan error, 1)
	go func() {
		_, cpErr := stdcopy.StdCopy(&stdout, &stderr, attach.Reader)
		copyDone <- cpErr
	}()

	ctxExpired := false
	select {
	case cpErr := <-copyDone:
		if cpErr != nil && ctx.Err() == nil {
			return ExecResult{}, cpErr
		}
	case <-ctx.Done():
		ctxExpired = true
		attach.Close()
		<-copyDone // reap the copier; Close above unblocks its read
	}

	// Inspect with a fresh short context: the exec ctx may already be
	// expired, and exit-code/TimedOut reporting must survive that.
	inspectCtx, cancelInspect := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelInspect()
	inspect, err := d.cli.ContainerExecInspect(inspectCtx, exec.ID)
	elapsed := time.Since(start)
	if err != nil {
		if ctxExpired {
			// A timeout is a result, not an error.
			return ExecResult{
				TimedOut: true,
				ExitCode: -1,
				Stderr:   "execution timed out and was force-disconnected",
				Duration: elapsed,
			}, nil
		}
		return ExecResult{}, err
	}
	timedOut := ctxExpired ||
		((inspect.ExitCode == 124 || inspect.ExitCode == 137) && elapsed >= timeout)
	return ExecResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: inspect.ExitCode,
		TimedOut: timedOut,
		Duration: elapsed,
	}, nil
}

// WriteFile writes one file into the container via an exec of `tee` with the
// content on stdin (argv-only — no shell interpolation of content or path,
// no size/quoting limits; "--" guards leading-dash paths). The Docker archive
// API (CopyToContainer) is unusable here: the daemon rejects it outright on a
// read-only rootfs ("container rootfs is marked read-only") even though the
// target is a tmpfs. Parent directories under /workspace are created first
// when needed.
func (d *dockerBackend) WriteFile(ctx context.Context, containerID, p string, content []byte) error {
	if dir := path.Dir(p); dir != workspace {
		res, err := d.Exec(ctx, containerID, []string{"mkdir", "-p", dir}, 10*time.Second)
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("mkdir %s failed: %s", dir, res.Stderr)
		}
	}
	res, err := d.execStdin(ctx, containerID, []string{"tee", "--", p}, content, 30*time.Second)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("write %s failed (exit %d): %s", p, res.ExitCode, res.Stderr)
	}
	return nil
}

// ReadFile reads one file out via an exec of `head -c` (argv-only, binary-
// safe through stdcopy), capped at limit bytes. The archive API
// (CopyFromContainer) cannot see tmpfs contents on this posture — existing
// files under the tmpfs /workspace report not-found — so file reads must go
// through exec too.
func (d *dockerBackend) ReadFile(ctx context.Context, containerID, p string, limit int) ([]byte, bool, error) {
	res, err := d.Exec(ctx, containerID, []string{"head", "-c", strconv.Itoa(limit + 1), "--", p}, 30*time.Second)
	if err != nil {
		return nil, false, err
	}
	if res.ExitCode != 0 {
		switch {
		case strings.Contains(res.Stderr, "No such file"):
			return nil, false, fmt.Errorf("%w: %s", ErrNoSuchFile, p)
		case strings.Contains(res.Stderr, "Is a directory"):
			return nil, false, fmt.Errorf("%s is not a regular file", p)
		default:
			return nil, false, fmt.Errorf("read %s failed (exit %d): %s", p, res.ExitCode, res.Stderr)
		}
	}
	content := []byte(res.Stdout)
	if len(content) > limit {
		return content[:limit], true, nil
	}
	return content, false, nil
}

// Remove force-removes the container.
func (d *dockerBackend) Remove(ctx context.Context, containerID string) error {
	return d.cli.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
}

// ListLeftovers returns every container (any state) carrying the sandbox
// label, for reap-on-start after a crash.
func (d *dockerBackend) ListLeftovers(ctx context.Context) ([]string, error) {
	list, err := d.cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", sandboxLabel+"=1")),
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
