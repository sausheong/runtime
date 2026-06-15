package console

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/sausheong/runtime/controlplane"
)

// sessionRow mirrors one element of the agent runtime's GET /sessions response.
type sessionRow struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	TurnCount int    `json:"turn_count"`
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
