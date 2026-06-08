package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/sausheong/runtime/console"
	"github.com/sausheong/runtime/controlplane"
	"github.com/sausheong/runtime/internal/config"
	"github.com/sausheong/runtime/internal/identity"
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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Identity layer (M1). Operator config via env:
	//   RUNTIME_OIDC_ISSUER / RUNTIME_OIDC_CLIENT_ID — enable OIDC human login.
	//   RUNTIME_ADMIN_BOOTSTRAP — one-time superuser service key (break-glass).
	//
	// Initialized BEFORE spawning agents: its failure paths os.Exit(1), which
	// skips deferred cleanup. Doing it first means no agentd subprocess has been
	// spawned yet, so a misconfig (e.g. bad OIDC issuer) can't orphan children.
	oidcIssuer := os.Getenv("RUNTIME_OIDC_ISSUER")
	oidcClientID := os.Getenv("RUNTIME_OIDC_CLIENT_ID")
	bootstrapKey := os.Getenv("RUNTIME_ADMIN_BOOTSTRAP")
	if oidcIssuer == "" && oidcClientID != "" {
		slog.Warn("RUNTIME_OIDC_CLIENT_ID set but RUNTIME_OIDC_ISSUER is empty — OIDC disabled")
	}
	legacyTokens := cfg.TokenMap()

	var handler http.Handler

	// Open a (separate) connection pool to the same Postgres the agents' control
	// plane uses; identity tables are created here under the shared DDL lock.
	identityDB, err := sql.Open("pgx", dsn)
	if err != nil {
		slog.Error("identity db open failed", "err", err)
		os.Exit(1)
	}
	defer identityDB.Close()
	idStore, err := identity.NewStore(ctx, identityDB)
	if err != nil {
		slog.Error("identity store init failed", "err", err)
		os.Exit(1)
	}

	configured, err := idStore.AnyConfigured(ctx)
	if err != nil {
		slog.Error("identity configured-check failed", "err", err)
		os.Exit(1)
	}
	identityOn := configured || oidcIssuer != "" || bootstrapKey != "" || len(legacyTokens) > 0

	if !identityOn {
		slog.Warn("no identity configured — control plane is running OPEN (unauthenticated)")
		handler = accessLog(buildRoot(reg, nil)) // no /admin in open mode
	} else {
		oidcVerifier, verr := identity.NewOIDCVerifier(ctx, oidcIssuer, oidcClientID)
		if verr != nil {
			slog.Error("oidc init failed", "issuer", oidcIssuer, "err", verr)
			os.Exit(1)
		}
		authr := identity.NewAuthenticator(idStore, oidcVerifier, bootstrapKey, legacyTokens)
		azr := identity.NewAuthorizer(reg.AgentTenants())
		root := buildRoot(reg, idStore) // mounts /admin since the store is non-nil
		handler = controlplane.IdentityMiddleware(accessLog(root), authr, azr)
		slog.Info("identity enabled", "oidc", oidcIssuer != "", "bootstrap", bootstrapKey != "", "legacy_tokens", len(legacyTokens))
	}

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

	srv := &http.Server{Addr: ctlAddr, Handler: handler}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()
	slog.Info("control plane listening", "addr", ctlAddr, "agents", len(reg.List()), "identity", identityOn)

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

// statusRecorder captures the response status code for access logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Flush forwards to the underlying ResponseWriter so SSE streaming still flushes
// immediately through this wrapper.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// accessLog logs one structured line per request, including the authenticated
// principal subject and tenant (empty in open mode) from the identity context.
func accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		var subject, tenant string
		if p, ok := controlplane.PrincipalFromContext(r.Context()); ok {
			subject, tenant = p.Subject, p.TenantID
		}
		slog.Info("request",
			"method", r.Method, "path", r.URL.Path,
			"status", rec.status, "subject", subject, "tenant", tenant, "remote", r.RemoteAddr)
	})
}

// buildRoot assembles the root mux: console at /ui, control-plane API at /, and
// (when adminS is non-nil) the admin API at /admin. Admin handlers self-enforce
// the admin role; mounting is gated here so open mode has no /admin surface.
func buildRoot(reg *controlplane.Registry, adminS controlplane.AdminStore) http.Handler {
	apiMux := controlplane.NewAPI(reg)
	if adminS != nil {
		controlplane.RegisterAdmin(apiMux, adminS)
	}
	consoleH := console.Handler(reg)
	root := http.NewServeMux()
	root.Handle("/ui", consoleH)
	root.Handle("/ui/", consoleH)
	root.Handle("/", apiMux)
	return root
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
