// Command agentd is the agent subprocess the control plane spawns. It reads its
// configuration from the environment, resolves an agent "kind" to a builder via
// the kind registry, and serves the agentruntime HTTP/SSE contract with a
// DBOS-backed durable per-session loop.
package main

import (
	"context"
	"database/sql"
	"log"
	"os"
	"os/signal"
	"syscall"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/sausheong/runtime/agentruntime"
	"github.com/sausheong/runtime/internal/agentkind"
	"github.com/sausheong/runtime/internal/store"
)

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("agentd: required env %s is not set", key)
	}
	return v
}

func main() {
	dsn := mustEnv("RUNTIME_PG_DSN")
	addr := mustEnv("RUNTIME_LISTEN_ADDR")
	agentID := mustEnv("RUNTIME_AGENT_ID")
	kind := os.Getenv("RUNTIME_AGENT_KIND") // "" ⇒ testagent

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		log.Fatalf("agentd: open postgres: %v", err)
	}
	defer db.Close()

	// Marker table for the test agent (under the shared DDL lock). Harmless for
	// other kinds; kept so the testagent kind needs no special-casing here.
	if err := store.ApplyDDLLocked(context.Background(), db,
		`CREATE TABLE IF NOT EXISTS markers (id BIGSERIAL PRIMARY KEY, ran_at TIMESTAMPTZ)`,
	); err != nil {
		log.Fatalf("agentd: create markers table: %v", err)
	}

	build, ok := agentkind.Get(kind)
	if !ok {
		log.Fatalf("agentd: unknown agent kind %q", kind)
	}
	cfg, err := build(agentkind.Deps{AgentID: agentID, ListenAddr: addr, PostgresDSN: dsn, DB: db})
	if err != nil {
		log.Fatalf("agentd: build agent kind %q: %v", kind, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := agentruntime.Serve(ctx, cfg); err != nil {
		log.Fatalf("agentd: serve: %v", err)
	}
}
