package console

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"

	"github.com/sausheong/runtime/controlplane"
)

// sessionRow mirrors one element of the agent runtime's GET /sessions response.
type sessionRow struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	TurnCount   int    `json:"turn_count"`
	CompletedAt string `json:"completed_at"` // RFC3339 UTC, empty if still running
	DurationMs  *int   `json:"duration_ms"`  // nil if still running
}

// eventRow mirrors one element of the agent runtime's GET /sessions/{id}/events
// response.
type eventRow struct {
	Seq  int64  `json:"seq"`
	Type string `json:"type"`
	Text string `json:"text"`
	Err  string `json:"error"`
}

// agentClient reads a single agent replica's own HTTP API. The console depends
// on this seam so tests can inject a fake (no network). httpAgentClient is the
// production implementation.
type agentClient interface {
	ListSessions(ctx context.Context, ap controlplane.AgentProcess) ([]sessionRow, error)
	ListEvents(ctx context.Context, ap controlplane.AgentProcess, sid string, limit int) ([]eventRow, error)
	Metrics(ctx context.Context, ap controlplane.AgentProcess) (AgentMetrics, error)
}

// ToolCount is one tool's lifetime invocation count.
type ToolCount struct {
	Name  string
	Count int64
}

// AgentMetrics is the parsed lifetime telemetry from an agent's /metrics
// (cumulative counters since the agent process started). Zero value is a valid
// "nothing recorded yet" snapshot.
type AgentMetrics struct {
	TokensIn, TokensOut      int64
	CacheCreation, CacheRead int64
	Tools                    []ToolCount      // sorted desc by Count
	TurnsByOutcome           map[string]int64 // e.g. {"completed":3,"error":1}
	TurnCount                int64            // agent_turn_duration_seconds_count
	TurnSumSeconds           float64          // agent_turn_duration_seconds_sum
}

// TurnsCompleted / TurnsError are template conveniences (maps are awkward in
// html/template index syntax).
func (m AgentMetrics) TurnsCompleted() int64 { return m.TurnsByOutcome["completed"] }
func (m AgentMetrics) TurnsError() int64     { return m.TurnsByOutcome["error"] }

// AvgTurnSeconds is sum/count, 0 when no turns have completed.
func (m AgentMetrics) AvgTurnSeconds() float64 {
	if m.TurnCount == 0 {
		return 0
	}
	return m.TurnSumSeconds / float64(m.TurnCount)
}

// httpAgentClient calls an agent replica over HTTP, mirroring httpProbe's
// baseURL + bearer + 1s-timeout pattern.
type httpAgentClient struct{}

func (httpAgentClient) get(ctx context.Context, ap controlplane.AgentProcess, path string, out any) error {
	client := &http.Client{Timeout: 1 * time.Second}
	req, err := http.NewRequestWithContext(ctx, "GET", ap.DialBase()+path, nil)
	if err != nil {
		return err
	}
	if ap.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+ap.AuthToken)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("agent GET %s: status %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c httpAgentClient) ListSessions(ctx context.Context, ap controlplane.AgentProcess) ([]sessionRow, error) {
	var rows []sessionRow
	if err := c.get(ctx, ap, "/sessions", &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

func (c httpAgentClient) ListEvents(ctx context.Context, ap controlplane.AgentProcess, sid string, limit int) ([]eventRow, error) {
	var rows []eventRow
	path := "/sessions/" + sid + "/events?limit=" + strconv.Itoa(limit)
	if err := c.get(ctx, ap, path, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// Metrics fetches and parses the agent's Prometheus /metrics exposition into the
// lifetime telemetry snapshot. Mirrors get()'s bearer + 1s-timeout pattern but
// reads text (not JSON). A non-200 or parse error returns a zero snapshot + the
// error; callers render an empty card rather than failing the page.
func (httpAgentClient) Metrics(ctx context.Context, ap controlplane.AgentProcess) (AgentMetrics, error) {
	client := &http.Client{Timeout: 1 * time.Second}
	req, err := http.NewRequestWithContext(ctx, "GET", ap.DialBase()+"/metrics", nil)
	if err != nil {
		return AgentMetrics{}, err
	}
	if ap.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+ap.AuthToken)
	}
	resp, err := client.Do(req)
	if err != nil {
		return AgentMetrics{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return AgentMetrics{}, fmt.Errorf("agent GET /metrics: status %d", resp.StatusCode)
	}
	return parseAgentMetrics(io.LimitReader(resp.Body, 4<<20))
}

// parseAgentMetrics extracts the agent_* lifetime families from a Prometheus text
// exposition. Unknown/absent families yield zero values (a body with none yields
// a zeroed struct, not an error), so an agent that serves /metrics without our
// families still renders a clean empty card.
func parseAgentMetrics(r io.Reader) (AgentMetrics, error) {
	m := AgentMetrics{TurnsByOutcome: map[string]int64{}}
	parser := expfmt.NewTextParser(model.UTF8Validation)
	fams, err := parser.TextToMetricFamilies(r)
	if err != nil {
		return AgentMetrics{}, err
	}
	label := func(metric *dto.Metric, name string) string {
		for _, lp := range metric.Label {
			if lp.GetName() == name {
				return lp.GetValue()
			}
		}
		return ""
	}
	if f := fams["agent_tokens_total"]; f != nil {
		for _, metric := range f.Metric {
			v := int64(metric.GetCounter().GetValue())
			switch label(metric, "direction") {
			case "input":
				m.TokensIn += v
			case "output":
				m.TokensOut += v
			case "cache_creation":
				m.CacheCreation += v
			case "cache_read":
				m.CacheRead += v
			}
		}
	}
	if f := fams["agent_tool_calls_total"]; f != nil {
		for _, metric := range f.Metric {
			if tool := label(metric, "tool"); tool != "" {
				m.Tools = append(m.Tools, ToolCount{Name: tool, Count: int64(metric.GetCounter().GetValue())})
			}
		}
		sort.Slice(m.Tools, func(i, j int) bool {
			if m.Tools[i].Count != m.Tools[j].Count {
				return m.Tools[i].Count > m.Tools[j].Count
			}
			return m.Tools[i].Name < m.Tools[j].Name // stable tiebreak
		})
	}
	if f := fams["agent_turns_total"]; f != nil {
		for _, metric := range f.Metric {
			if o := label(metric, "outcome"); o != "" {
				m.TurnsByOutcome[o] += int64(metric.GetCounter().GetValue())
			}
		}
	}
	// Histogram: _count and _sum are summed across any series of the family.
	if f := fams["agent_turn_duration_seconds"]; f != nil {
		for _, metric := range f.Metric {
			if h := metric.GetHistogram(); h != nil {
				m.TurnCount += int64(h.GetSampleCount())
				m.TurnSumSeconds += h.GetSampleSum()
			}
		}
	}
	return m, nil
}
