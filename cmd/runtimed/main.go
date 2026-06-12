package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/sausheong/runtime/console"
	"github.com/sausheong/runtime/controlplane"
	"github.com/sausheong/runtime/internal/config"
	"github.com/sausheong/runtime/internal/gateway"
	"github.com/sausheong/runtime/internal/identity"
	"github.com/sausheong/runtime/internal/memory"
	"github.com/sausheong/runtime/internal/obs"
	"golang.org/x/oauth2"
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

	// Control-plane metrics registry: created early so the gateway, edge
	// middleware, supervisors, and proxy hooks below all share the one registry.
	cm := obs.NewControlMetrics()

	reg := controlplane.NewRegistry(cfg, agentBin, dsn)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Gateway (B1 M1): build the upstream manager when configured. PrincipalFor
	// is wired below once we know whether identity is on (open vs enforced).
	// Start is deferred until after the identity block: its failure paths
	// os.Exit(1), which skips deferred cleanup, and a started manager may have
	// spawned stdio upstream children that would be orphaned.
	var gwHandler *gateway.Handler
	var gwManager *gateway.Manager
	if cfg.Gateway.Enabled() {
		gwURL := gatewaySelfURL(cfg.Gateway.SelfURL, ctlAddr)
		reg.SetGateway(gwURL, cfg.Gateway.AgentKeys)
		gwManager = gateway.NewManager(cfg.Gateway.Servers)
		gwHandler = gateway.NewHandler(gwManager)
		// Metrics wiring must precede gwManager.Start (no race on the first
		// connect transition).
		gwManager.Metrics = cm
		gwHandler.Metrics = cm
		slog.Info("gateway enabled", "upstreams", len(cfg.Gateway.Servers), "url", gwURL)

		// Gateway search (M2): assemble the Index from the Memory embedder env
		// and fail fast when a search-mode agent exists without embeddings.
		// Safe to os.Exit here: gwManager.Start runs later, so no stdio upstream
		// children have been spawned yet.
		emb, _, embOn, eerr := memory.NewEmbedderFromEnv()
		if eerr != nil {
			slog.Error("gateway: embeddings config invalid", "err", eerr)
			os.Exit(1)
		}
		if embOn {
			floor := envFloatOr("RUNTIME_GATEWAY_SEARCH_FLOOR", 0.2)
			k := envIntOr("RUNTIME_GATEWAY_SEARCH_K", 5)
			gwHandler.Index = gateway.NewIndex(emb, floor, k)
			slog.Info("gateway search enabled", "floor", floor, "k", k)
		}
		if err := validateGatewaySearch(cfg, embOn); err != nil {
			slog.Error("gateway search misconfigured", "err", err)
			os.Exit(1)
		}
	}

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

	// Secret broker (Identity M2/M3): built whenever a secrets keyring is
	// configured (RUNTIME_SECRETS_KEYS or the legacy RUNTIME_SECRETS_KEY),
	// independent of whether identity enforcement is on. Injected into the
	// registry so each agent's SpawnFunc brokers its tenant's secrets.
	secretBroker := buildSecretBroker(idStore)
	if secretBroker != nil {
		reg.SetBroker(secretBroker)
	}
	// secretAdmin is a true-nil interface when brokering is disabled, so
	// RegisterSecretAdmin's `sa == nil` 503 guard works (avoids the typed-nil
	// interface trap).
	var secretAdmin controlplane.SecretAdmin
	if secretBroker != nil {
		secretAdmin = secretBroker
	}

	configured, err := idStore.AnyConfigured(ctx)
	if err != nil {
		slog.Error("identity configured-check failed", "err", err)
		os.Exit(1)
	}
	identityOn := configured || oidcIssuer != "" || bootstrapKey != "" || len(legacyTokens) > 0

	// Fail-closed: identity on + a gateway:true agent whose tenant has no
	// agent key would spawn an agent that can never authenticate to the
	// gateway. Refuse to start instead.
	if identityOn && cfg.Gateway.Enabled() {
		if err := validateGatewayKeys(cfg); err != nil {
			slog.Error("gateway agent has no agent_key for its tenant (identity is on)", "err", err)
			os.Exit(1)
		}
	}

	if !identityOn {
		slog.Warn("no identity configured — control plane is running OPEN (unauthenticated)")
		if secretBroker != nil {
			// The broker still injects secrets into spawns, but /admin/secrets is
			// only mounted with an admin store (identity on). So a key is set yet
			// no secret can be created — warn rather than silently mislead.
			slog.Warn("a secrets key is set (RUNTIME_SECRETS_KEYS/RUNTIME_SECRETS_KEY) but identity is open/unconfigured — /admin/secrets is unavailable and no secrets can be set; configure identity (OIDC, a service key, or RUNTIME_ADMIN_BOOTSTRAP) to manage secrets")
		}
		if gwHandler != nil {
			gwHandler.PrincipalFor = gateway.OpenMode
		}
		handler = obs.RequestID(accessLog(buildRoot(reg, nil, console.OIDCConfig{}, secretAdmin, gwHandler, cm), cm)) // no /admin in open mode
	} else {
		oidcVerifier, verr := identity.NewOIDCVerifier(ctx, oidcIssuer, oidcClientID)
		if verr != nil {
			slog.Error("oidc init failed", "issuer", oidcIssuer, "err", verr)
			os.Exit(1)
		}
		consoleOIDC := console.OIDCConfig{}
		if oidcIssuer != "" {
			if prov, perr := oidc.NewProvider(ctx, oidcIssuer); perr == nil {
				oauthCfg := &oauth2.Config{
					ClientID:     oidcClientID,
					ClientSecret: os.Getenv("RUNTIME_OIDC_CLIENT_SECRET"),
					Endpoint:     prov.Endpoint(),
					RedirectURL:  envOr("RUNTIME_OIDC_REDIRECT_URL", "http://localhost:8080/ui/callback"),
					Scopes:       []string{oidc.ScopeOpenID, "email"},
				}
				consoleOIDC = console.OIDCConfig{
					Enabled:     true,
					AuthCodeURL: func(state string) string { return oauthCfg.AuthCodeURL(state) },
					Exchange: func(c context.Context, code string) (string, error) {
						tok, exErr := oauthCfg.Exchange(c, code)
						if exErr != nil {
							return "", exErr
						}
						raw, ok := tok.Extra("id_token").(string)
						if !ok {
							return "", fmt.Errorf("no id_token in token response")
						}
						return raw, nil
					},
				}
			} else {
				slog.Warn("oidc provider discovery failed; console OIDC login disabled", "err", perr)
			}
		}
		authr := identity.NewAuthenticator(idStore, oidcVerifier, bootstrapKey, legacyTokens)
		azr := identity.NewAuthorizer(reg.AgentTenants())
		if gwHandler != nil {
			gwHandler.PrincipalFor = controlplane.PrincipalFromContext
		}
		root := buildRoot(reg, idStore, consoleOIDC, secretAdmin, gwHandler, cm) // mounts /admin since the store is non-nil
		handler = obs.RequestID(controlplane.IdentityMiddleware(accessLog(root, cm), authr, azr, func(status int) {
			cm.AuthRejected(status)
		}))
		slog.Info("identity enabled", "oidc", oidcIssuer != "", "bootstrap", bootstrapKey != "", "legacy_tokens", len(legacyTokens))
	}

	// Mounted OUTSIDE the identity/access-log chain (like /healthz — standard
	// Prometheus practice; spec §5: label values are operator-level identifiers,
	// never tenant/user data). Scrape probes get no request-id/access-log
	// treatment by design (probe noise).
	handler = mountMetrics(handler, cm, func() []obs.ScrapeTarget {
		var ts []obs.ScrapeTarget
		for _, info := range reg.List() {
			ap, _ := reg.Get(info.ID)
			ts = append(ts, obs.ScrapeTarget{Agent: ap.AgentID, BaseURL: ap.DialBase(), Token: ap.AuthToken})
		}
		return ts
	})

	// Start the gateway upstreams only now: every os.Exit(1) path above has
	// passed, so the deferred Close is guaranteed to run and stdio upstream
	// children can't be orphaned by a misconfig exit.
	if gwManager != nil {
		gwManager.Start(ctx)
		defer gwManager.Close()
	}

	// Server starts before agents so gateway-enabled agents can connect to
	// /gateway/mcp on first spawn.
	srv := &http.Server{Addr: ctlAddr, Handler: handler}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()
	slog.Info("control plane listening", "addr", ctlAddr, "agents", len(reg.List()), "identity", identityOn)

	// Start agents sequentially with a readiness gate (M2: DBOS first-run
	// schema init is not safe to run concurrently).
	for _, info := range reg.List() {
		ap, _ := reg.Get(info.ID)
		sup := &controlplane.Supervisor{Spawn: ap.SpawnFunc(), Backoff: time.Second, OnRestart: func() { cm.AgentRestart(ap.AgentID) }}
		go sup.Run(ctx)
		slog.Info("supervising agent", "agent", ap.AgentID, "addr", ap.Addr)
		if err := waitAgentHealthy(ctx, ap.Addr, 30*time.Second); err != nil {
			slog.Warn("agent not healthy yet; continuing", "agent", ap.AgentID, "err", err)
		}
	}

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
// principal subject and tenant (empty in open mode) from the identity context,
// and records the request in the control-plane HTTP metrics. The metrics route
// label is the matched mux pattern (r.Pattern, Go 1.22+), never the raw path —
// raw paths would explode label cardinality. Unmatched requests (404s, stdlib
// redirects) share the "unmatched" bucket.
//
// In identity mode, requests rejected by IdentityMiddleware never reach this
// handler: they are counted only under route="auth_rejected" (via the
// middleware's onReject hook) — no per-route metric, no access-log line, and
// no principal, by design.
func accessLog(next http.Handler, cm *obs.ControlMetrics) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		// r.Pattern is set by ServeMux on match, as "METHOD /path/{param}" (or
		// just "/path/{param}" for method-less patterns); strip the method prefix.
		route := r.Pattern
		if route == "" {
			route = "unmatched"
		} else if i := strings.IndexByte(route, ' '); i >= 0 {
			route = route[i+1:]
		}
		cm.HTTPObserved(route, r.Method, rec.status, time.Since(start))
		var subject, tenant string
		if p, ok := controlplane.PrincipalFromContext(r.Context()); ok {
			subject, tenant = p.Subject, p.TenantID
		}
		slog.Info("request",
			"method", r.Method, "path", r.URL.Path,
			"status", rec.status, "subject", subject, "tenant", tenant, "remote", r.RemoteAddr,
			"request_id", obs.RequestIDFromContext(r.Context()))
	})
}

