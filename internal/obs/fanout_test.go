package obs

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
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

// mustParseClean asserts the merged body is a valid exposition that the
// Prometheus text parser accepts whole (no truncation, no duplicate
// TYPE/HELP, no type conflicts).
func mustParseClean(t *testing.T, body string) {
	t.Helper()
	parser := expfmt.NewTextParser(model.UTF8Validation)
	if _, err := parser.TextToMetricFamilies(strings.NewReader(body)); err != nil {
		t.Fatalf("merged body not parseable: %v\n%s", err, body)
	}
}

func TestFanoutMergesHealthyAgents(t *testing.T) {
	a1 := fakeAgent(t, func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, exposition("alpha")) })
	a2 := fakeAgent(t, func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, exposition("beta")) })
	c := NewControlMetrics()
	h := FanoutHandler(c, func() []ScrapeTarget {
		return []ScrapeTarget{{Agent: "alpha", BaseURL: "http://" + a1}, {Agent: "beta", BaseURL: "http://" + a2}}
	})
	body := scrapeHandler(t, h)
	mustParseClean(t, body)
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
	hung := fakeAgent(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	})
	c := NewControlMetrics()
	h := FanoutHandler(c, func() []ScrapeTarget {
		return []ScrapeTarget{{Agent: "alpha", BaseURL: "http://" + healthy}, {Agent: "hung", BaseURL: "http://" + hung}}
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
		return []ScrapeTarget{{Agent: "bad", BaseURL: "http://" + bad}}
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
		return []ScrapeTarget{{Agent: "shim", BaseURL: "http://" + shim}}
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
		return []ScrapeTarget{{Agent: "gone", BaseURL: "http://127.0.0.1:1"}}
	})
	body := scrapeHandler(t, h)
	if !strings.Contains(body, "runtime_metrics_scrape_skips_total") {
		t.Fatalf("skip counter family missing:\n%s", body)
	}
	if v := testutil.ToFloat64(c.agentUp.WithLabelValues("gone")); v != 0 {
		t.Fatalf("gone up = %v, want 0", v)
	}
}

func TestFanoutEnforcesAgentLabel(t *testing.T) {
	// The agent claims agent="liar" in its own exposition but is registered
	// as "honest". The merge must overwrite the label server-side.
	liar := fakeAgent(t, func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, exposition("liar")) })
	c := NewControlMetrics()
	h := FanoutHandler(c, func() []ScrapeTarget {
		return []ScrapeTarget{{Agent: "honest", BaseURL: "http://" + liar}}
	})
	body := scrapeHandler(t, h)
	mustParseClean(t, body)
	if !strings.Contains(body, `agent_turns_total{agent="honest",outcome="completed"} 3`) {
		t.Fatalf("registered agent label not enforced:\n%s", body)
	}
	if strings.Contains(body, `agent="liar"`) {
		t.Fatalf("agent-claimed label leaked through:\n%s", body)
	}
}

func TestFanoutDropsReservedControlFamilies(t *testing.T) {
	// Agent tries to inject a control-plane family (runtime_*). Control
	// families are authoritative: the spoof must be dropped wholesale.
	spoof := fakeAgent(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `# TYPE runtime_agent_up gauge
runtime_agent_up{agent="evil"} 1
`+exposition("sneaky"))
	})
	c := NewControlMetrics()
	h := FanoutHandler(c, func() []ScrapeTarget {
		return []ScrapeTarget{{Agent: "sneaky", BaseURL: "http://" + spoof}}
	})
	body := scrapeHandler(t, h)
	mustParseClean(t, body)
	if strings.Contains(body, `runtime_agent_up{agent="evil"}`) {
		t.Fatalf("spoofed control series leaked through:\n%s", body)
	}
	// The control registry's own series for the registered agent wins.
	if !strings.Contains(body, `runtime_agent_up{agent="sneaky"} 1`) {
		t.Fatalf("control registry's own runtime_agent_up missing:\n%s", body)
	}
	// Legitimate (non-reserved) families from the same agent still merge.
	if !strings.Contains(body, `agent_turns_total{agent="sneaky",outcome="completed"} 3`) {
		t.Fatalf("agent's legitimate family lost:\n%s", body)
	}
	if v := testutil.ToFloat64(c.scrapeSkips.WithLabelValues("sneaky", "reserved_name")); v != 1 {
		t.Fatalf("skip reason reserved_name = %v, want 1", v)
	}
}

func TestFanoutTypeConflictDropsFamily(t *testing.T) {
	// Two agents expose the same family name with conflicting TYPEs. Exactly
	// one survives (scrape completion order is concurrent, so either may win)
	// and the other is skipped with reason type_conflict.
	one := fakeAgent(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "# TYPE flaky_metric counter\nflaky_metric 1\n")
	})
	two := fakeAgent(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "# TYPE flaky_metric gauge\nflaky_metric 2\n")
	})
	c := NewControlMetrics()
	h := FanoutHandler(c, func() []ScrapeTarget {
		return []ScrapeTarget{{Agent: "one", BaseURL: "http://" + one}, {Agent: "two", BaseURL: "http://" + two}}
	})
	body := scrapeHandler(t, h)
	mustParseClean(t, body)
	if n := strings.Count(body, "# TYPE flaky_metric"); n != 1 {
		t.Fatalf("TYPE lines for flaky_metric = %d, want exactly 1:\n%s", n, body)
	}
	skipOne := testutil.ToFloat64(c.scrapeSkips.WithLabelValues("one", "type_conflict"))
	skipTwo := testutil.ToFloat64(c.scrapeSkips.WithLabelValues("two", "type_conflict"))
	if skipOne+skipTwo != 1 {
		t.Fatalf("type_conflict skips: one=%v two=%v, want exactly one total", skipOne, skipTwo)
	}
	switch {
	case skipTwo == 1: // agent one's counter survived
		if !strings.Contains(body, "# TYPE flaky_metric counter") ||
			!strings.Contains(body, `flaky_metric{agent="one"} 1`) {
			t.Fatalf("survivor should be agent one's counter:\n%s", body)
		}
		if strings.Contains(body, `flaky_metric{agent="two"}`) {
			t.Fatalf("dropped agent two's series leaked:\n%s", body)
		}
	case skipOne == 1: // agent two's gauge survived
		if !strings.Contains(body, "# TYPE flaky_metric gauge") ||
			!strings.Contains(body, `flaky_metric{agent="two"} 2`) {
			t.Fatalf("survivor should be agent two's gauge:\n%s", body)
		}
		if strings.Contains(body, `flaky_metric{agent="one"}`) {
			t.Fatalf("dropped agent one's series leaked:\n%s", body)
		}
	}
}

func TestFanout_RemoteTargetUsesBaseURLAndToken(t *testing.T) {
	const token = "scrape-tok"
	var sawAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		sawAuth = r.Header.Get("Authorization")
		fmt.Fprint(w, exposition("remote"))
	}))
	defer srv.Close()

	c := NewControlMetrics()
	h := FanoutHandler(c, func() []ScrapeTarget {
		return []ScrapeTarget{{Agent: "remote", BaseURL: srv.URL, Token: token}}
	})
	body := scrapeHandler(t, h)
	mustParseClean(t, body)
	if !strings.Contains(body, `agent_turns_total{agent="remote",outcome="completed"} 3`) {
		t.Fatalf("remote series missing:\n%s", body)
	}
	if sawAuth != "Bearer "+token {
		t.Fatalf("scrape Authorization = %q", sawAuth)
	}
}
