package main

import (
	"context"
	"fmt"
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

	// Start agents sequentially with a readiness gate: spawn one, wait for its
	// /healthz, then spawn the next. DBOS's first-run schema initialization is
	// not safe to execute from multiple agent processes concurrently (they
	// stall racing to create the `dbos` schema), so we stagger cold starts.
	// Once an agent is up its schema exists, so the next agent initializes
	// against an existing schema and comes up fast.
	for _, info := range reg.List() {
		ap, _ := reg.Get(info.ID)
		sup := &controlplane.Supervisor{Spawn: ap.SpawnFunc(), Backoff: time.Second}
		go sup.Run(ctx)
		log.Printf("supervising agent %q at %s", ap.AgentID, ap.Addr)
		if err := waitAgentHealthy(ctx, ap.Addr, 30*time.Second); err != nil {
			log.Printf("warning: agent %q not healthy yet (%v); continuing", ap.AgentID, err)
		}
	}

	srv := &http.Server{Addr: ctlAddr, Handler: controlplane.NewAPI(reg)}
	go func() { <-ctx.Done(); _ = srv.Shutdown(context.Background()) }()
	log.Printf("control plane on %s hosting %d agents", ctlAddr, len(reg.List()))
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

// waitAgentHealthy polls an agent's /healthz until it returns 200, the timeout
// elapses, or ctx is cancelled.
func waitAgentHealthy(ctx context.Context, addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	url := "http://" + addr + "/healthz"
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		resp, err := client.Get(url)
		if err == nil {
			code := resp.StatusCode
			resp.Body.Close()
			if code == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return fmt.Errorf("timed out after %s", timeout)
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
