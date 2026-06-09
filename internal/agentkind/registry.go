// Package agentkind maps an agent "kind" string to a builder that produces an
// agentruntime.Config. Keeps cmd/agentd thin and the mapping unit-testable.
package agentkind

import (
	"context"
	"database/sql"
	"fmt"

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

// attachMemory adds the per-tenant Postgres memory tool to reg when d.Memory is
// set. Requires d.DB. Returns an error if the store cannot be constructed — an
// agent that asked for memory must not start without it (fail fast).
func attachMemory(reg *tool.Registry, d Deps) error {
	if !d.Memory {
		return nil
	}
	if d.DB == nil {
		return fmt.Errorf("agentkind: memory enabled for %q but no DB handle", d.AgentID)
	}
	st, err := memory.NewStore(context.Background(), d.DB, d.Tenant)
	if err != nil {
		return fmt.Errorf("agentkind: memory store for %q: %w", d.AgentID, err)
	}
	reg.Register(&hmemory.MemoryTool{Store: st})
	return nil
}

func buildNutrition(d Deps) (agentruntime.Config, error) {
	cfg, err := nutrition.BuildConfig(nutrition.Deps{AgentID: d.AgentID})
	if err != nil {
		return agentruntime.Config{}, err
	}
	if err := attachMemory(cfg.Tools, d); err != nil {
		return agentruntime.Config{}, err
	}
	return cfg, nil
}

func buildTestAgent(d Deps) (agentruntime.Config, error) {
	reg := tool.NewRegistry()
	reg.Register(testagent.MarkerTool{DB: d.DB})
	if err := attachMemory(reg, d); err != nil {
		return agentruntime.Config{}, err
	}
	return agentruntime.Config{
		Spec: hrt.AgentSpec{
			ID: d.AgentID, Name: d.AgentID, Model: "test/scripted", MaxTurns: 10,
		},
		Provider: testagent.New(),
		Tools:    reg,
	}, nil
}
