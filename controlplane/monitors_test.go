package controlplane

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sausheong/runtime/internal/config"
)

// TestMonitorSet_StartReportsReachable: starting a monitor for a live backend
// flips the registry's reachability to reachable.
func TestMonitorSet_StartReportsReachable(t *testing.T) {
	var hits int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(200)
	}))
	defer backend.Close()

	reg := NewRegistry(&config.Config{}, "/bin/agentd", "dsn")
	reg.AddRemote(AgentInfo{ID: "a"}, AgentProcess{AgentID: "a", BaseURL: backend.URL}, true)

	ms := NewMonitorSet(context.Background(), reg, nil)
	ms.Start(AgentProcess{AgentID: "a", BaseURL: backend.URL})
	defer ms.Stop("a")

	// The first probe is immediate; give it a moment to land.
	waitFor(t, func() bool { return atomic.LoadInt32(&hits) >= 1 })
	if !reg.reachableOrUnknown("a", 0) {
		t.Fatal("agent should be reachable after a 200 probe")
	}
}

func TestMonitorSet_RemotePoolTracksEveryOrdinal(t *testing.T) {
	var codes [3]atomic.Int32
	var hits [3]atomic.Int32
	servers := make([]*httptest.Server, 3)
	for i := range servers {
		codes[i].Store(http.StatusOK)
		idx := i
		servers[i] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits[idx].Add(1)
			w.WriteHeader(int(codes[idx].Load()))
		}))
		defer servers[i].Close()
	}

	cfg := &config.Config{Agents: []config.AgentConfig{{
		ID: "pool", Name: "Pool", Model: "m",
		URL: "http://pool-{i}.example:8080", Replicas: 3,
	}}}
	reg := NewRegistry(cfg, "/bin/agentd", "dsn")
	var mu sync.Mutex
	observed := map[int]bool{}
	ms := NewMonitorSet(context.Background(), reg, func(_ string, replica int, reachable bool) {
		mu.Lock()
		observed[replica] = reachable
		mu.Unlock()
	})
	ms.interval = 10 * time.Millisecond
	for i := range servers {
		ms.Start(AgentProcess{
			AgentID: "pool", ReplicaIndex: i, Remote: true, BaseURL: servers[i].URL,
		})
	}
	defer ms.Stop("pool")

	waitFor(t, func() bool {
		return hits[0].Load() >= 2 && hits[1].Load() >= 2 && hits[2].Load() >= 2
	})
	codes[1].Store(http.StatusServiceUnavailable)
	waitFor(t, func() bool { return !reg.reachableOrUnknown("pool", 1) })
	if !reg.reachableOrUnknown("pool", 0) || !reg.reachableOrUnknown("pool", 2) {
		t.Fatal("healthy ordinals were affected by ordinal 1")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, replica := range []int{0, 1, 2} {
		if _, ok := observed[replica]; !ok {
			t.Fatalf("no metric callback observed for replica %d: %v", replica, observed)
		}
	}
}

func TestMonitorSet_StopReplicaDoesNotStopSiblings(t *testing.T) {
	var hits [2]atomic.Int32
	servers := make([]*httptest.Server, 2)
	for i := range servers {
		idx := i
		servers[i] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits[idx].Add(1)
			w.WriteHeader(http.StatusOK)
		}))
		defer servers[i].Close()
	}
	ms := NewMonitorSet(context.Background(), NewRegistry(&config.Config{}, "", ""), nil)
	ms.interval = 10 * time.Millisecond
	for i := range servers {
		ms.Start(AgentProcess{AgentID: "pool", ReplicaIndex: i, BaseURL: servers[i].URL})
	}
	waitFor(t, func() bool { return hits[0].Load() >= 2 && hits[1].Load() >= 2 })
	ms.StopReplica("pool", 0)
	stoppedAt := hits[0].Load()
	siblingAt := hits[1].Load()
	waitFor(t, func() bool { return hits[1].Load() > siblingAt+1 })
	if got := hits[0].Load(); got != stoppedAt {
		t.Fatalf("stopped replica continued probing: %d -> %d", stoppedAt, got)
	}
	ms.Stop("pool")
}

// TestMonitorSet_StopHaltsProbing: after Stop, no further probes hit the backend.
func TestMonitorSet_StopHaltsProbing(t *testing.T) {
	var hits int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(200)
	}))
	defer backend.Close()

	reg := NewRegistry(&config.Config{}, "/bin/agentd", "dsn")
	ms := NewMonitorSet(context.Background(), reg, nil)
	ms.Start(AgentProcess{AgentID: "a", BaseURL: backend.URL})
	waitFor(t, func() bool { return atomic.LoadInt32(&hits) >= 1 })
	ms.Stop("a")
	settled := atomic.LoadInt32(&hits)
	// No ticker should fire after Stop (interval is 10s; we wait far less, but
	// the cancel must prevent any in-flight reschedule).
	time.Sleep(50 * time.Millisecond)
	if atomic.LoadInt32(&hits) != settled {
		t.Fatalf("probes continued after Stop: %d -> %d", settled, atomic.LoadInt32(&hits))
	}
}

// TestMonitorSet_RestartResetsReachability: Restart clears a prior reachable
// state to unknown and re-probes.
func TestMonitorSet_RestartResetsReachability(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer backend.Close()

	reg := NewRegistry(&config.Config{}, "/bin/agentd", "dsn")
	reg.AddRemote(AgentInfo{ID: "a"}, AgentProcess{AgentID: "a", BaseURL: backend.URL}, true)
	reg.SetReachable("a", 0, false) // pretend a prior probe marked it down

	ms := NewMonitorSet(context.Background(), reg, nil)
	ms.Restart(AgentProcess{AgentID: "a", BaseURL: backend.URL})
	defer ms.Stop("a")

	// Restart resets to unknown then re-probes a live backend → reachable.
	waitFor(t, func() bool { return reg.reachableOrUnknown("a", 0) })
}

func TestMonitorSet_ReplacementIgnoresSupersededCallback(t *testing.T) {
	oldStarted := make(chan struct{})
	oldReleased := make(chan struct{})
	oldBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(oldStarted)
		<-r.Context().Done()
		close(oldReleased)
	}))
	defer oldBackend.Close()
	newBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer newBackend.Close()

	reg := NewRegistry(&config.Config{}, "/bin/agentd", "dsn")
	reg.AddRemote(AgentInfo{ID: "a"}, AgentProcess{AgentID: "a", BaseURL: oldBackend.URL}, true)
	ms := NewMonitorSet(context.Background(), reg, nil)
	ms.Start(AgentProcess{AgentID: "a", BaseURL: oldBackend.URL})
	select {
	case <-oldStarted:
	case <-time.After(time.Second):
		t.Fatal("old monitor did not begin its probe")
	}

	ms.Restart(AgentProcess{AgentID: "a", BaseURL: newBackend.URL})
	defer ms.Stop("a")
	waitFor(t, func() bool { return reg.reachableOrUnknown("a", 0) })
	select {
	case <-oldReleased:
	case <-time.After(time.Second):
		t.Fatal("superseded monitor request was not cancelled")
	}

	// Give the old Run goroutine time to deliver the cancelled probe's false
	// transition. The generation fence must discard it.
	time.Sleep(20 * time.Millisecond)
	if !reg.reachableOrUnknown("a", 0) {
		t.Fatal("superseded monitor overwrote replacement reachability")
	}
}
