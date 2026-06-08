// Package conformance verifies that an agent satisfies the runtime HTTP/SSE
// agent contract. It runs under `go test` (via *testing.T) and from the CLI
// (via a small adapter), exercising any agent reachable at a base URL.
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
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Errorf("get session: decode: %v", err)
		return
	}
	if _, ok := m["status"]; !ok {
		t.Errorf("get session: missing status")
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
	var rows []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		t.Errorf("list sessions: decode: %v", err)
		return
	}
	t.Logf("list sessions: ok (%d)", len(rows))
}
