package agentruntime

import (
	"context"
	"errors"
	"time"

	"github.com/sausheong/harness/llm"
	hrt "github.com/sausheong/harness/runtime"
	"github.com/sausheong/harness/tool"

	"github.com/sausheong/runtime/internal/config"
	"github.com/sausheong/runtime/internal/eval"
)

// Config is the entire surface an agent author provides to Serve. It describes
// the agent itself — identity, model, behavior, and tools. Operator concerns
// (where Postgres lives, which address to bind) are NOT here: Serve reads those
// from the environment the control plane injects, so an agent builder never has
// to know or carry them.
type Config struct {
	Spec     hrt.AgentSpec                                           // harness agent spec (id, model, system prompt, ...)
	Provider llm.LLMProvider                                         // resolved LLM provider for Spec.Model
	Tools    *tool.Registry                                          // the agent's tool registry
	KGFn     func(model, sessionID, actor string) hrt.KnowledgeGraph // optional; nil ⇒ no semantic recall
	// Price is this agent's resolved per-model price, or nil when the model is
	// unpriced (tokens still metered, cost skipped). Set by agentd from
	// RUNTIME_AGENT_PRICING.
	Price *config.ModelPrice
	// StartMemoryGC, when non-nil, launches memory maintenance bound to ctx.
	// onGC receives dead-history deletes; onRetention receives live-memory
	// deletes by kind. Nil ⇒ maintenance disabled. Set by agentkind.wireMemory;
	// invoked by Serve after metrics are built.
	StartMemoryGC func(ctx context.Context, onGC func(int), onRetention func(string, int))
	// SetMemoryMetrics, when non-nil, wires the memory write metrics (summary,
	// episode) after AgentMetrics is built. Nil ⇒ metrics inert. Set by
	// agentkind.wireMemory; invoked by Serve.
	SetMemoryMetrics func(summary, episode func())
	// DrainMemory stops admission, drains accepted asynchronous ingestion, and
	// cancels overdue workers before the memory database closes.
	DrainMemory func(timeout time.Duration) error
	// EvalPolicy is this agent's standing online-scoring policy, or nil when no
	// policy is configured (nil ⇒ no scoring). Set by agentd from the eval
	// policy store.
	EvalPolicy *eval.Policy
	// EvalJudge grades judge-scorer criteria (LLM-as-judge), or nil when no judge
	// model is configured. A nil judge fails judge criteria closed (never aborts).
	EvalJudge eval.Judge
	// TranscriptFilter is an optional deployment-specific final filter applied
	// after built-in best-effort credential redaction and before persistence.
	TranscriptFilter func([]byte) ([]byte, error)
}

// Validate checks required fields.
func (c Config) Validate() error {
	if c.Spec.ID == "" || c.Spec.Model == "" {
		return errors.New("agentruntime: Spec.ID and Spec.Model are required")
	}
	return nil
}
