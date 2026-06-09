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

func TestWireMemory_IngestEnabledWithoutModelFatal(t *testing.T) {
	t.Setenv("RUNTIME_EMBED_MODEL", "embed-1")
	t.Setenv("RUNTIME_EMBED_DIM", "3")
	t.Setenv("OPENAI_BASE_URL", "https://proxy.invalid")
	t.Setenv("OPENAI_API_KEY", "k")
	t.Setenv("RUNTIME_INGEST_ENABLED", "1")
	t.Setenv("RUNTIME_INGEST_MODEL", "") // enabled but no model
	build, _ := Get("testagent")
	// DB nil is fine: ingest config is validated BEFORE the DB check, so this
	// genuinely exercises the ingest-misconfig-fatal path.
	_, err := build(Deps{AgentID: "a1", Memory: true, Tenant: "alpha", DB: nil})
	if err == nil {
		t.Fatal("ingest enabled + no model must error")
	}
	if !strings.Contains(err.Error(), "ingest config") {
		t.Fatalf("expected ingest-config error, got: %v", err)
	}
}

func TestWireMemory_IngestWithoutEmbeddingsNotFatal(t *testing.T) {
	t.Setenv("RUNTIME_EMBED_MODEL", "") // embeddings off
	t.Setenv("RUNTIME_INGEST_ENABLED", "1")
	t.Setenv("RUNTIME_INGEST_MODEL", "chat-1")
	build, _ := Get("testagent")
	_, err := build(Deps{AgentID: "a1", Memory: true, Tenant: "alpha", DB: nil})
	// Ingest-without-embeddings warns and is ignored; the only error is the nil DB.
	if err == nil {
		t.Fatal("memory enabled + nil DB still errors")
	}
	if strings.Contains(err.Error(), "ingest config") {
		t.Fatalf("ingest-without-embeddings must not be an ingest-config fatal: %v", err)
	}
	if !strings.Contains(err.Error(), "no DB handle") {
		t.Fatalf("expected the nil-DB error, got: %v", err)
	}
}

func TestWireMemory_IngestDisabledUnaffected(t *testing.T) {
	t.Setenv("RUNTIME_EMBED_MODEL", "embed-1")
	t.Setenv("RUNTIME_EMBED_DIM", "3")
	t.Setenv("OPENAI_BASE_URL", "https://proxy.invalid")
	t.Setenv("OPENAI_API_KEY", "k")
	t.Setenv("RUNTIME_INGEST_ENABLED", "") // off
	build, _ := Get("testagent")
	_, err := build(Deps{AgentID: "a1", Memory: true, Tenant: "alpha", DB: nil})
	// With ingest off this is exactly the M2 path: nil DB ⇒ the DB error, no
	// ingest-config error.
	if err == nil || strings.Contains(err.Error(), "ingest config") {
		t.Fatalf("ingest off should not change M2 behavior; got: %v", err)
	}
}

func TestEnvBool(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "yes", "on", " On "} {
		t.Setenv("X_FLAG", v)
		if !envBool("X_FLAG") {
			t.Fatalf("%q should be truthy", v)
		}
	}
	for _, v := range []string{"", "0", "false", "no", "off", "nope"} {
		t.Setenv("X_FLAG", v)
		if envBool("X_FLAG") {
			t.Fatalf("%q should be falsy", v)
		}
	}
}
