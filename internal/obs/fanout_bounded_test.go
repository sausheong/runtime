package obs

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sausheong/runtime/internal/xfan"
)

// TestFanoutBoundsConcurrentScrapes is the large-registry regression for the
// resource half of RT-11: before the xfan conversion, FanoutHandler started one
// goroutine (and one socket) per registered target, so a single inbound
// /metrics scrape of a 200-agent registry opened 200 concurrent outbound
// requests. Every target here points at ONE server that records its own
// high-water mark of in-flight requests, so the assertion is on real observed
// socket concurrency, not on the helper's bookkeeping.
func TestFanoutBoundsConcurrentScrapes(t *testing.T) {
	const targets = 200

	var cur, peak atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		c := cur.Add(1)
		// CAS-raise the high-water mark (same shape as
		// TestEachBoundsPeakConcurrency): lock-free and no lost update.
		for {
			p := peak.Load()
			if c <= p || peak.CompareAndSwap(p, c) {
				break
			}
		}
		// Long enough that overlap is observable, short enough to stay well
		// inside the 500ms per-scrape timeout.
		time.Sleep(2 * time.Millisecond)
		cur.Add(-1)
		// The agent label here is irrelevant: the handler overwrites it
		// server-side with the registered target id (injectTargetLabels).
		fmt.Fprint(w, exposition("unset"))
	}))
	t.Cleanup(srv.Close)

	c := NewControlMetrics()
	h := FanoutHandler(c, func() []ScrapeTarget {
		ts := make([]ScrapeTarget, targets)
		for i := range ts {
			ts[i] = ScrapeTarget{Agent: fmt.Sprintf("agent-%03d", i), BaseURL: srv.URL}
		}
		return ts
	})

	body := scrapeHandler(t, h)
	mustParseClean(t, body)

	if p := peak.Load(); p > xfan.DefaultLimit {
		t.Fatalf("peak concurrent scrapes = %d, want <= %d", p, xfan.DefaultLimit)
	}
	if p := peak.Load(); p < 2 {
		t.Fatalf("peak concurrent scrapes = %d, want real parallelism", p)
	}
	// Bounding must not drop work: every target still contributed a series.
	for _, agent := range []string{"agent-000", "agent-100", "agent-199"} {
		if !strings.Contains(body, fmt.Sprintf(`agent=%q`, agent)) {
			t.Fatalf("target %s missing from merged body (work dropped)", agent)
		}
	}
}
