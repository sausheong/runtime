//go:build live

package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/client"
)

// TestDockerBackendLiveFileRoundTrip proves file I/O works against a REAL
// Docker daemon with the spec-mandated container posture (read-only rootfs,
// tmpfs /workspace). This is the regression gate for the archive-API bug:
// CopyToContainer is rejected on a read-only rootfs and CopyFromContainer
// cannot see tmpfs contents, so file I/O must ride exec (tee/head).
//
// Requires a running daemon and the runtime-sandbox:latest image
// (make sandbox-image); skips otherwise.
func TestDockerBackendLiveFileRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	probe, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Skipf("docker client init failed: %v", err)
	}
	if _, err := probe.Ping(ctx); err != nil {
		t.Skipf("docker daemon unreachable: %v", err)
	}
	if _, _, err := probe.ImageInspectWithRaw(ctx, "runtime-sandbox:latest"); err != nil {
		t.Skipf("image runtime-sandbox:latest missing (run `make sandbox-image`): %v", err)
	}

	be, err := NewDockerBackend(DockerConfig{})
	if err != nil {
		t.Fatalf("NewDockerBackend: %v", err)
	}

	id, err := be.Create(ctx, "live-test")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() {
		if err := be.Remove(context.Background(), id); err != nil {
			t.Errorf("Remove: %v", err)
		}
	}()

	// WriteFile into a subdirectory (exercises the mkdir -p path) with a few
	// hundred bytes including a unique marker.
	const marker = "LIVE_ROUNDTRIP_MARKER_7f3a"
	content := []byte("col_a,col_b\n" + strings.Repeat("1,2\n", 100) + marker + "\n")
	if err := be.WriteFile(ctx, id, "/workspace/sub/x.csv", content); err != nil {
		t.Fatalf("WriteFile /workspace/sub/x.csv: %v", err)
	}

	// Python reads it back: proves the bytes really landed in the tmpfs.
	res, err := be.Exec(ctx, id, []string{
		"python3", "-c", "print(open('/workspace/sub/x.csv').read())",
	}, 30*time.Second)
	if err != nil {
		t.Fatalf("Exec python read: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("python read exit %d, stderr: %s", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, marker) {
		t.Fatalf("python read stdout missing marker %q:\n%s", marker, res.Stdout)
	}

	// Python WRITES a file; ReadFile must see it (this is exactly what the
	// archive API could not do on tmpfs).
	res, err = be.Exec(ctx, id, []string{
		"python3", "-c", "open('/workspace/out.txt','w').write('hello from python\\n')",
	}, 30*time.Second)
	if err != nil {
		t.Fatalf("Exec python write: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("python write exit %d, stderr: %s", res.ExitCode, res.Stderr)
	}
	got, truncated, err := be.ReadFile(ctx, id, "/workspace/out.txt", 256<<10)
	if err != nil {
		t.Fatalf("ReadFile out.txt: %v", err)
	}
	if string(got) != "hello from python\n" {
		t.Fatalf("ReadFile out.txt = %q", got)
	}
	if truncated {
		t.Fatal("ReadFile out.txt reported truncated")
	}

	// Missing file ⇒ ErrNoSuchFile (typed, passes maskIfGone to the model).
	if _, _, err := be.ReadFile(ctx, id, "/workspace/missing.txt", 1024); !errors.Is(err, ErrNoSuchFile) {
		t.Fatalf("ReadFile missing.txt err = %v, want ErrNoSuchFile", err)
	}

	// Directory ⇒ "not a regular file".
	if _, _, err := be.ReadFile(ctx, id, "/workspace", 1024); err == nil ||
		!strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("ReadFile /workspace err = %v, want not-a-regular-file", err)
	}

	// Truncation: a small backend limit caps content and reports truncated.
	got, truncated, err = be.ReadFile(ctx, id, "/workspace/out.txt", 5)
	if err != nil {
		t.Fatalf("ReadFile (limit 5): %v", err)
	}
	if !truncated || string(got) != "hello" {
		t.Fatalf("ReadFile (limit 5) = %q truncated=%v, want \"hello\" true", got, truncated)
	}

	// Leading-dash filename: the "--" argv guard must keep tee/head from
	// parsing it as a flag.
	if err := be.WriteFile(ctx, id, "/workspace/-x.txt", []byte("dash")); err != nil {
		t.Fatalf("WriteFile -x.txt: %v", err)
	}
	got, _, err = be.ReadFile(ctx, id, "/workspace/-x.txt", 1024)
	if err != nil {
		t.Fatalf("ReadFile -x.txt: %v", err)
	}
	if string(got) != "dash" {
		t.Fatalf("ReadFile -x.txt = %q, want \"dash\"", got)
	}
}
