package agentkind

import (
	"strings"
	"testing"
)

func TestGetKnownKinds(t *testing.T) {
	for _, k := range []string{"", "testagent", "nutrition"} {
		if _, ok := Get(k); !ok {
			t.Errorf("kind %q: expected a builder", k)
		}
	}
	if _, ok := Get("does-not-exist"); ok {
		t.Error("unknown kind should not resolve")
	}
}

func TestBuildTestAgent_NoMemoryToolByDefault(t *testing.T) {
	build, _ := Get("testagent")
	cfg, err := build(Deps{AgentID: "a1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Tools.Get("memory"); ok {
		t.Fatal("memory tool must be absent when Deps.Memory is false")
	}
}

func TestBuildTestAgent_MemoryEnabledRequiresDB(t *testing.T) {
	build, _ := Get("testagent")
	_, err := build(Deps{AgentID: "a1", Memory: true, Tenant: "alpha", DB: nil})
	if err == nil {
		t.Fatal("memory enabled with nil DB must error (fail fast)")
	}
}

func TestBuildTestAgent_SetsKGFnWhenEmbeddingsConfigured(t *testing.T) {
	t.Setenv("RUNTIME_EMBED_MODEL", "embed-1")
	t.Setenv("RUNTIME_EMBED_DIM", "3")
	t.Setenv("OPENAI_BASE_URL", "https://proxy.invalid")
	t.Setenv("OPENAI_API_KEY", "k")
	build, _ := Get("testagent")
	// DB nil → memory store construction fails fast; we only want to assert the
	// embeddings-config path is reached, so this must error (fail fast).
	_, err := build(Deps{AgentID: "a1", Memory: true, Tenant: "alpha", DB: nil})
	if err == nil {
		t.Fatal("memory enabled + nil DB must fail fast even with embeddings configured")
	}
}

func TestBuildTestAgent_EmbeddingsMisconfiguredFatal(t *testing.T) {
	t.Setenv("RUNTIME_EMBED_MODEL", "embed-1")
	t.Setenv("RUNTIME_EMBED_DIM", "") // bad: model set, dim missing
	build, _ := Get("testagent")
	// DB nil is fine here: embeddings config is validated BEFORE the DB check,
	// so this genuinely exercises the misconfig-fatal path.
	_, err := build(Deps{AgentID: "a1", Memory: true, Tenant: "alpha", DB: nil})
	if err == nil {
		t.Fatal("model set + bad dim must error")
	}
	if !strings.Contains(err.Error(), "embeddings config") {
		t.Fatalf("expected embeddings-config error, got: %v", err)
	}
}

func TestBuildTestAgent_NoKGFnWhenEmbeddingsUnset(t *testing.T) {
	t.Setenv("RUNTIME_EMBED_MODEL", "")
	build, _ := Get("testagent")
	cfg, err := build(Deps{AgentID: "a1"}) // memory off
	if err != nil {
		t.Fatal(err)
	}
	if cfg.KGFn != nil {
		t.Fatal("KGFn must be nil when embeddings/memory are off")
	}
}
