// Package agentkind maps an agent "kind" string to a builder that produces an
// agentruntime.Config. Keeps cmd/agentd thin and the mapping unit-testable.
package agentkind

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"

	hrt "github.com/sausheong/harness/runtime"
	"github.com/sausheong/harness/tool"
	hmemory "github.com/sausheong/harness/tool/memory"

	"github.com/sausheong/runtime/agentruntime"
	nutrition "github.com/sausheong/runtime/examples/nutrition-label-go"
	"github.com/sausheong/runtime/internal/memory"
	"github.com/sausheong/runtime/testagent"
)

// Deps carries everything any builder might need to describe its agent. It
// deliberately excludes operator concerns (the listen address, the Postgres
// DSN) — those are read from the environment by agentruntime.Serve, not handled
// by builders. DB is non-nil only when the caller opened Postgres (the test
// agent's marker tool needs it).
type Deps struct {
	AgentID string
	DB      *sql.DB
	Tenant  string // the agent's tenant; pins the memory store. "" ⇒ "default".
	Memory  bool   // when true, attach the per-tenant Postgres memory tool.
}

// Builder turns Deps into a serveable Config.
type Builder func(Deps) (agentruntime.Config, error)

var builders = map[string]Builder{
	"":          buildTestAgent, // default
	"testagent": buildTestAgent,
	"nutrition": buildNutrition,
}

// Get returns the builder for a kind, or false if the kind is unknown.
func Get(kind string) (Builder, bool) {
	b, ok := builders[kind]
	return b, ok
}

// wireMemory attaches the per-tenant memory tool to cfg.Tools when d.Memory is
// set, and — when embeddings are configured (RUNTIME_EMBED_*) — embeds entries on
// save and installs cfg.KGFn for semantic recall. Fail-fast: an agent that asked
// for memory must not start without it, and misconfigured embeddings are fatal.
func wireMemory(cfg *agentruntime.Config, d Deps) error {
	if !d.Memory {
		return nil
	}
	if d.DB == nil {
		return fmt.Errorf("agentkind: memory enabled for %q but no DB handle", d.AgentID)
	}
	emb, _, enabled, err := memory.NewEmbedderFromEnv()
	if err != nil {
		return fmt.Errorf("agentkind: embeddings config for %q: %w", d.AgentID, err)
	}
	var opts []memory.Option
	if enabled {
		opts = append(opts, memory.WithEmbedder(emb))
	}
	st, err := memory.NewStore(context.Background(), d.DB, d.Tenant, opts...)
	if err != nil {
		return fmt.Errorf("agentkind: memory store for %q: %w", d.AgentID, err)
	}
	cfg.Tools.Register(&hmemory.MemoryTool{Store: st})
	if enabled {
		k := envInt("RUNTIME_EMBED_RECALL_K", 5)
		floor := envFloat("RUNTIME_EMBED_RECALL_FLOOR", 0.7)
		kg := memory.NewKG(st, k, floor)
		cfg.KGFn = func(string) hrt.KnowledgeGraph { return kg }
	}
	return nil
}

// envInt reads an int env var with a default.
func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// envFloat reads a float64 env var with a default.
func envFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func buildNutrition(d Deps) (agentruntime.Config, error) {
	cfg, err := nutrition.BuildConfig(nutrition.Deps{AgentID: d.AgentID})
	if err != nil {
		return agentruntime.Config{}, err
	}
	if err := wireMemory(&cfg, d); err != nil {
		return agentruntime.Config{}, err
	}
	return cfg, nil
}

func buildTestAgent(d Deps) (agentruntime.Config, error) {
	reg := tool.NewRegistry()
	reg.Register(testagent.MarkerTool{DB: d.DB})
	cfg := agentruntime.Config{
		Spec: hrt.AgentSpec{
			ID: d.AgentID, Name: d.AgentID, Model: "test/scripted", MaxTurns: 10,
		},
		Provider: testagent.New(),
		Tools:    reg,
	}
	if err := wireMemory(&cfg, d); err != nil {
		return agentruntime.Config{}, err
	}
	return cfg, nil
}
