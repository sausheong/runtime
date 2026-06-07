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
	"github.com/sausheong/runtime/internal/config"
)

func main() {
	dsn := envOr("RUNTIME_PG_DSN", "postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable")
	ctlAddr := envOr("RUNTIME_CTL_ADDR", ":8080")
	agentBin := envOr("RUNTIME_AGENTD_BIN", "./agentd")
	cfgPath := envOr("RUNTIME_CONFIG", "runtime.yaml")

	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	reg := controlplane.NewRegistry(cfg, agentBin, dsn)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	for _, info := range reg.List() {
		ap, _ := reg.Get(info.ID)
		sup := &controlplane.Supervisor{Spawn: ap.SpawnFunc(), Backoff: time.Second}
		go sup.Run(ctx)
		log.Printf("supervising agent %q at %s", ap.AgentID, ap.Addr)
	}

	srv := &http.Server{Addr: ctlAddr, Handler: controlplane.NewAPI(reg)}
	go func() { <-ctx.Done(); _ = srv.Shutdown(context.Background()) }()
	log.Printf("control plane on %s hosting %d agents", ctlAddr, len(reg.List()))
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
