// Package conformance verifies that an agent satisfies the runtime HTTP/SSE
// agent contract. It runs under `go test` (via *testing.T) and from the CLI
// (via a small adapter), exercising any agent reachable at a base URL.
//
// Optional endpoints (not checked here): an agent MAY expose GET /metrics
// serving Prometheus text-format exposition. The platform's fan-out scrape
// merges it into runtimed's /metrics; an agent without it (e.g. a foreign-SDK
// shim) is simply skipped (skip reason "no_metrics") — this is NOT an error
// and does not mark the agent down.
package conformance

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// TestingT is the minimal subset of *testing.T the suite needs, so the same
// checks run under `go test` and from the runtimectl CLI.
type TestingT interface {
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
	Logf(format string, args ...any)
}

// Run executes all contract checks against the agent at baseURL. Failures are
// reported via t.Errorf (non-fatal, so all checks run).
func Run(t TestingT, baseURL string) {
	baseURL = strings.TrimRight(baseURL, "/")
	client := &http.Client{Timeout: 15 * time.Second}

	checkHealthz(t, client, baseURL)
	checkMeta(t, client, baseURL)
	sid := checkCreateSession(t, client, baseURL)
	if sid != "" {
		checkStream(t, client, baseURL, sid)
		checkGetSession(t, client, baseURL, sid)
	}
	checkListSessions(t, client, baseURL)
}

func checkHealthz(t TestingT, c *http.Client, base string) {
	resp, err := c.Get(base + "/healthz")
	if err != nil {
		t.Errorf("healthz: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("healthz: status %d, want 200", resp.StatusCode)
	} else {
		t.Logf("healthz: ok")
	}
}

func checkMeta(t TestingT, c *http.Client, base string) {
	resp, err := c.Get(base + "/meta")
	if err != nil {
		t.Errorf("meta: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("meta: status %d, want 200", resp.StatusCode)
		return
	}
	var m map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Errorf("meta: decode: %v", err)
		return
	}
	if m["agent_id"] == "" {
		t.Errorf("meta: missing agent_id")
	}
	if m["contract_version"] == "" {
		t.Errorf("meta: missing contract_version")
	}
	if m["agent_id"] != "" && m["contract_version"] != "" {
		t.Logf("meta: ok (contract %s)", m["contract_version"])
	}
}

func checkCreateSession(t TestingT, c *http.Client, base string) string {
	resp, err := c.Post(base+"/sessions", "application/json", strings.NewReader(`{"message":"conformance ping"}`))
	if err != nil {
		t.Errorf("create session: %v", err)
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		t.Errorf("create session: status %d, want 2xx", resp.StatusCode)
		return ""
	}
	var out struct {
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Errorf("create session: decode: %v", err)
		return ""
	}
	if out.SessionID == "" {
		t.Errorf("create session: empty session_id")
		return ""
	}
	t.Logf("create session: ok (%s)", out.SessionID)
	return out.SessionID
}

func checkStream(t TestingT, c *http.Client, base, sid string) {
	resp, err := c.Get(base + "/sessions/" + sid + "/stream?since=0")
	if err != nil {
		t.Errorf("stream: %v", err)
		return
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("stream: content-type %q, want text/event-stream", ct)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if !strings.Contains(string(body), "id: ") {
		t.Errorf("stream: missing SSE id line")
	}
	if !strings.Contains(string(body), `"type":"done"`) {
		t.Errorf("stream: never saw terminal done event; got %q", string(body))
	} else {
		t.Logf("stream: ok")
	}
}

func checkGetSession(t TestingT, c *http.Client, base, sid string) {
	resp, err := c.Get(base + "/sessions/" + sid)
	if err != nil {
		t.Errorf("get session: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("get session: status %d, want 200", resp.StatusCode)
		return
	}
	var row struct {
		ID        string `json:"id"`
		Status    string `json:"status"`
		TurnCount *int   `json:"turn_count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&row); err != nil {
		t.Errorf("get session: decode: %v", err)
		return
	}
	if row.ID != sid {
		t.Errorf("get session: id %q, want %q", row.ID, sid)
	} else if !validStatus(row.Status) {
		t.Errorf("get session: invalid status %q", row.Status)
	} else if row.TurnCount == nil {
		t.Errorf("get session: missing turn_count")
	} else {
		t.Logf("get session: ok")
	}
}

func checkListSessions(t TestingT, c *http.Client, base string) {
	resp, err := c.Get(base + "/sessions")
	if err != nil {
		t.Errorf("list sessions: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("list sessions: status %d, want 200", resp.StatusCode)
		return
	}
	var rows []struct {
		ID        string `json:"id"`
		Status    string `json:"status"`
		TurnCount *int   `json:"turn_count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		t.Errorf("list sessions: decode: %v", err)
		return
	}
	for i, row := range rows {
		if row.ID == "" || !validStatus(row.Status) || row.TurnCount == nil {
			t.Errorf("list sessions: row %d missing or invalid id/status/turn_count", i)
		}
	}
	t.Logf("list sessions: ok (%d)", len(rows))
}

func validStatus(s string) bool {
	return s == "created" || s == "running" || s == "completed" || s == "error" || s == "limit_exceeded"
}
