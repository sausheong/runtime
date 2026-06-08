package nutrition

import (
	"os"
	"testing"
)

func TestBuildConfigRequiresKey(t *testing.T) {
	os.Unsetenv("OPENAI_API_KEY")
	if _, err := BuildConfig(Deps{AgentID: "nutrition"}); err == nil {
		t.Fatal("expected error when OPENAI_API_KEY unset")
	}
}

func TestBuildConfigOK(t *testing.T) {
	os.Setenv("OPENAI_API_KEY", "test-key")
	os.Setenv("OPENAI_BASE_URL", "https://example.invalid")
	os.Setenv("OPENAI_MODEL", "gpt-5.4")
	defer func() { os.Unsetenv("OPENAI_API_KEY"); os.Unsetenv("OPENAI_BASE_URL"); os.Unsetenv("OPENAI_MODEL") }()
	cfg, err := BuildConfig(Deps{AgentID: "nutrition"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Spec.ID != "nutrition" || cfg.Provider == nil || cfg.Tools == nil {
		t.Fatalf("bad config: %+v", cfg)
	}
	if cfg.Spec.SystemPrompt == "" {
		t.Error("missing system prompt")
	}
	if len(cfg.Tools.Names()) != 4 {
		t.Errorf("want 4 tools, got %v", cfg.Tools.Names())
	}
}
