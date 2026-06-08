package nutrition

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/sausheong/harness/providers/openai"
	hrt "github.com/sausheong/harness/runtime"
	"github.com/sausheong/harness/tool"

	"github.com/sausheong/runtime/agentruntime"
)

// Deps is what the agentd runner hands the builder.
type Deps struct {
	AgentID     string
	ListenAddr  string
	PostgresDSN string
}

// BuildConfig assembles the agentruntime.Config for the SG Nutrition
// Investigator: the OpenAI provider pointed at the configured (LiteLLM) proxy,
// the four tools, and the investigator system prompt. Reads OPENAI_API_KEY,
// OPENAI_BASE_URL, OPENAI_MODEL from the environment. The memory file lives
// under RUNTIME_NUTRITION_DATA_DIR (default ".").
func BuildConfig(d Deps) (agentruntime.Config, error) {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		return agentruntime.Config{}, errors.New("nutrition: OPENAI_API_KEY is required")
	}
	baseURL := os.Getenv("OPENAI_BASE_URL")
	model := os.Getenv("OPENAI_MODEL")
	if model == "" {
		model = "gpt-4o"
	}

	dataDir := os.Getenv("RUNTIME_NUTRITION_DATA_DIR")
	if dataDir == "" {
		dataDir = "."
	}

	idx := newAdditiveIndex()
	mem := newMemory(filepath.Join(dataDir, "nutrition_memory.json"))
	tl := newTools(idx, mem, nil)

	reg := tool.NewRegistry()
	reg.Register(tl.checkAdditive())
	reg.Register(tl.recallProduct())
	reg.Register(tl.checkHCS())
	reg.Register(tl.nutriGrade())

	provider := openai.NewOpenAIProviderWithKind(key, baseURL, "openai-compatible")

	return agentruntime.Config{
		Spec: hrt.AgentSpec{
			ID:           d.AgentID,
			Name:         "SG Nutrition Investigator",
			Model:        "openai/" + model,
			SystemPrompt: investigatorPrompt,
			MaxTurns:     12,
		},
		Provider:    provider,
		Tools:       reg,
		ListenAddr:  d.ListenAddr,
		PostgresDSN: d.PostgresDSN,
	}, nil
}
