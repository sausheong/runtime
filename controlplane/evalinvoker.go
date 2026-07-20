package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/sausheong/runtime/internal/eval"
)

// evalInvokeDefaultTimeout bounds one Invoke (submit + poll to a terminal
// event). Overridable via RUNTIME_EVAL_INVOKE_TIMEOUT (a Go duration).
const evalInvokeDefaultTimeout = 120 * time.Second

// evalPollInterval is the sleep between event polls when no terminal event has
// arrived yet.
const evalPollInterval = 200 * time.Millisecond

// evalInvoker drives one agent input to completion over the agent HTTP contract
// (POST /sessions, then poll GET /sessions/{id}/events). The registry lookup is
// behind the resolve seam so the HTTP drain is hermetically testable against an
// httptest server.
type evalInvoker struct {
	client  *http.Client
	timeout time.Duration
	// resolve maps an agentID to its dial base + bearer. The registry-backed
	// default picks a replica; a test supplies a fake pointing at httptest.
	resolve func(agentID string) (base, token string, ok bool)
}

// NewEvalInvoker returns an eval.Invoker that resolves an agent's replica from
// the registry (round-robin new-session pick) and drives it over HTTP.
func NewEvalInvoker(reg *Registry) eval.Invoker {
	return &evalInvoker{
		client:  &http.Client{},
		timeout: evalInvokeTimeoutFromEnv(),
		resolve: func(agentID string) (string, string, bool) {
			i := reg.NextReplica(agentID)
			ap, ok := reg.Replica(agentID, i)
			if !ok {
				return "", "", false
			}
			return ap.baseURL(), ap.AuthToken, true
		},
	}
}

func evalInvokeTimeoutFromEnv() time.Duration {
	if v := strings.TrimSpace(os.Getenv("RUNTIME_EVAL_INVOKE_TIMEOUT")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return evalInvokeDefaultTimeout
}

// Invoke submits input to the agent and polls its event stream until a terminal
// (done|error) event, returning the concatenated text output. Bounded by the
// invoker timeout (derived as a child ctx at the top) and the caller's ctx.
func (e *evalInvoker) Invoke(ctx context.Context, agentID, input string) (string, error) {
	base, token, ok := e.resolve(agentID)
	if !ok {
		return "", fmt.Errorf("no replica for agent %s", agentID)
	}
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	sid, err := e.startSession(ctx, base, token, input)
	if err != nil {
		return "", err
	}
	return e.pollEvents(ctx, base, token, sid)
}

func (e *evalInvoker) startSession(ctx context.Context, base, token, input string) (string, error) {
	body, _ := json.Marshal(map[string]string{"message": input})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/sessions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("eval invoke: create session non-200: %d %s", resp.StatusCode, strings.TrimSpace(string(rb)))
	}
	var sr struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(rb, &sr); err != nil || sr.SessionID == "" {
		return "", fmt.Errorf("eval invoke: unparseable session response")
	}
	return sr.SessionID, nil
}

type evalEvent struct {
	Seq  int64  `json:"seq"`
	Type string `json:"type"`
	Text string `json:"text"`
	Err  string `json:"error"`
}

func (e *evalInvoker) pollEvents(ctx context.Context, base, token, sid string) (string, error) {
	var out strings.Builder
	var since int64
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		evs, err := e.fetchEvents(ctx, base, token, sid, since)
		if err != nil {
			return "", err
		}
		terminal := false
		for _, ev := range evs {
			if ev.Seq > since {
				since = ev.Seq
			}
			switch ev.Type {
			case "text":
				out.WriteString(ev.Text)
			case "error":
				return "", errors.New(ev.Err)
			case "done":
				terminal = true
			}
		}
		if terminal {
			return out.String(), nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(evalPollInterval):
		}
	}
}

func (e *evalInvoker) fetchEvents(ctx context.Context, base, token, sid string, since int64) ([]evalEvent, error) {
	url := base + "/sessions/" + sid + "/events?since=" + strconv.FormatInt(since, 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("eval invoke: events non-200: %d %s", resp.StatusCode, strings.TrimSpace(string(rb)))
	}
	var evs []evalEvent
	if err := json.Unmarshal(rb, &evs); err != nil {
		return nil, fmt.Errorf("eval invoke: unparseable events response: %w", err)
	}
	return evs, nil
}
