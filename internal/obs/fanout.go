package obs

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

// ScrapeTarget is one supervised agent's metrics endpoint.
type ScrapeTarget struct {
	Agent string // agent id (used for up/skip series labels)
	Addr  string // host:port (the agent's listen_addr)
}

// perAgentTimeout bounds each sub-scrape so one sick agent can never stall
// the merged scrape (spec §3.4).
const perAgentTimeout = 500 * time.Millisecond

// FanoutHandler serves the merged exposition: the control registry's own
// families plus every healthy agent's families, merged by name (NOT text
// concatenation — duplicate TYPE/HELP blocks are invalid). Sub-scrapes run
// concurrently; skip rules: timeout/unreachable/non-200/parse ⇒ agent omitted
// this scrape + skip counter + up=0. A 404 means the process serves HTTP but
// has no /metrics (foreign shim) ⇒ reason no_metrics, up STAYS 1.
func FanoutHandler(c *ControlMetrics, targets func() []ScrapeTarget) http.Handler {
	client := &http.Client{}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		type result struct {
			agent    string
			families map[string]*dto.MetricFamily
		}
		ts := targets()
		results := make([]result, len(ts))
		var wg sync.WaitGroup
		for i, tgt := range ts {
			wg.Add(1)
			go func(i int, tgt ScrapeTarget) {
				defer wg.Done()
				fams, up, reason := scrapeOne(r.Context(), client, tgt)
				c.AgentUp(tgt.Agent, up)
				if reason != "" {
					c.ScrapeSkip(tgt.Agent, reason)
					level := slog.LevelWarn
					if reason == "no_metrics" {
						level = slog.LevelDebug
					}
					slog.Log(r.Context(), level, "metrics fan-out skip",
						"agent", tgt.Agent, "reason", reason)
				}
				results[i] = result{agent: tgt.Agent, families: fams}
			}(i, tgt)
		}
		wg.Wait()

		// Merge: own registry families first, then each agent's, by name.
		merged := map[string]*dto.MetricFamily{}
		own, err := c.reg.Gather()
		if err == nil {
			for _, mf := range own {
				merged[mf.GetName()] = mf
			}
		}
		for _, res := range results {
			for name, mf := range res.families {
				if exist, ok := merged[name]; ok {
					exist.Metric = append(exist.Metric, mf.Metric...)
				} else {
					merged[name] = mf
				}
			}
		}
		names := make([]string, 0, len(merged))
		for n := range merged {
			names = append(names, n)
		}
		sort.Strings(names)
		w.Header().Set("Content-Type", string(expfmt.NewFormat(expfmt.TypeTextPlain)))
		for _, n := range names {
			if _, err := expfmt.MetricFamilyToText(w, merged[n]); err != nil {
				return // client gone; nothing useful to do
			}
		}
	})
}

// scrapeOne fetches and parses one agent's exposition.
// Returns (families, up, skipReason); skipReason "" means scraped clean.
func scrapeOne(ctx context.Context, client *http.Client, tgt ScrapeTarget) (map[string]*dto.MetricFamily, bool, string) {
	ctx, cancel := context.WithTimeout(ctx, perAgentTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", "http://"+tgt.Addr+"/metrics", nil)
	if err != nil {
		return nil, false, "error"
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, false, "timeout"
		}
		return nil, false, "unreachable"
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, true, "no_metrics" // serving, just no endpoint (foreign shim)
	case resp.StatusCode != http.StatusOK:
		return nil, false, fmt.Sprintf("status_%d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, false, "error"
	}
	parser := expfmt.NewTextParser(model.UTF8Validation)
	fams, err := parser.TextToMetricFamilies(strings.NewReader(string(body)))
	if err != nil {
		return nil, false, "parse"
	}
	return fams, true, ""
}
