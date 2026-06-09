//go:build integration

package test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/sausheong/harness/llm"
	hrt "github.com/sausheong/harness/runtime"
	"github.com/sausheong/harness/session"
	"github.com/sausheong/harness/tool"

	"github.com/sausheong/runtime/internal/memory"
)

// captureProvider records the system-prompt parts of the last ChatStream request
// and emits a single assistant text message (no tool calls ⇒ the turn completes
// in one round).
type captureProvider struct {
	lastParts []llm.SystemPromptPart
	reply     string
}

func (captureProvider) Models() []llm.ModelInfo { return nil }
func (captureProvider) NormalizeToolSchema(t []llm.ToolDef) ([]llm.ToolDef, []llm.Diagnostic) {
	return t, nil
}
func (p *captureProvider) ChatStream(_ context.Context, req llm.ChatRequest) (<-chan llm.ChatEvent, error) {
	p.lastParts = req.SystemPromptParts
	ch := make(chan llm.ChatEvent, 2)
	ch <- llm.ChatEvent{Type: llm.EventTextDelta, Text: p.reply}
	ch <- llm.ChatEvent{Type: llm.EventDone}
	close(ch)
	return ch, nil
}

func buildKGRuntime(t *testing.T, prov llm.LLMProvider, kg hrt.KnowledgeGraph, sess *session.Session) *hrt.Runtime {
	t.Helper()
	rt, err := hrt.BuildRuntime(
		hrt.RuntimeDeps{KGFn: func(string) hrt.KnowledgeGraph { return kg }},
		hrt.RuntimeInputs{Provider: prov, Tools: tool.NewRegistry(), Session: sess},
		hrt.AgentSpec{ID: "a", Name: "a", Model: "test/scripted", MaxTurns: 10},
	)
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

func TestKGRunTurnE2E_IngestAndRecallOnServePath(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Skipf("postgres not reachable: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE IF EXISTS memory_events CASCADE`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DROP TABLE IF EXISTS memory_events CASCADE`) })

	const fact = "the user lives in Singapore"
	emb := ingestEmbedder{vecs: map[string][]float32{
		fact:                        {1, 0, 0},
		"where does the user live?": {1, 0, 0},
	}}
	st, err := memory.NewStore(ctx, db, "alpha", memory.WithEmbedder(emb))
	if err != nil {
		t.Fatal(err)
	}
	ext := fixedExtractor{facts: []string{fact}}
	kg := memory.NewKG(st, 5, 0.5, memory.WithIngest(ext, 0.85, 2, 4))

	// Exchange 1: drive a turn through RunTurn; ingest must fire on completion.
	prov := &captureProvider{reply: "noted"}
	sess1 := session.NewSession("a", "alpha")
	rt1 := buildKGRuntime(t, prov, kg, sess1)
	res, err := rt1.RunTurn(ctx, "I live in Singapore", nil, nil)
	if err != nil || !res.Done {
		t.Fatalf("turn 1: err=%v done=%v", err, res.Done)
	}
	waitForContent(t, st, fact) // ingest is async; poll until durable

	// Exchange 2: a fresh turn; recall must inject the fact into the prompt.
	prov2 := &captureProvider{reply: "ok"}
	sess2 := session.NewSession("a", "alpha")
	rt2 := buildKGRuntime(t, prov2, kg, sess2)
	if _, err := rt2.RunTurn(ctx, "where does the user live?", nil, nil); err != nil {
		t.Fatal(err)
	}
	var joined string
	for _, p := range prov2.lastParts {
		joined += p.Text + "\n"
	}
	if !strings.Contains(joined, "Singapore") {
		t.Fatalf("recall hint should reach the prompt on the serve path; parts=%q", joined)
	}
}
