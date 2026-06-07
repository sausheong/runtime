package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sausheong/runtime/controlplane"
)

func main() {
	dsn := envOr("RUNTIME_PG_DSN", "postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable")
	agentAddr := envOr("RUNTIME_AGENT_ADDR", "127.0.0.1:8081")
	ctlAddr := envOr("RUNTIME_CTL_ADDR", ":8080")
	agentBin := envOr("RUNTIME_AGENTD_BIN", "./agentd")

	ap := controlplane.AgentProcess{AgentID: "default", Addr: agentAddr, BinPath: agentBin, PGDSN: dsn}
	sup := &controlplane.Supervisor{Spawn: ap.SpawnFunc(), Backoff: time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go sup.Run(ctx)

	mux := controlplane.NewAPI(agentAddr)
	srv := &http.Server{Addr: ctlAddr, Handler: mux}
	go func() { <-ctx.Done(); _ = srv.Shutdown(context.Background()) }()
	log.Printf("control plane on %s -> agent %s", ctlAddr, agentAddr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