// mountMetrics overlays GET /metrics OUTSIDE the identity/access-log chain
// (like /healthz — standard Prometheus practice; spec §5: label values are
// operator-level identifiers, never tenant/user data). Everything else falls
// through to the wrapped handler chain.
//
// r.Pattern note: this outer mux sets r.Pattern ("/") on fall-through, but the
// inner root mux overwrites it on match, and accessLog reads r.Pattern only
// AFTER next.ServeHTTP returns — so route normalization in the metrics/access
// log is unaffected.
func mountMetrics(inner http.Handler, cm *obs.ControlMetrics, targets func() []obs.ScrapeTarget) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", obs.FanoutHandler(cm, targets))
	mux.Handle("/", inner)
	return mux
}

// buildRoot assembles the root mux: console at /ui, control-plane API at /, and
// (when adminS is non-nil) the admin API at /admin. The secret admin is mounted
// alongside the admin API; a nil secretBroker makes /admin/secrets return 503.
// A nil gw leaves /gateway/* unmounted (stdlib mux → 404).
func buildRoot(reg *controlplane.Registry, adminS controlplane.AdminStore, consoleOIDC console.OIDCConfig, secretBroker controlplane.SecretAdmin, gw *gateway.Handler, cm *obs.ControlMetrics) http.Handler {
	apiMux := controlplane.NewAPI(reg, cm)
	if adminS != nil {
		controlplane.RegisterAdmin(apiMux, adminS)
		controlplane.RegisterSecretAdmin(apiMux, adminS, secretBroker)
	}
	if gw != nil {
		apiMux.Handle("/gateway/mcp", gw.HTTP())
		apiMux.HandleFunc("GET /gateway/status", gw.Status)
	}
	consoleH := console.Handler(reg, consoleOIDC)
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

// validateGatewayKeys returns an error naming the first gateway-enabled agent
// whose tenant has no agent key. Only meaningful when identity is enforced.
func validateGatewayKeys(cfg *config.Config) error {
	for _, a := range cfg.Agents {
		if a.Gateway.Enabled() && cfg.Gateway.AgentKeys[a.Tenant] == "" {
			return fmt.Errorf("gateway agent %q has no agent_key for tenant %q", a.ID, a.Tenant)
		}
	}
	return nil
}

// validateGatewaySearch returns an error naming the first agent that opted
// into gateway search mode while embeddings are not configured — search mode
// cannot work without an embedder, so refuse to start (fail-fast, like
// validateGatewayKeys).
func validateGatewaySearch(cfg *config.Config, embeddingsOn bool) error {
	if embeddingsOn {
		return nil
	}
	for _, a := range cfg.Agents {
		if a.Gateway == config.GatewaySearch {
			return fmt.Errorf("agent %q has gateway: search but embeddings are not configured (RUNTIME_EMBED_MODEL)", a.ID)
		}
	}
	return nil
}

// gatewaySelfURL derives the URL agents use to reach the gateway. An explicit
// self_url wins; otherwise it comes from the control-plane listen address with
// a wildcard/empty host rewritten to loopback (agents are local subprocesses).
func gatewaySelfURL(selfURL, ctlAddr string) string {
	if selfURL != "" {
		return strings.TrimRight(selfURL, "/") + "/gateway/mcp"
	}
	host, port, err := net.SplitHostPort(ctlAddr)
	if err != nil {
		// Malformed listen addr; fall back to loopback + raw addr (best effort).
		return "http://127.0.0.1" + ctlAddr + "/gateway/mcp"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/gateway/mcp"
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// envFloatOr reads a float env var with a default (malformed ⇒ default + warn).
func envFloatOr(key string, def float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		slog.Warn("ignoring malformed env float", "key", key, "value", v, "default", def)
		return def
	}
	return f
}

// envIntOr reads an int env var with a default (malformed ⇒ default + warn).
func envIntOr(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		slog.Warn("ignoring malformed env int", "key", key, "value", v, "default", def)
		return def
	}
	return n
}

// buildSecretBroker constructs a secret broker from the keyring env vars over the
// identity store. Returns nil when no key is configured (feature disabled,
// backward-compatible). Any malformed config is an operator error and is fatal.
//
//	RUNTIME_SECRETS_KEYS    "id:base64key,id:base64key" (each key 32 bytes)
//	RUNTIME_SECRETS_PRIMARY id new writes seal under (required when KEYS is set)
//	RUNTIME_SECRETS_KEY     legacy single key; also names the version-less decrypt key
func buildSecretBroker(idStore *identity.Store) *identity.Broker {
	kr, err := identity.ParseKeyring(
		os.Getenv("RUNTIME_SECRETS_KEYS"),
		os.Getenv("RUNTIME_SECRETS_PRIMARY"),
		os.Getenv("RUNTIME_SECRETS_KEY"),
	)
	if err != nil {
		slog.Error("secrets keyring invalid", "err", err)
		os.Exit(1)
	}
	if kr == nil {
		slog.Info("secrets brokering disabled: no secrets key configured")
		return nil
	}
	slog.Info("secrets brokering enabled", "keys", kr.NumKeys(), "primary", kr.PrimaryID())
	return identity.NewBroker(idStore, kr)
}
