package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sausheong/runtime/controlplane"
	"github.com/sausheong/runtime/internal/config"
)

func main() {
	setupLogging()

	dsn := envOr("RUNTIME_PG_DSN", "postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable")
	ctlAddr := envOr("RUNTIME_CTL_ADDR", ":8080")
	agentBin := envOr("RUNTIME_AGENTD_BIN", "./agentd")
	cfgPath := envOr("RUNTIME_CONFIG", "runtime.yaml")

	cfg, err := config.Load(cfgPath)
	if err != nil {
		slog.Error("config load failed", "err", err)
		os.Exit(1)
	}
	reg := controlplane.NewRegistry(cfg, agentBin, dsn)
	tokens := cfg.TokenMap()
	if len(tokens) == 0 {
		slog.Warn("no API tokens configured — control plane is running OPEN (unauthenticated)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Start agents sequentially with a readiness gate (M2: DBOS first-run
	// schema init is not safe to run concurrently).
	for _, info := range reg.List() {
		ap, _ := reg.Get(info.ID)
		sup := &controlplane.Supervisor{Spawn: ap.SpawnFunc(), Backoff: time.Second}
		go sup.Run(ctx)
		slog.Info("supervising agent", "agent", ap.AgentID, "addr", ap.Addr)
		if err := waitAgentHealthy(ctx, ap.Addr, 30*time.Second); err != nil {
			slog.Warn("agent not healthy yet; continuing", "agent", ap.AgentID, "err", err)
		}
	}

	mux := controlplane.NewAPI(reg)
	// NOTE (Task 5): console routes are mounted here (composed with mux) before wrapping.
	handler := controlplane.AuthMiddleware(mux, tokens)

	srv := &http.Server{Addr: ctlAddr, Handler: handler}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()
	slog.Info("control plane listening", "addr", ctlAddr, "agents", len(reg.List()), "auth", len(tokens) > 0)

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	case err := <-serveErr:
		if err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "err", err)
			os.Exit(1)
		}
	}
}

func setupLogging() {
	var h slog.Handler
	if os.Getenv("RUNTIME_LOG_FORMAT") == "json" {
		h = slog.NewJSONHandler(os.Stderr, nil)
	} else {
		h = slog.NewTextHandler(os.Stderr, nil)
	}
	slog.SetDefault(slog.New(h))
}

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
