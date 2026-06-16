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
	"github.com/sausheong/runtime/internal/store"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"golang.org/x/oauth2"
)

// tracedHandler wraps h with an otelhttp server span named by matched route
// (never the raw path — cardinality-safe). Placed inside RequestID so the id is
// already in context; transparent when tracing is off (no-op provider).
func tracedHandler(h http.Handler) http.Handler {
	return otelhttp.NewHandler(h, "runtimed.request",
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			if r.Pattern != "" {
				return r.Method + " " + r.Pattern
			}
			return r.Method
		}),
	)
}

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

	traceShutdown, terr := obs.InitTracing(ctx, "runtimed")
	if terr != nil {
		slog.Warn("tracing init failed; continuing without traces", "err", terr)
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = traceShutdown(sctx)
	}()

	// Gateway (B1 M1 + v1.0-M1 self-service): the upstream manager is built
	// below, AFTER the secret broker exists, so it can take the per-tenant
	// credential resolver and be seeded from both file config and DB rows. The
	// var declarations live here so the identity branch can wire PrincipalFor and
	// pass gwHandler/gwManager into buildRoot. Start is deferred until after the
	// identity block: its failure paths os.Exit(1), which skips deferred cleanup,
	// and a started manager may have spawned stdio upstream children that would be
	// orphaned.
	var gwHandler *gateway.Handler
	var gwManager *gateway.Manager

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

	// Gateway upstream store (v1.0-M1 Task 2): persists self-registered upstreams
	// in the identity DB. Always constructed (both modes); only mounted via the
	// onboarding API when a gateway manager exists (identity on + brokering).
	gwStore, err := gateway.NewUpstreamStore(ctx, identityDB)
	if err != nil {
		slog.Error("gateway upstream store init failed", "err", err)
		os.Exit(1)
	}

	ctlStore, err := store.NewPGStore(ctx, dsn)
	if err != nil {
		slog.Error("control store init failed", "err", err)
		os.Exit(1)
	}
	defer ctlStore.Close()

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

	// Gateway (B1 + v1.0-M1 self-service): build the manager when file upstreams
	// exist OR a secrets broker is available (so tenants can self-register
	// upstreams even with zero file config). Seed from file config + DB rows;
	// inject the per-tenant credential resolver backed by the broker. Built here,
	// after secretBroker/secretAdmin are established and BEFORE the identity
	// branch, but still BEFORE gwManager.Start (below) — preserving the
	// "no Start until every os.Exit path has passed" invariant.
	gatewayActive := cfg.Gateway.Enabled() || secretBroker != nil
	if gatewayActive {
		gwURL := gatewaySelfURL(cfg.Gateway.SelfURL, ctlAddr)
		reg.SetGateway(gwURL, cfg.Gateway.AgentKeys)

		dbRows, lerr := gwStore.ListUpstreams(ctx, "") // all tenants
		if lerr != nil {
			slog.Error("gateway: load upstreams failed", "err", lerr)
			os.Exit(1)
		}
		servers := append([]config.GatewayServer(nil), cfg.Gateway.Servers...)
		for _, row := range dbRows {
			servers = append(servers, row.ToConfig())
		}
		var resolver gateway.CredentialResolver
		if secretBroker != nil {
			resolver = func(rctx context.Context, tenant, name string) (string, error) {
				m, serr := secretBroker.SecretsFor(rctx, tenant)
				if serr != nil {
					// SecretsFor's decrypt error embeds the secret NAME; scrub it
					// here so the name never reaches gateway logs/LastError/Status.
					return "", fmt.Errorf("required credential could not be resolved for tenant")
				}
				v, ok := m[name]
				if !ok {
					return "", fmt.Errorf("required credential not found for tenant")
				}
				return v, nil
			}
		}
		// WithCredentials(nil) is safe: dialWith only invokes the resolver when
		// m.cred != nil, so a nil resolver (file upstreams, no broker) is a no-op.
		gwManager = gateway.NewManager(servers, gateway.WithCredentials(resolver))
		gwHandler = gateway.NewHandler(gwManager)
		// Metrics wiring must precede gwManager.Start (no race on the first
		// connect transition).
		gwManager.Metrics = cm
		gwHandler.Metrics = cm
		slog.Info("gateway enabled", "file_upstreams", len(cfg.Gateway.Servers), "db_upstreams", len(dbRows), "url", gwURL)

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
		if verr := validateGatewaySearch(cfg, embOn); verr != nil {
			slog.Error("gateway search misconfigured", "err", verr)
			os.Exit(1)
		}
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
		// Open mode: no admin store ⇒ no /admin, no onboarding API, no onboarding
		// page. Pass literal nil for the interface params (true nil interface).
		handler = obs.RequestID(tracedHandler(accessLog(buildRoot(reg, nil, console.OIDCConfig{}, secretAdmin, gwHandler, nil, nil, nil, cm, ctlStore), cm)))
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
		// Self-service onboarding (v1.0-M1): only when a gateway manager exists.
		// Guard the GatewayMutator interface assignment to avoid the typed-nil
		// trap — a nil *gateway.Manager stored in the interface would be != nil.
		var gwMut controlplane.GatewayMutator
		var onb *console.Onboarding
		if gwManager != nil {
			gwMut = gwManager
			onb = &console.Onboarding{
				Upstreams: gwStore,
				Mutator:   gwManager,
				Admin:     idStore,
				Secrets:   secretAdmin,
			}
		}
		root := buildRoot(reg, idStore, consoleOIDC, secretAdmin, gwHandler, gwStore, gwMut, onb, cm, ctlStore) // mounts /admin since the store is non-nil
		onReject := func(status int) { cm.AuthRejected(status) }
		// When OIDC login is available, lock the browser console to OIDC sessions:
		// a service-key/bootstrap cookie authenticates the API but is bounced from
		// /ui to the Google sign-in. Without OIDC there is no console session to
		// require, so fall back to the standard middleware (token cookie still works
		// for the UI) — otherwise the console would be unreachable.
		mw := controlplane.IdentityMiddleware
		if consoleOIDC.Enabled {
			mw = controlplane.IdentityMiddlewareConsoleOIDCOnly
		}
		handler = obs.RequestID(tracedHandler(mw(accessLog(root, cm), authr, azr, onReject)))
		slog.Info("identity enabled", "oidc", oidcIssuer != "", "console_oidc_only", consoleOIDC.Enabled, "bootstrap", bootstrapKey != "", "legacy_tokens", len(legacyTokens))
	}

	// Mounted OUTSIDE the identity/access-log chain (like /healthz — standard
	// Prometheus practice; spec §5: label values are operator-level identifiers,
	// never tenant/user data). Scrape probes get no request-id/access-log
	// treatment by design (probe noise).
	// POST /register is authenticated by the agent's own registration token, not
	// the identity middleware, so it is mounted on the same pre-identity outer mux
	// as /metrics. idStore is always constructed (both modes) and satisfies
	// controlplane.RegTokenVerifier via ActiveRegTokenByID.
	regMux := http.NewServeMux()
	controlplane.RegisterHandshake(regMux, idStore, reg)
	handler = mountMetrics(handler, cm, func() []obs.ScrapeTarget {
		var ts []obs.ScrapeTarget
		for _, info := range reg.List() {
			replicas, _ := reg.Replicas(info.ID)
			for _, ap := range replicas {
				ts = append(ts, obs.ScrapeTarget{
					Agent: ap.AgentID, Replica: ap.ReplicaIndex,
					BaseURL: ap.DialBase(), Token: ap.AuthToken,
				})
			}
		}
		return ts
	}, regMux)

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

	// Autoscaled agents (Spine A2): each PoolManager owns its replicas + policy
	// loop. Start them with the same readiness gate that serializes DBOS schema
	// init, then the policy goroutine grows/drains by load. Tuning via env.
	asPoll := envFloatOr("RUNTIME_AUTOSCALE_POLL_SECONDS", 0)
	asUpCD := envFloatOr("RUNTIME_AUTOSCALE_UP_COOLDOWN_SECONDS", 0)
	asDownCD := envFloatOr("RUNTIME_AUTOSCALE_DOWN_COOLDOWN_SECONDS", 0)
	for id, pm := range reg.Pools() {
		pm.SetDeps(ctlStore, cm, func(ctx context.Context, addr string) error {
			return waitAgentHealthy(ctx, addr, 30*time.Second)
		})
		pm.ApplyTuning(asPoll, asUpCD, asDownCD)
		if err := pm.Start(ctx); err != nil {
			// Degrade-don't-fail (consistent with the static boot loop and the
			// post-gateway-start no-fatal-exit discipline): the policy loop will
			// keep retrying grow toward min. An os.Exit here would orphan gateway
			// stdio children and skip deferred cleanup.
			slog.Error("autoscaled agent could not start its first replica; policy loop will retry", "agent", id, "err", err)
		}
		slog.Info("autoscaling agent", "agent", id)
	}

	// Start agents sequentially with a readiness gate (M2: DBOS first-run
	// schema init is not safe to run concurrently).
	for _, info := range reg.List() {
		if _, isPool := reg.Pools()[info.ID]; isPool {
			continue // autoscaled: started above via its PoolManager
		}
		replicas, _ := reg.Replicas(info.ID)
		for _, ap := range replicas {
			if ap.Remote {
				id := ap.AgentID
				idx := ap.ReplicaIndex
				hm := &controlplane.HealthMonitor{
					BaseURL: ap.DialBase(), Token: ap.AuthToken,
					OnChange: func(ok bool) {
						cm.AgentReachable(id, idx, ok)
						reg.SetReachable(id, idx, ok)
					},
				}
				go hm.Run(ctx)
				slog.Info("monitoring remote agent", "agent", ap.AgentID, "replica", idx, "url", ap.DialBase())
				continue
			}
			idx := ap.ReplicaIndex
			sup := &controlplane.Supervisor{
				Spawn:     ap.SpawnFunc(),
				Backoff:   time.Second,
				OnRestart: func() { cm.AgentRestart(ap.AgentID, idx) },
			}
			go sup.Run(ctx)
			slog.Info("supervising agent replica", "agent", ap.AgentID, "replica", idx, "addr", ap.Addr)
			if err := waitAgentHealthy(ctx, ap.Addr, 30*time.Second); err != nil {
				slog.Warn("agent replica not healthy yet; continuing", "agent", ap.AgentID, "replica", idx, "err", err)
			}
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
func mountMetrics(inner http.Handler, cm *obs.ControlMetrics, targets func() []obs.ScrapeTarget, registerMux http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", obs.FanoutHandler(cm, targets))
	// POST /register authenticates with the agent's own per-agent registration
	// token (not the identity middleware), so — like /metrics — it lives on this
	// outer mux, reachable WITHOUT an identity principal in both open and
	// identity-on modes.
	mux.Handle("POST /register", registerMux)
	mux.Handle("/", inner)
	return mux
}

// buildRoot assembles the root mux: console at /ui, control-plane API at /, and
// (when adminS is non-nil) the admin API at /admin. The secret admin is mounted
// alongside the admin API; a nil secretBroker makes /admin/secrets return 503.
// A nil gw leaves /gateway/* unmounted (stdlib mux → 404). The self-service
// onboarding API (/admin/upstreams) mounts only when adminS, us, and gwMut are
// all non-nil; onb (when non-nil) enables the console onboarding page.
func buildRoot(reg *controlplane.Registry, adminS controlplane.AdminStore, consoleOIDC console.OIDCConfig, secretBroker controlplane.SecretAdmin, gw *gateway.Handler, us controlplane.UpstreamStore, gwMut controlplane.GatewayMutator, onb *console.Onboarding, cm *obs.ControlMetrics, ctlStore store.Store) http.Handler {
	apiMux := controlplane.NewAPI(reg, cm, ctlStore)
	if adminS != nil {
		controlplane.RegisterAdmin(apiMux, adminS, reg.AgentTenants())
		controlplane.RegisterSecretAdmin(apiMux, adminS, secretBroker)
		// Onboarding API mounts only with an admin store AND a live gateway
		// (us+gwMut non-nil): self-service requires both persistence and a
		// manager to add the upstream to.
		if us != nil && gwMut != nil {
			controlplane.RegisterUpstreamAdmin(apiMux, adminS, us, gwMut)
		}
	}
	if gw != nil {
		apiMux.Handle("/gateway/mcp", gw.HTTP())
		apiMux.HandleFunc("GET /gateway/status", gw.Status)
	}
	consoleH := console.Handler(reg, ctlStore, consoleOIDC, onb)
	root := http.NewServeMux()
	root.Handle("/ui", consoleH)
	root.Handle("/ui/", consoleH)
	// Exact root → the console landing page (a browser's front door). Bare "/"
	// has no API route (it 404s), so repurposing only the exact path leaves every
	// other API path on apiMux untouched. "/{$}" matches "/" only, not the catch-all.
	root.Handle("/{$}", consoleH)
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
