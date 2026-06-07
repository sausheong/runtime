package agentruntime

import (
	"errors"

	"github.com/sausheong/harness/llm"
	hrt "github.com/sausheong/harness/runtime"
	"github.com/sausheong/harness/tool"
)

// Config is the entire surface an agent author provides to Serve.
type Config struct {
	Spec        hrt.AgentSpec   // harness agent spec (id, model, system prompt, ...)
	Provider    llm.LLMProvider // resolved LLM provider for Spec.Model
	Tools       *tool.Registry  // the agent's tool registry
	ListenAddr  string          // HTTP bind address for the agent contract (e.g. ":8081")
	PostgresDSN string          // DBOS system database connection string
}

// Validate checks required fields.
func (c Config) Validate() error {
	if c.Spec.ID == "" || c.Spec.Model == "" {
		return errors.New("agentruntime: Spec.ID and Spec.Model are required")
	}
	if c.PostgresDSN == "" {
		return errors.New("agentruntime: PostgresDSN is required")
	}
	if c.ListenAddr == "" {
		return errors.New("agentruntime: ListenAddr is required")
	}
	return nil
}
