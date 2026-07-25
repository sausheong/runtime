package controlplane

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The monitor flips reachable→unreachable and fires OnChange only on
// transitions (edge-triggered), and sends the bearer on its probe.
func TestHealthMonitor_TransitionsAndBearer(t *testing.T) {
	var up atomic.Bool
	up.Store(true)
	var sawAuth atomic.Value
	sawAuth.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth.Store(r.Header.Get("Authorization"))
		if up.Load() {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()

	var mu sync.Mutex
	var changes []bool
	hm := &HealthMonitor{
		BaseURL:  srv.URL,
		Token:    "mon-tok",
		Interval: 10 * time.Millisecond,
		OnChange: func(ok bool) { mu.Lock(); changes = append(changes, ok); mu.Unlock() },
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hm.Run(ctx)

	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(changes) >= 1 && changes[0] })

	up.Store(false)
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(changes) >= 2 && !changes[len(changes)-1]
	})

	mu.Lock()
	n := len(changes)
	mu.Unlock()
	if n > 5 {
		t.Fatalf("OnChange fired %d times — not edge-triggered (should fire only on transitions)", n)
	}
	if a := sawAuth.Load().(string); a != "Bearer mon-tok" {
		t.Fatalf("probe Authorization = %q", a)
	}
}

func TestHealthMonitor_StopsOnCtxCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	hm := &HealthMonitor{BaseURL: srv.URL, Interval: 5 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { hm.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

func TestHealthMonitor_RejectsCrossOriginRedirect(t *testing.T) {
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	source := httptest.NewServer(http.RedirectHandler(target.URL, http.StatusFound))
	defer source.Close()

	state := make(chan bool, 1)
	hm := &HealthMonitor{
		BaseURL:  source.URL,
		Token:    "must-not-follow",
		Interval: time.Hour,
		OnChange: func(ok bool) { state <- ok },
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hm.Run(ctx)

	select {
	case ok := <-state:
		if ok {
			t.Fatal("redirecting agent reported healthy")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("monitor did not report initial state")
	}
	if got := targetCalls.Load(); got != 0 {
		t.Fatalf("cross-origin redirect target called %d times", got)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}
