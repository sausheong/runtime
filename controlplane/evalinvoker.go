package controlplane

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/sausheong/runtime/internal/eval"
)

// evalInvokeDefaultTimeout bounds one Invoke (submit + poll to a terminal
// event). Overridable via RUNTIME_EVAL_INVOKE_TIMEOUT (a Go duration).
const evalInvokeDefaultTimeout = 120 * time.Second

// evalInvoker drives one agent input to completion over the common agent HTTP
// contract (POST /sessions, then GET /sessions/{id}/stream). The registry lookup
// is behind the resolve seam so the HTTP drain is hermetically testable.
type evalInvoker struct {
	timeout time.Duration
	// resolve returns the complete process descriptor so outbound policy can never
	// be dropped by reducing it to URL/token strings.
	resolve func(agentID string) (AgentProcess, bool)
}

// NewEvalInvoker returns an eval.Invoker that resolves an agent's replica from
// the registry (round-robin new-session pick) and drives it over HTTP.
func NewEvalInvoker(reg *Registry) eval.Invoker {
	return &evalInvoker{
		timeout: evalInvokeTimeoutFromEnv(),
		resolve: func(agentID string) (AgentProcess, bool) {
			i := reg.NextReplica(agentID)
			ap, ok := reg.Replica(agentID, i)
			if !ok {
				return AgentProcess{}, false
			}
			return ap, true
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
	ap, ok := e.resolve(agentID)
	if !ok {
		return "", fmt.Errorf("no replica for agent %s", agentID)
	}
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	client := NewAgentHTTPClient(ap, 0)

	sid, err := e.startSession(ctx, client, ap.baseURL(), input)
	if err != nil {
		return "", err
	}
	return e.readStream(ctx, client, ap.baseURL(), sid)
}

func (e *evalInvoker) startSession(ctx context.Context, client *http.Client, base, input string) (string, error) {
	body, _ := json.Marshal(map[string]string{"message": input})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/sessions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
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

// readStream consumes the SSE endpoint required by the common contract. Using
// SSE rather than the native-Go-only /events extension keeps golden-set
// evaluation compatible with foreign shims.
func (e *evalInvoker) readStream(ctx context.Context, client *http.Client, base, sid string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/sessions/"+sid+"/stream?since=0", nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return "", fmt.Errorf("eval invoke: stream non-200: %d %s", resp.StatusCode, strings.TrimSpace(string(rb)))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		return "", fmt.Errorf("eval invoke: stream content type %q", ct)
	}

	var out strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	total := 0
	for scanner.Scan() {
		line := scanner.Text()
		total += len(line)
		if total > 16<<20 {
			return "", fmt.Errorf("eval invoke: stream exceeded 16 MiB")
		}
		raw, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		var ev evalEvent
		if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &ev); err != nil {
			return "", fmt.Errorf("eval invoke: malformed stream event: %w", err)
		}
		switch ev.Type {
		case "text":
			out.WriteString(ev.Text)
		case "error":
			return "", errors.New(ev.Err)
		case "done":
			return out.String(), nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("eval invoke: stream ended before a terminal event")
}
