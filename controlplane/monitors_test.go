package controlplane

import (
	"context"
	"net/http"
	"net/http/httptest"
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
