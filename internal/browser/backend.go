package browser

import (
	"context"
	"fmt"
	"sync"
)

// BrowserHandle is the connected form of one browser container: its CDP
// websocket endpoint plus the container id for removal. The Manager turns the
// endpoint into a chromedp context lazily on first action (a later task fills
// in the chromedp wiring; the fake leaves Endpoint empty).
type BrowserHandle struct {
	ContainerID string
	Endpoint    string // ws://… CDP endpoint; empty under the fake backend
}

// Backend abstracts the container engine for browser sandboxes. dockerBackend
// (docker.go, a later task) is the real implementation; fakeBackend serves
// hermetic tests and cmd/browserd's RUNTIME_BROWSER_FAKE mode.
type Backend interface {
	// Create starts one locked-down Chrome container wired to the egress proxy
	// at proxyAddr and returns its handle.
	Create(ctx context.Context, tenant, proxyAddr string) (BrowserHandle, error)
	// Remove force-removes the container.
	Remove(ctx context.Context, containerID string) error
	// ListLeftovers returns ids of all runtime.browser=1 containers (reap-on-start).
	ListLeftovers(ctx context.Context) ([]string, error)
}

// fakeBackend is an in-memory Backend: no Chrome, no Docker. Create returns a
// synthetic handle with an empty endpoint.
type fakeBackend struct {
	mu    sync.Mutex
	next  int
	boxes map[string]bool // containerID → exists
}

// NewFakeBackend returns the in-memory Backend (tests / RUNTIME_BROWSER_FAKE).
func NewFakeBackend() Backend {
	return &fakeBackend{boxes: map[string]bool{}}
}

func (f *fakeBackend) Create(_ context.Context, _, _ string) (BrowserHandle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	id := fmt.Sprintf("fake-%d", f.next)
	f.boxes[id] = true
	return BrowserHandle{ContainerID: id}, nil
}

func (f *fakeBackend) Remove(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.boxes[id] {
		return fmt.Errorf("fake backend: unknown container %q", id)
	}
	delete(f.boxes, id)
	return nil
}

func (f *fakeBackend) ListLeftovers(_ context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]string, 0, len(f.boxes))
	for id := range f.boxes {
		ids = append(ids, id)
	}
	return ids, nil
}
