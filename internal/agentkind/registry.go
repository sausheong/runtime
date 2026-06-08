// Package agentkind maps an agent "kind" string to a builder that produces an
// agentruntime.Config. Keeps cmd/agentd thin and the mapping unit-testable.
package agentkind

import (
	"database/sql"

	hrt "github.com/sausheong/harness/runtime"
	"github.com/sausheong/harness/tool"

	"github.com/sausheong/runtime/agentruntime"
	nutrition "github.com/sausheong/runtime/examples/nutrition-label-go"
	"github.com/sausheong/runtime/testagent"
)

// Deps carries everything any builder might need. DB is non-nil only when the
// caller opened Postgres (the test agent's marker tool needs it).
type Deps struct {
	AgentID     string
	ListenAddr  string
	PostgresDSN string
	DB          *sql.DB
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

func buildNutrition(d Deps) (agentruntime.Config, error) {
	return nutrition.BuildConfig(nutrition.Deps{
		AgentID: d.AgentID, ListenAddr: d.ListenAddr, PostgresDSN: d.PostgresDSN,
	})
}

func buildTestAgent(d Deps) (agentruntime.Config, error) {
	reg := tool.NewRegistry()
	reg.Register(testagent.MarkerTool{DB: d.DB})
	return agentruntime.Config{
		Spec: hrt.AgentSpec{
			ID: d.AgentID, Name: d.AgentID, Model: "test/scripted", MaxTurns: 10,
		},
		Provider:    testagent.New(),
		Tools:       reg,
		ListenAddr:  d.ListenAddr,
		PostgresDSN: d.PostgresDSN,
	}, nil
}
