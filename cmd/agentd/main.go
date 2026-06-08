// Command agentd is the agent subprocess the control plane spawns. It reads its
// configuration from the environment, builds a harness agent backed by the
// deterministic test agent (scripted provider + marker tool), and serves the
// agentruntime HTTP/SSE contract with a DBOS-backed durable per-session loop.
package main

import (
	"context"
	"database/sql"
	"log"
	"os"
	"os/signal"
	"syscall"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/sausheong/harness/tool"
	hrt "github.com/sausheong/harness/runtime"

	"github.com/sausheong/runtime/agentruntime"
	"github.com/sausheong/runtime/internal/store"
	"github.com/sausheong/runtime/testagent"
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

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		log.Fatalf("agentd: open postgres: %v", err)
	}
	defer db.Close()

	// Create the marker table under the shared DDL advisory lock so multiple
	// agentd processes starting concurrently (spawned by runtimed) don't race
	// on CREATE TABLE — which is not atomic against a concurrent creator in
	// Postgres.
	if err := store.ApplyDDLLocked(context.Background(), db,
		`CREATE TABLE IF NOT EXISTS markers (id BIGSERIAL PRIMARY KEY, ran_at TIMESTAMPTZ)`,
	); err != nil {
		log.Fatalf("agentd: create markers table: %v", err)
	}

	reg := tool.NewRegistry()
	reg.Register(testagent.MarkerTool{DB: db})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg := agentruntime.Config{
		Spec: hrt.AgentSpec{
			ID:       agentID,
			Name:     agentID,
			Model:    "test/scripted",
			MaxTurns: 10,
		},
		Provider:    testagent.New(),
		Tools:       reg,
		ListenAddr:  addr,
		PostgresDSN: dsn,
	}

	if err := agentruntime.Serve(ctx, cfg); err != nil {
		log.Fatalf("agentd: serve: %v", err)
	}
}
