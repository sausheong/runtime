package obs

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

// ScrapeTarget is one agent's metrics endpoint. BaseURL is the full dial base
// "scheme://host:port"; Token (optional) is the shared bearer for remote agents.
type ScrapeTarget struct {
	Agent   string // agent id (used for up/skip series labels)
	Replica int    // 0-based replica index, surfaced as the "replica" label
	BaseURL string // full base, e.g. "http://127.0.0.1:8101" or "https://h:8443"
	Token   string // optional bearer ("" ⇒ no auth header)
}

// perAgentTimeout bounds each sub-scrape so one sick agent can never stall
// the merged scrape (spec §3.4).
const perAgentTimeout = 500 * time.Millisecond

// agentLabel is injected (server-side) into every metric scraped from an
// agent, overwriting whatever the agent claimed. Agents are NOT trusted to
// label themselves: the registered target identity is authoritative, which
// makes series disjoint across agents by construction.
const agentLabel = "agent"

// replicaLabel is injected (server-side) alongside agentLabel into every
// scraped metric, identifying which replica of the agent the series came from.
const replicaLabel = "replica"

// FanoutHandler serves the merged exposition: the control registry's own
// families plus every healthy agent's families, merged by name (NOT text
// concatenation — duplicate TYPE/HELP blocks are invalid). Sub-scrapes run
// concurrently; skip rules: timeout/unreachable/non-200/parse ⇒ agent omitted
// this scrape + skip counter + up=0. A 404 means the process serves HTTP but
// has no /metrics (foreign shim) ⇒ reason no_metrics, up STAYS 1.
//
// Merge hardening (agents are untrusted):
//   - every agent metric gets agent=<registered id> injected/overwritten
//     server-side (label-lying is impossible);
//   - agent families colliding with control families (or any runtime_* name —
//     the control plane owns that namespace) are dropped, reason
//     reserved_name;
//   - agent families colliding with another agent's family of a different
//     TYPE are dropped, reason type_conflict (same-type collisions are safe:
//     the injected agent label keeps series disjoint);
//   - each family is encoded into a buffer first, so a single bad family is
//     skipped instead of truncating the whole response mid-stream.
func FanoutHandler(c *ControlMetrics, targets func() []ScrapeTarget) http.Handler {
	client := &http.Client{}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		type result struct {
			agent    string
			replica  int
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
				c.AgentUp(tgt.Agent, tgt.Replica, up)
				if reason != "" {
					c.ScrapeSkip(tgt.Agent, tgt.Replica, reason)
					level := slog.LevelWarn
					if reason == "no_metrics" {
						level = slog.LevelDebug
					}
					slog.Log(r.Context(), level, "metrics fan-out skip",
						"agent", tgt.Agent, "reason", reason)
				}
				results[i] = result{agent: tgt.Agent, replica: tgt.Replica, families: fams}
			}(i, tgt)
		}
		wg.Wait()

		// Merge: own registry families first, then each agent's, by name.
		// Control families are authoritative — agents may not contribute to
		// them. Note Gather() omits vecs with zero series, hence the extra
		// runtime_* prefix guard below.
		merged := map[string]*dto.MetricFamily{}
		reserved := map[string]struct{}{}
		own, err := c.reg.Gather()
		if err == nil {
			for _, mf := range own {
				merged[mf.GetName()] = mf
				reserved[mf.GetName()] = struct{}{}
			}
		}
		for _, res := range results {
			for name, mf := range res.families {
				// Note: reserved_name/type_conflict skip increments below land in
				// the NEXT exposition — the own registry was gathered above,
				// before this merge loop.
				if _, isReserved := reserved[name]; isReserved || strings.HasPrefix(name, "runtime_") {
					c.ScrapeSkip(res.agent, res.replica, "reserved_name")
					slog.Warn("metrics fan-out: dropped reserved control family",
						"agent", res.agent, "family", name)
					continue
				}
				injectTargetLabels(mf, res.agent, res.replica)
				if exist, ok := merged[name]; ok {
					if exist.GetType() != mf.GetType() {
						c.ScrapeSkip(res.agent, res.replica, "type_conflict")
						slog.Warn("metrics fan-out: dropped type-conflicting family",
							"agent", res.agent, "family", name)
						continue
					}
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
		var buf bytes.Buffer
		for _, n := range names {
			buf.Reset()
			if _, err := expfmt.MetricFamilyToText(&buf, merged[n]); err != nil {
				// Data error in ONE family must not truncate the response:
				// a half-written exposition is rejected by Prometheus
				// wholesale. Skip just this family.
				slog.Warn("metrics fan-out: family failed to encode; dropped",
					"family", n, "err", err)
				continue
			}
			if _, err := w.Write(buf.Bytes()); err != nil {
				return // client gone; nothing useful to do
			}
		}
	})
}

// injectTargetLabels overwrites (or appends) agent=<agent> and replica=<replica>
// on every metric in mf, then re-sorts labels. The registered target identity is
// authoritative; agents are not trusted to label themselves.
func injectTargetLabels(mf *dto.MetricFamily, agent string, replica int) {
	for _, m := range mf.Metric {
		setLabel(m, agentLabel, agent)
		setLabel(m, replicaLabel, strconv.Itoa(replica))
		sort.Slice(m.Label, func(i, j int) bool {
			return m.Label[i].GetName() < m.Label[j].GetName()
		})
	}
}

// setLabel overwrites an existing label of the given name or appends it.
func setLabel(m *dto.Metric, name, val string) {
	for _, lp := range m.Label {
		if lp.GetName() == name {
			v := val
			lp.Value = &v
			return
		}
	}
	n, v := name, val
	m.Label = append(m.Label, &dto.LabelPair{Name: &n, Value: &v})
}

// scrapeOne fetches and parses one agent's exposition.
// Returns (families, up, skipReason); skipReason "" means scraped clean.
func scrapeOne(ctx context.Context, client *http.Client, tgt ScrapeTarget) (map[string]*dto.MetricFamily, bool, string) {
	ctx, cancel := context.WithTimeout(ctx, perAgentTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", tgt.BaseURL+"/metrics", nil)
	if err != nil {
		return nil, false, "error"
	}
	if tgt.Token != "" {
		req.Header.Set("Authorization", "Bearer "+tgt.Token)
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
	parser := expfmt.NewTextParser(model.UTF8Validation)
	fams, err := parser.TextToMetricFamilies(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, false, "parse"
	}
	return fams, true, ""
}
