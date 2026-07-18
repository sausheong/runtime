// Package agentkind maps an agent "kind" string to a builder that produces an
// agentruntime.Config. Keeps cmd/agentd thin and the mapping unit-testable.
package agentkind

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	hrt "github.com/sausheong/harness/runtime"
	"github.com/sausheong/harness/tool"
	hmemory "github.com/sausheong/harness/tool/memory"
	hmcp "github.com/sausheong/harness/tools/mcp"

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

	GatewayURL string // when set, append the platform gateway as an MCP server on the spec.
	GatewayKey string // optional Bearer key for the gateway ("" in open mode).
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
// save and installs cfg.KGFn for semantic recall. When RUNTIME_INGEST_ENABLED is
// truthy (and recall is on), it also installs auto-ingestion. Fail-fast: an agent
// that asked for memory must not start without it; misconfigured embeddings or a
// requested-but-modelless ingest are fatal.
func wireMemory(cfg *agentruntime.Config, d Deps) error {
	if !d.Memory {
		return nil
	}
	emb, _, enabled, err := memory.NewEmbedderFromEnv()
	if err != nil {
		return fmt.Errorf("agentkind: embeddings config for %q: %w", d.AgentID, err)
	}

	// Resolve auto-ingestion config BEFORE the DB check so a misconfiguration is
	// fatal regardless of DB state (mirrors the embeddings-config ordering).
	var ingestExt memory.Extractor
	if envBool("RUNTIME_INGEST_ENABLED") {
		if !enabled {
			slog.Warn("agentkind: RUNTIME_INGEST_ENABLED set but embeddings are not configured; ingestion disabled", "agent", d.AgentID)
		} else {
			ext, ingEnabled := memory.NewExtractorFromEnv()
			if !ingEnabled {
				return fmt.Errorf("agentkind: ingest config for %q: RUNTIME_INGEST_ENABLED set but RUNTIME_INGEST_MODEL is empty", d.AgentID)
			}
			ingestExt = ext
		}
	}

	if d.DB == nil {
		return fmt.Errorf("agentkind: memory enabled for %q but no DB handle", d.AgentID)
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
		// Default tuned for OpenAI-family embeddings (text-embedding-3-*), where a
		// question scores only ~0.25-0.40 cosine against the declarative memory it
		// should recall (measured ~0.27 against text-embedding-3-small; unrelated
		// text sits near 0). A higher floor like 0.7 silently suppresses ALL recall
		// on those models. Operators on normalized-embedding families may raise it;
		// see the README per-model guidance.
		floor := envFloat("RUNTIME_EMBED_RECALL_FLOOR", 0.25)
		var kgOpts []memory.KGOption
		if ingestExt != nil {
			dedupFloor := envFloat("RUNTIME_INGEST_DEDUP_FLOOR", 0.85)
			minMsgs := envInt("RUNTIME_INGEST_MIN_MESSAGES", 2)
			maxInflight := envInt("RUNTIME_INGEST_MAX_INFLIGHT", 4)
			kgOpts = append(kgOpts, memory.WithIngest(ingestExt, dedupFloor, minMsgs, maxInflight))
		}
		kg := memory.NewKG(st, k, floor, kgOpts...)
		cfg.KGFn = func(string) hrt.KnowledgeGraph { return kg }
	}
	return nil
}

// wireGateway appends the platform MCP gateway to the agent's spec when the
// control plane injected RUNTIME_GATEWAY_URL. BuildRuntime then connects to it
// like any other MCP server; tools surface as mcp__gateway__<server>__<tool>.
func wireGateway(cfg *agentruntime.Config, d Deps) {
	if d.GatewayURL == "" {
		return
	}
	s := hmcp.ServerConfig{Name: "gateway", URL: d.GatewayURL}
	if d.GatewayKey != "" {
		s.Headers = map[string]string{"Authorization": "Bearer " + d.GatewayKey}
	}
	cfg.Spec.MCPServers = append(cfg.Spec.MCPServers, s)
}

// envBool reports whether key is set to a truthy value (1/true/yes/on,
// case-insensitive, surrounding spaces ignored).
func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// envInt reads an int env var with a default, warning (and using the default)
// on a malformed value.
func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		slog.Warn("agentkind: ignoring malformed env int; using default", "key", key, "value", v, "default", def)
		return def
	}
	return n
}

// envFloat reads a float64 env var with a default, warning (and using the
// default) on a malformed value.
func envFloat(key string, def float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		slog.Warn("agentkind: ignoring malformed env float; using default", "key", key, "value", v, "default", def)
		return def
	}
	return f
}

func buildNutrition(d Deps) (agentruntime.Config, error) {
	cfg, err := nutrition.BuildConfig(nutrition.Deps{AgentID: d.AgentID})
	if err != nil {
		return agentruntime.Config{}, err
	}
	if err := wireMemory(&cfg, d); err != nil {
		return agentruntime.Config{}, err
	}
	wireGateway(&cfg, d)
	return cfg, nil
}

func buildTestAgent(d Deps) (agentruntime.Config, error) {
	reg := tool.NewRegistry()
	reg.Register(testagent.MarkerTool{DB: d.DB})
	reg.Register(testagent.SleepTool{})
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
	wireGateway(&cfg, d)
	return cfg, nil
}
