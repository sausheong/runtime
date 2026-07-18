package agentruntime

import (
	"errors"

	"github.com/sausheong/harness/llm"
	hrt "github.com/sausheong/harness/runtime"
	"github.com/sausheong/harness/tool"
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
	KGFn     func(model string) hrt.KnowledgeGraph // optional; nil ⇒ no semantic recall
}

// Validate checks required fields.
func (c Config) Validate() error {
	if c.Spec.ID == "" || c.Spec.Model == "" {
		return errors.New("agentruntime: Spec.ID and Spec.Model are required")
	}
	return nil
}
