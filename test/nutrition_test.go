//go:build integration

package test

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/sausheong/harness/llm"
	hrt "github.com/sausheong/harness/runtime"
	"github.com/sausheong/harness/tool"

	"github.com/sausheong/runtime/agentruntime"
)

// scripted is a network-free provider: first turn → one tool call; then → text.
type scripted struct{}

func (scripted) ChatStream(ctx context.Context, req llm.ChatRequest) (<-chan llm.ChatEvent, error) {
	ch := make(chan llm.ChatEvent, 4)
	hasResult := false
	for _, m := range req.Messages {
		if m.ToolCallID != "" {
			hasResult = true
		}
	}
	go func() {
		defer close(ch)
		if hasResult {
			ch <- llm.ChatEvent{Type: llm.EventTextDelta, Text: "VERDICT: GREEN ok"}
			ch <- llm.ChatEvent{Type: llm.EventDone, Usage: &llm.Usage{InputTokens: 1, OutputTokens: 1}}
			return
		}
		tc := &llm.ToolCall{ID: "c1", Name: "recall_product", Input: []byte(`{"product_name":"Test"}`)}
		ch <- llm.ChatEvent{Type: llm.EventToolCallStart, ToolCall: tc}
		ch <- llm.ChatEvent{Type: llm.EventToolCallDone, ToolCall: tc}
		ch <- llm.ChatEvent{Type: llm.EventDone, Usage: &llm.Usage{InputTokens: 1, OutputTokens: 1}}
	}()
	return ch, nil
}
func (scripted) Models() []llm.ModelInfo { return []llm.ModelInfo{{ID: "scripted"}} }
func (scripted) NormalizeToolSchema(t []llm.ToolDef) ([]llm.ToolDef, []llm.Diagnostic) { return t, nil }

// recallTool is a tiny stand-in matching the tool the scripted provider calls.
type recallTool struct{}

func (recallTool) Name() string       { return "recall_product" }
func (recallTool) Description() string { return "recall" }
func (recallTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"product_name":{"type":"string"}}}`)
}
func (recallTool) IsConcurrencySafe(json.RawMessage) bool { return true }
func (recallTool) Execute(context.Context, json.RawMessage) (tool.ToolResult, error) {
	return tool.ToolResult{Output: "first investigation"}, nil
}

func TestNutritionAgentImageSession(t *testing.T) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatalf("ping postgres (is it running at %s?): %v", dsn, err)
	}

	// Clean slate.
	mustExec(t, db, `DROP TABLE IF EXISTS session_events, sessions, agents CASCADE`)
	mustExec(t, db, `DROP SCHEMA IF EXISTS dbos CASCADE`)

	reg := tool.NewRegistry()
	reg.Register(recallTool{})

	addr := "127.0.0.1:8211"
	cfg := agentruntime.Config{
		Spec:        hrt.AgentSpec{ID: "nutrition", Name: "Nutrition", Model: "openai/scripted", MaxTurns: 5},
		Provider:    scripted{},
		Tools:       reg,
		ListenAddr:  addr,
		PostgresDSN: dsn,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = agentruntime.Serve(ctx, cfg) }()

	base := "http://" + addr
	waitURL(t, base+"/healthz", 20*time.Second)

	// POST a session WITH an image, then stream to done.
	img := base64.StdEncoding.EncodeToString([]byte("\xff\xd8\xff\xe0fake-jpeg"))
	body, _ := json.Marshal(map[string]string{
		"message": "Investigate this label.", "image_b64": img, "image_mime": "image/jpeg",
	})
	resp, err := http.Post(base+"/sessions", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		SessionID string `json:"session_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if out.SessionID == "" {
		t.Fatal("no session id")
	}

	final := streamURL(t, base+"/sessions/"+out.SessionID+"/stream?since=0", 30*time.Second)
	if !strings.Contains(final, `"type":"done"`) {
		t.Fatalf("session did not complete:\n%s", final)
	}
	if !strings.Contains(final, "GREEN") {
		t.Fatalf("expected verdict text in stream:\n%s", final)
	}
}
