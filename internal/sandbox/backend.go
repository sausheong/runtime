package sandbox

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ExecResult is the outcome of one exec inside a sandbox container.
type ExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	TimedOut bool
	Duration time.Duration
}

// Backend abstracts the container engine. The real implementation is
// dockerBackend (docker.go); fakeBackend serves hermetic tests and the
// RUNTIME_SANDBOX_FAKE e2e mode.
type Backend interface {
	// Create starts one locked-down sandbox container and returns its id.
	Create(ctx context.Context, tenant string) (containerID string, err error)
	// Exec runs argv inside the container with a wall-clock timeout that
	// kills the process (never the container).
	Exec(ctx context.Context, containerID string, argv []string, timeout time.Duration) (ExecResult, error)
	// WriteFile/ReadFile move bytes in and out of the container. path is
	// already confined (callers run confinePath first). ReadFile returns at
	// most limit bytes, reporting truncation.
	WriteFile(ctx context.Context, containerID, path string, content []byte) error
	ReadFile(ctx context.Context, containerID, path string, limit int) (content []byte, truncated bool, err error)
	// Remove force-removes the container. Removing an unknown id is an error.
	Remove(ctx context.Context, containerID string) error
	// ListLeftovers returns ids of all runtime.sandbox=1 containers (for
	// reap-on-start).
	ListLeftovers(ctx context.Context) ([]string, error)
}

// fakeBackend is an in-memory Backend: files are a map, exec echoes its
// argv. It backs unit tests and sandboxd's RUNTIME_SANDBOX_FAKE mode (the
// through-serve e2e without Docker).
type fakeBackend struct {
	mu    sync.Mutex
	next  int
	boxes map[string]map[string][]byte // containerID → path → content
}

// NewFakeBackend returns the in-memory Backend (tests / RUNTIME_SANDBOX_FAKE).
func NewFakeBackend() Backend {
	return &fakeBackend{boxes: map[string]map[string][]byte{}}
}

func (f *fakeBackend) Create(ctx context.Context, tenant string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	id := fmt.Sprintf("fake-%d", f.next)
	f.boxes[id] = map[string][]byte{}
	return id, nil
}

func (f *fakeBackend) Exec(ctx context.Context, containerID string, argv []string, timeout time.Duration) (ExecResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.boxes[containerID]; !ok {
		return ExecResult{}, fmt.Errorf("fake backend: unknown container %q", containerID)
	}
	return ExecResult{Stdout: "fake exec: " + strings.Join(argv, " ")}, nil
}

func (f *fakeBackend) WriteFile(ctx context.Context, containerID, path string, content []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	files, ok := f.boxes[containerID]
	if !ok {
		return fmt.Errorf("fake backend: unknown container %q", containerID)
	}
	files[path] = append([]byte(nil), content...)
	return nil
}

func (f *fakeBackend) ReadFile(ctx context.Context, containerID, path string, limit int) ([]byte, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	files, ok := f.boxes[containerID]
	if !ok {
		return nil, false, fmt.Errorf("fake backend: unknown container %q", containerID)
	}
	content, ok := files[path]
	if !ok {
		return nil, false, fmt.Errorf("fake backend: no such file %q in container %q", path, containerID)
	}
	if limit >= 0 && len(content) > limit {
		return append([]byte(nil), content[:limit]...), true, nil
	}
	return append([]byte(nil), content...), false, nil
}

func (f *fakeBackend) Remove(ctx context.Context, containerID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.boxes[containerID]; !ok {
		return fmt.Errorf("fake backend: unknown container %q", containerID)
	}
	delete(f.boxes, containerID)
	return nil
}

func (f *fakeBackend) ListLeftovers(ctx context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]string, 0, len(f.boxes))
	for id := range f.boxes {
		ids = append(ids, id)
	}
	return ids, nil
}
