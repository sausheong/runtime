package agentruntime

import (
	"context"
	"testing"

	hrt "github.com/sausheong/harness/runtime"
)

// stubKG is a minimal KnowledgeGraph for wiring assertions.
type stubKG struct{}

func (stubKG) ShouldRecall(string) bool              { return false }
func (stubKG) Recall(context.Context, string) string { return "" }
func (stubKG) Ingest(context.Context, []hrt.Message) {}

func TestConfig_KGFnField(t *testing.T) {
	called := false
	cfg := Config{
		KGFn: func(model, sessionID, actor string) hrt.KnowledgeGraph {
			called = true
			return stubKG{}
		},
	}
	if cfg.KGFn == nil {
		t.Fatal("KGFn field must be settable")
	}
	if kg := cfg.KGFn("m", "s", "a"); kg == nil || !called {
		t.Fatal("KGFn must return the KG")
	}
}
