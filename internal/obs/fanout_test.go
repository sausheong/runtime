package obs

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// fakeAgent serves a fixed body (or behavior) at /metrics.
func fakeAgent(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func exposition(agent string) string {
	return fmt.Sprintf(`# HELP agent_turns_total Agent turns by outcome (completed/error/aborted/continue).
# TYPE agent_turns_total counter
agent_turns_total{agent=%q,outcome="completed"} 3
`, agent)
}

func TestFanoutMergesHealthyAgents(t *testing.T) {
	a1 := fakeAgent(t, func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, exposition("alpha")) })
	a2 := fakeAgent(t, func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, exposition("beta")) })
	c := NewControlMetrics()
	h := FanoutHandler(c, func() []ScrapeTarget {
		return []ScrapeTarget{{Agent: "alpha", Addr: a1}, {Agent: "beta", Addr: a2}}
	})
	body := scrapeHandler(t, h)
	// Exactly ONE TYPE line for the family despite two agents (merge, not concat).
	if n := strings.Count(body, "# TYPE agent_turns_total counter"); n != 1 {
		t.Fatalf("TYPE lines for agent_turns_total = %d, want 1\n%s", n, body)
	}
	for _, agent := range []string{"alpha", "beta"} {
		if !strings.Contains(body, fmt.Sprintf(`agent_turns_total{agent=%q,outcome="completed"} 3`, agent)) {
			t.Fatalf("missing %s series:\n%s", agent, body)
		}
	}
	// Control-plane families present too.
	if !strings.Contains(body, "runtime_agent_up") {
		t.Fatalf("control families missing:\n%s", body)
	}
	if v := testutil.ToFloat64(c.agentUp.WithLabelValues("alpha")); v != 1 {
		t.Fatalf("alpha up = %v, want 1", v)
	}
}

func TestFanoutSkipsHangingAgent(t *testing.T) {
	healthy := fakeAgent(t, func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, exposition("alpha")) })
	hung := fakeAgent(t, func(w http.ResponseWriter, _ *http.Request) { time.Sleep(3 * time.Second) })
	c := NewControlMetrics()
	h := FanoutHandler(c, func() []ScrapeTarget {
		return []ScrapeTarget{{Agent: "alpha", Addr: healthy}, {Agent: "hung", Addr: hung}}
	})
	start := time.Now()
	body := scrapeHandler(t, h)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("scrape took %v — hung agent not bounded by timeout", elapsed)
	}
	if !strings.Contains(body, `agent_turns_total{agent="alpha"`) {
		t.Fatal("healthy agent missing")
	}
	if v := testutil.ToFloat64(c.agentUp.WithLabelValues("hung")); v != 0 {
		t.Fatalf("hung up = %v, want 0", v)
	}
	if v := testutil.ToFloat64(c.scrapeSkips.WithLabelValues("hung", "timeout")); v != 1 {
		t.Fatalf("skip counter = %v, want 1", v)
	}
}

func TestFanoutSkipsMalformedExposition(t *testing.T) {
	bad := fakeAgent(t, func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "{{{not exposition") })
	c := NewControlMetrics()
	h := FanoutHandler(c, func() []ScrapeTarget {
		return []ScrapeTarget{{Agent: "bad", Addr: bad}}
	})
	body := scrapeHandler(t, h)
	if !strings.Contains(body, "runtime_agent_up") {
		t.Fatal("merged output invalid")
	}
	if v := testutil.ToFloat64(c.agentUp.WithLabelValues("bad")); v != 0 {
		t.Fatalf("bad up = %v, want 0", v)
	}
	if v := testutil.ToFloat64(c.scrapeSkips.WithLabelValues("bad", "parse")); v != 1 {
		t.Fatalf("skip reason parse = %v, want 1", v)
	}
}

func TestFanout404IsNoMetricsButStillUp(t *testing.T) {
	shim := fakeAgent(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	c := NewControlMetrics()
	h := FanoutHandler(c, func() []ScrapeTarget {
		return []ScrapeTarget{{Agent: "shim", Addr: shim}}
	})
	_ = scrapeHandler(t, h)
	if v := testutil.ToFloat64(c.agentUp.WithLabelValues("shim")); v != 1 {
		t.Fatalf("shim up = %v, want 1 (404 proves serving)", v)
	}
	if v := testutil.ToFloat64(c.scrapeSkips.WithLabelValues("shim", "no_metrics")); v != 1 {
		t.Fatalf("skip reason no_metrics = %v, want 1", v)
	}
}

func TestFanoutUnreachableAgent(t *testing.T) {
	c := NewControlMetrics()
	h := FanoutHandler(c, func() []ScrapeTarget {
		return []ScrapeTarget{{Agent: "gone", Addr: "127.0.0.1:1"}}
	})
	body := scrapeHandler(t, h)
	if !strings.Contains(body, "runtime_metrics_scrape_skips_total") {
		t.Fatalf("skip counter family missing:\n%s", body)
	}
	if v := testutil.ToFloat64(c.agentUp.WithLabelValues("gone")); v != 0 {
		t.Fatalf("gone up = %v, want 0", v)
	}
}
