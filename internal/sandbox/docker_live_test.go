//go:build live

package sandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/client"
)

// requireLiveDocker skips unless a daemon is reachable and the bundled
// sandbox image exists (run `make sandbox-image` to build it).
func requireLiveDocker(t *testing.T, ctx context.Context) {
	t.Helper()
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
}

// TestDockerBackendLiveFileRoundTrip proves file I/O works against a REAL
// Docker daemon with the spec-mandated container posture (read-only rootfs,
// tmpfs /workspace). This is the regression gate for the archive-API bug:
// CopyToContainer is rejected on a read-only rootfs and CopyFromContainer
// cannot see tmpfs contents, so file I/O must ride exec (dd/head).
//
// Requires a running daemon and the runtime-sandbox:latest image
// (make sandbox-image); skips otherwise.
func TestDockerBackendLiveFileRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	requireLiveDocker(t, ctx)

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

	// Leading-dash filename: confinePath output is always /workspace-rooted,
	// so a dash-leading NAME arrives as /workspace/-x.txt — dd's of= operand
	// takes it verbatim, and head keeps its "--" guard.
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

// TestDockerBackendLiveLargeWrite is the regression gate for the exec-stdin
// deadlock: with the old tee-based WriteFile, stdin was written synchronously
// BEFORE the output drainer started. tee echoed stdin to stdout; once the
// undrained stdout filled the pipe/socket buffers (~0.5-1.5 MiB slack on
// Linux unix sockets), tee blocked on write(1), stopped reading stdin, and
// the client's Conn.Write blocked forever. 8 MiB is far past any buffer
// slack on every platform (and fits the 64 MiB tmpfs /workspace), so a
// reintroduced ordering bug fails this test by timeout instead of hanging
// the suite.
func TestDockerBackendLiveLargeWrite(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	requireLiveDocker(t, ctx)

	be, err := NewDockerBackend(DockerConfig{})
	if err != nil {
		t.Fatalf("NewDockerBackend: %v", err)
	}
	id, err := be.Create(ctx, "live-large")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() {
		if err := be.Remove(context.Background(), id); err != nil {
			t.Errorf("Remove: %v", err)
		}
	}()

	// Known pattern, 8 MiB.
	content := bytes.Repeat([]byte("0123456789abcdef"), 8<<20/16)
	wantSum := sha256.Sum256(content)
	want := hex.EncodeToString(wantSum[:])

	// Bound the write so a regression fails fast instead of wedging the
	// suite until the 3-minute ctx (WriteFile's internal exec timeout is
	// 30s; a deadlocked stdin write never reaches it).
	done := make(chan error, 1)
	go func() { done <- be.WriteFile(ctx, id, "/workspace/big.bin", content) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WriteFile 8MiB: %v", err)
		}
	case <-time.After(90 * time.Second):
		t.Fatal("DEADLOCK: WriteFile 8MiB did not return within 90s (stdin write blocked against undrained exec output)")
	}

	// Hash inside the container: proves every byte landed, without hauling
	// 8 MiB back through the exec stream.
	res, err := be.Exec(ctx, id, []string{
		"python3", "-c",
		"import hashlib; print(hashlib.sha256(open('/workspace/big.bin','rb').read()).hexdigest())",
	}, 60*time.Second)
	if err != nil {
		t.Fatalf("Exec python sha256: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("python sha256 exit %d, stderr: %s", res.ExitCode, res.Stderr)
	}
	if got := strings.TrimSpace(res.Stdout); got != want {
		t.Fatalf("sha256 mismatch: container %s, local %s", got, want)
	}

	// ReadFile at the 256 KiB default limit must come back truncated with
	// exactly the first limit bytes.
	got, truncated, err := be.ReadFile(ctx, id, "/workspace/big.bin", 256<<10)
	if err != nil {
		t.Fatalf("ReadFile big.bin (256KiB limit): %v", err)
	}
	if !truncated {
		t.Fatal("ReadFile big.bin at 256KiB limit not reported truncated")
	}
	if len(got) != 256<<10 || !bytes.Equal(got, content[:256<<10]) {
		t.Fatalf("ReadFile big.bin truncated content wrong: %d bytes", len(got))
	}
}
