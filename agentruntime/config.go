package agentruntime

import (
	"context"
	"errors"

	"github.com/sausheong/harness/llm"
	hrt "github.com/sausheong/harness/runtime"
	"github.com/sausheong/harness/tool"

	"github.com/sausheong/runtime/internal/config"
)

// Config is the entire surface an agent author provides to Serve. It describes
// the agent itself — identity, model, behavior, and tools. Operator concerns
// (where Postgres lives, which address to bind) are NOT here: Serve reads those
// from the environment the control plane injects, so an agent builder never has
// to know or carry them.
type Config struct {
	Spec     hrt.AgentSpec                         // harness agent spec (id, model, system prompt, ...)
	Provider llm.LLMProvider                       // resolved LLM provider for Spec.Model
	Tools    *tool.Registry                        // the agent's tool registry
	KGFn     func(model, sessionID, actor string) hrt.KnowledgeGraph // optional; nil ⇒ no semantic recall
	// Price is this agent's resolved per-model price, or nil when the model is
	// unpriced (tokens still metered, cost skipped). Set by agentd from
	// RUNTIME_AGENT_PRICING.
	Price *config.ModelPrice
	// StartMemoryGC, when non-nil, launches the memory GC reaper bound to ctx.
	// onReap receives each sweep's delete count (for metrics). Nil ⇒ GC disabled.
	// Set by agentkind.wireMemory; invoked by Serve after metrics are built.
	StartMemoryGC func(ctx context.Context, onReap func(int))
	// SetMemoryMetrics, when non-nil, wires the memory write metrics (summary,
	// episode) after AgentMetrics is built. Nil ⇒ metrics inert. Set by
	// agentkind.wireMemory; invoked by Serve.
	SetMemoryMetrics func(summary, episode func())
}

// Validate checks required fields.
func (c Config) Validate() error {
	if c.Spec.ID == "" || c.Spec.Model == "" {
		return errors.New("agentruntime: Spec.ID and Spec.Model are required")
	}
	return nil
}
