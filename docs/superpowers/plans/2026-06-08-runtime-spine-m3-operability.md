# Runtime Spine — Milestone 3: Operability Layer — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Make the multi-agent platform operable: token auth on the control plane, a read-only web console, structured logging, a contract conformance suite, and deploy polish (bounded shutdown, 503-on-restart, full-stack Compose).

**Architecture:** Additive over M2. `runtime.yaml` gains a `tokens:` section. `runtimed` wraps the M2 router mux with an auth middleware (header- or cookie-based bearer token), mounts a `//go:embed`-ed Go-template console under `/ui`, and standardizes logging on `slog`. A new `conformance` package exercises any agent against the HTTP/SSE contract, exposed via `runtimectl conformance`. The M2 routing/supervision/durability layers are unchanged beneath.

**Tech Stack:** Go 1.25.1+ stdlib (`net/http`, `html/template`, `//go:embed`, `log/slog`), `gopkg.in/yaml.v3` (already direct dep), Postgres + DBOS (unchanged). Ground truth = the `go` CLI; IGNORE IDE/LSP diagnostics (multi-module replace setup confuses gopls).

**Spec:** `docs/superpowers/specs/2026-06-08-runtime-spine-m3-operability-design.md`.

**Branch:** `feat/runtime-spine-m3` (already created). Commit with `git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "..."`. Postgres for integration: `postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable`.

**Key backward-compat rule:** when `runtime.yaml` has NO `tokens:`, auth is DISABLED (open mode) with a startup warning. This keeps every existing M1/M2 test and the dev experience working unchanged.

---

## File Structure

### New
- `controlplane/auth.go` — `AuthMiddleware`, token extraction (header or cookie), context label.
- `controlplane/auth_test.go`.
- `conformance/conformance.go` — `TestingT` interface, `Run(t, baseURL)`, individual checks.
- `conformance/conformance_test.go` — run against httptest fake agents (passing + broken).
- `console/console.go` — console handlers + `//go:embed` templates/static.
- `console/templates/*.html`, `console/static/app.js`, `console/static/style.css`.
- `console/console_test.go`.
- `deploy/Dockerfile` — multi-stage build of `runtimed` + `agentd`.
- `deploy/docker-compose.full.yml` — Postgres + runtimed.
- `test/auth_test.go`, `test/console_test.go`, `test/conformance_test.go` — integration (gated).

### Modified
- `internal/config/config.go` — add `TokenConfig` + `Tokens`; validate; a `TokenMap()` helper. `Validate` must NOT require agents-only (tokens optional).
- `internal/config/config_test.go` — token cases.
- `controlplane/proxy.go` — `reverseProxy` gets a 503 `ErrorHandler`.
- `cmd/runtimed/main.go` — slog setup, auth-wrap mux, mount console, bounded shutdown, `/agents` health field wiring.
- `controlplane/api.go` — `GET /agents` includes per-agent health (best-effort).
- `controlplane/registry.go` — `AgentInfo` may gain a `Healthy`/`status` field (or a separate probe helper).
- `cmd/runtimectl/main.go` — bearer header from `RUNTIME_TOKEN`; `conformance` subcommand.
- `agentruntime/serve.go`, `controlplane/*` — swap `log.Printf` → `slog`.
- `README.md` — auth, console, conformance, full-stack Compose.

---

## Task 1: Config tokens

**Files:** `internal/config/config.go`, `config_test.go`.

- [ ] **Step 1: Failing tests** — append to `internal/config/config_test.go`:

```go
func TestLoad_WithTokens(t *testing.T) {
	p := writeTmp(t, `
agents:
  - {id: a, name: A, model: m, listen_addr: 127.0.0.1:8101}
tokens:
  - {token: "abc", label: "ci"}
  - {token: "xyz", label: "ops"}
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Tokens) != 2 {
		t.Fatalf("tokens = %d, want 2", len(cfg.Tokens))
	}
	tm := cfg.TokenMap()
	if tm["abc"] != "ci" || tm["xyz"] != "ops" {
		t.Fatalf("TokenMap wrong: %+v", tm)
	}
}

func TestLoad_NoTokensIsValid(t *testing.T) {
	p := writeTmp(t, `
agents:
  - {id: a, name: A, model: m, listen_addr: 127.0.0.1:8101}
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Tokens) != 0 || len(cfg.TokenMap()) != 0 {
		t.Fatalf("expected no tokens")
	}
}

func TestLoad_DuplicateToken(t *testing.T) {
	p := writeTmp(t, `
agents:
  - {id: a, name: A, model: m, listen_addr: 127.0.0.1:8101}
tokens:
  - {token: "dup", label: "one"}
  - {token: "dup", label: "two"}
`)
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for duplicate token")
	}
}

func TestLoad_EmptyTokenString(t *testing.T) {
	p := writeTmp(t, `
agents:
  - {id: a, name: A, model: m, listen_addr: 127.0.0.1:8101}
tokens:
  - {token: "", label: "x"}
`)
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for empty token")
	}
}
```

- [ ] **Step 2:** `go test ./internal/config/ -run 'TestLoad_WithTokens|NoTokens|DuplicateToken|EmptyToken'` → FAIL.

- [ ] **Step 3: Implement** in `internal/config/config.go`. Add the type, the field, validation, and `TokenMap`:

```go
// TokenConfig is one control-plane API token. Label is for log attribution.
type TokenConfig struct {
	Token string `yaml:"token"`
	Label string `yaml:"label"`
}
```
Add to `Config`:
```go
	Tokens []TokenConfig `yaml:"tokens"`
```
Extend `Validate()` (append BEFORE `return nil`, after the agents loop):
```go
	seen := map[string]bool{}
	for i, tk := range c.Tokens {
		if tk.Token == "" {
			return fmt.Errorf("config: token[%d] has empty token string", i)
		}
		if seen[tk.Token] {
			return fmt.Errorf("config: duplicate token at index %d", i)
		}
		seen[tk.Token] = true
	}
```
Add a helper:
```go
// TokenMap returns token→label for all configured tokens. Empty when none.
func (c *Config) TokenMap() map[string]string {
	m := make(map[string]string, len(c.Tokens))
	for _, tk := range c.Tokens {
		m[tk.Token] = tk.Label
	}
	return m
}
```

- [ ] **Step 4:** `go test ./internal/config/ -v` → PASS (all). `go build ./...` → clean.

- [ ] **Step 5: Commit** `internal/config/`.

```bash
git add internal/config/
git commit -m "feat(config): optional control-plane API tokens"
```

---

## Task 2: Auth middleware

**Files:** `controlplane/auth.go`, `auth_test.go`.

- [ ] **Step 1: Failing test** — `controlplane/auth_test.go`:

```go
package controlplane

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		label, _ := TokenLabelFromContext(r.Context())
		w.Header().Set("X-Token-Label", label)
		w.WriteHeader(200)
	})
}

func TestAuth_OpenWhenNoTokens(t *testing.T) {
	h := AuthMiddleware(okHandler(), map[string]string{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/agents", nil))
	if rec.Code != 200 {
		t.Fatalf("open mode: code = %d, want 200", rec.Code)
	}
}

func TestAuth_ValidHeaderToken(t *testing.T) {
	h := AuthMiddleware(okHandler(), map[string]string{"abc": "ci"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/agents", nil)
	req.Header.Set("Authorization", "Bearer abc")
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("valid token: code = %d, want 200", rec.Code)
	}
	if rec.Header().Get("X-Token-Label") != "ci" {
		t.Fatalf("label not propagated: %q", rec.Header().Get("X-Token-Label"))
	}
}

func TestAuth_ValidCookieToken(t *testing.T) {
	h := AuthMiddleware(okHandler(), map[string]string{"abc": "ci"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/ui", nil)
	req.AddCookie(&http.Cookie{Name: "runtime_token", Value: "abc"})
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("cookie token: code = %d, want 200", rec.Code)
	}
}

func TestAuth_MissingAndInvalid(t *testing.T) {
	h := AuthMiddleware(okHandler(), map[string]string{"abc": "ci"})
	// missing
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/agents", nil))
	if rec.Code != 401 {
		t.Fatalf("missing token: code = %d, want 401", rec.Code)
	}
	// invalid
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/agents", nil)
	req.Header.Set("Authorization", "Bearer nope")
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("invalid token: code = %d, want 401", rec.Code)
	}
}

func TestAuth_HealthzExempt(t *testing.T) {
	h := AuthMiddleware(okHandler(), map[string]string{"abc": "ci"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Fatalf("healthz must be exempt: code = %d", rec.Code)
	}
}
```

- [ ] **Step 2:** `go test ./controlplane/ -run TestAuth` → FAIL (undefined).

- [ ] **Step 3: Implement** `controlplane/auth.go`:

```go
package controlplane

import (
	"context"
	"net/http"
	"strings"
)

type ctxKey int

const tokenLabelKey ctxKey = 0

// TokenLabelFromContext returns the matched token's label, if the request was
// authenticated. ok is false in open mode or when unset.
func TokenLabelFromContext(ctx context.Context) (label string, ok bool) {
	v, ok := ctx.Value(tokenLabelKey).(string)
	return v, ok
}

// extractToken pulls a bearer token from the Authorization header, falling back
// to the runtime_token cookie (EventSource and plain browser navigations can't
// set headers).
func extractToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	if c, err := r.Cookie("runtime_token"); err == nil {
		return c.Value
	}
	return ""
}

// AuthMiddleware gates next with bearer-token auth. tokens maps token→label.
// When tokens is empty, auth is DISABLED (open mode) — every request passes.
// GET /healthz is always exempt so liveness probes work without a token.
func AuthMiddleware(next http.Handler, tokens map[string]string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(tokens) == 0 || r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		tok := extractToken(r)
		label, ok := tokens[tok]
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), tokenLabelKey, label)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
```

- [ ] **Step 4:** `go test ./controlplane/ -run TestAuth -v` → PASS. `go build ./...` → clean.

- [ ] **Step 5: Commit** `controlplane/auth.go`, `auth_test.go`.

```bash
git add controlplane/auth.go controlplane/auth_test.go
git commit -m "feat(controlplane): bearer-token auth middleware (header or cookie)"
```

---

## Task 3: Wire auth + structured logging + bounded shutdown into runtimed

**Files:** `cmd/runtimed/main.go`, `controlplane/proxy.go`, plus `log.Printf`→`slog` swaps.

- [ ] **Step 1: 503 ErrorHandler on the proxy** — in `controlplane/proxy.go`, update `reverseProxy`:

```go
func reverseProxy(addr string) *httputil.ReverseProxy {
	target, _ := url.Parse("http://" + addr)
	rp := httputil.NewSingleHostReverseProxy(target)
	rp.FlushInterval = -1
	rp.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(w, "agent unavailable", http.StatusServiceUnavailable)
	}
	return rp
}
```
Add `"net/http"` to the imports if not present.

- [ ] **Step 2: Rewrite `cmd/runtimed/main.go`** to: set up slog, load tokens, wrap the mux with auth, mount the console, and bound the shutdown. (Console mount is added in Task 5; for THIS task add everything except the console mount, leaving a clear insertion point.) Full file:

```go
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

	// Start agents sequentially with a readiness gate (see M2: DBOS first-run
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
	// NOTE (Task 5): console routes are mounted onto `mux` here before wrapping.
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
```

- [ ] **Step 3: Swap `log.Printf`→`slog` in agentruntime/controlplane** where present. Check: `grep -rn 'log\.' agentruntime/ controlplane/ --include='*.go' | grep -v _test`. Replace any `log.Printf(...)` with an appropriate `slog.Info/Warn/Error(...)` call with structured fields, and drop the now-unused `"log"` import. (M2 left a couple; runtimed's are handled above.) Keep changes minimal and mechanical.

- [ ] **Step 4: Verify** `go build ./... && go vet ./... && go test ./...` → green. `go build -o /tmp/rd ./cmd/runtimed && echo ok && rm /tmp/rd`.

- [ ] **Step 5: Regression — integration tests must still pass** (auth is OFF in those configs since they set no tokens, so they're unaffected):

```bash
go test -tags integration ./test/ -count=1 -timeout 200s
```
Expected: both `TestResumeAfterKill` and `TestMultiAgentRouting` PASS (open mode).

- [ ] **Step 6: Commit.**

```bash
git add cmd/runtimed/main.go controlplane/proxy.go agentruntime/ controlplane/
git commit -m "feat(runtimed): auth-wrapped mux, slog logging, bounded shutdown, 503 proxy errors"
```

---

## Task 4: runtimectl bearer token + conformance command

**Files:** `cmd/runtimectl/main.go`, `conformance/conformance.go`, `conformance/conformance_test.go`.

- [ ] **Step 1: Conformance package failing test** — `conformance/conformance_test.go`:

```go
package conformance

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// goodAgent is a minimal in-memory fake satisfying the agent contract.
func goodAgent() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("GET /meta", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"agent_id": "a", "contract_version": "v1"})
	})
	mux.HandleFunc("POST /sessions", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"session_id": "ses-1"})
	})
	mux.HandleFunc("GET /sessions", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{{"id": "ses-1", "status": "completed", "turn_count": 1}})
	})
	mux.HandleFunc("GET /sessions/{id}", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "ses-1", "status": "completed", "turn_count": 1})
	})
	mux.HandleFunc("GET /sessions/{id}/stream", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"text\",\"text\":\"hi\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"done\"}\n\n"))
	})
	return mux
}

// recorder implements TestingT, collecting failures instead of aborting.
type recorder struct{ fails []string }

func (r *recorder) Errorf(f string, a ...any) { r.fails = append(r.fails, "err") }
func (r *recorder) Fatalf(f string, a ...any) { r.fails = append(r.fails, "fatal") }
func (r *recorder) Logf(f string, a ...any)   {}

func TestRun_GoodAgentPasses(t *testing.T) {
	srv := httptest.NewServer(goodAgent())
	defer srv.Close()
	rec := &recorder{}
	Run(rec, srv.URL)
	if len(rec.fails) != 0 {
		t.Fatalf("good agent should pass; got failures: %v", rec.fails)
	}
}

func TestRun_BrokenAgentFails(t *testing.T) {
	// Agent missing /meta contract_version.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("GET /meta", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"agent_id":"a"}`)) // no contract_version
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	rec := &recorder{}
	Run(rec, srv.URL)
	if len(rec.fails) == 0 {
		t.Fatal("broken agent (missing contract_version + endpoints) should fail conformance")
	}
}

var _ = strings.TrimSpace // keep import if unused after edits
```

- [ ] **Step 2:** `go test ./conformance/` → FAIL (package/Run undefined).

- [ ] **Step 3: Implement `conformance/conformance.go`:**

```go
// Package conformance verifies that an agent satisfies the runtime HTTP/SSE
// agent contract. It runs under `go test` (via *testing.T) and from the CLI
// (via a small adapter), exercising any agent reachable at a base URL.
package conformance

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// TestingT is the minimal subset of *testing.T the suite needs, so the same
// checks run under `go test` and from the runtimectl CLI.
type TestingT interface {
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
	Logf(format string, args ...any)
}

// Run executes all contract checks against the agent at baseURL. Failures are
// reported via t.Errorf (non-fatal, so all checks run); only an unrecoverable
// setup problem uses t.Fatalf.
func Run(t TestingT, baseURL string) {
	baseURL = strings.TrimRight(baseURL, "/")
	client := &http.Client{Timeout: 15 * time.Second}

	checkHealthz(t, client, baseURL)
	checkMeta(t, client, baseURL)
	sid := checkCreateSession(t, client, baseURL)
	if sid != "" {
		checkStream(t, client, baseURL, sid)
		checkGetSession(t, client, baseURL, sid)
	}
	checkListSessions(t, client, baseURL)
}

func checkHealthz(t TestingT, c *http.Client, base string) {
	resp, err := c.Get(base + "/healthz")
	if err != nil {
		t.Errorf("healthz: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("healthz: status %d, want 200", resp.StatusCode)
	} else {
		t.Logf("healthz: ok")
	}
}

func checkMeta(t TestingT, c *http.Client, base string) {
	resp, err := c.Get(base + "/meta")
	if err != nil {
		t.Errorf("meta: %v", err)
		return
	}
	defer resp.Body.Close()
	var m map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Errorf("meta: decode: %v", err)
		return
	}
	if m["agent_id"] == "" {
		t.Errorf("meta: missing agent_id")
	}
	if m["contract_version"] == "" {
		t.Errorf("meta: missing contract_version")
	}
	if m["agent_id"] != "" && m["contract_version"] != "" {
		t.Logf("meta: ok (contract %s)", m["contract_version"])
	}
}

func checkCreateSession(t TestingT, c *http.Client, base string) string {
	resp, err := c.Post(base+"/sessions", "application/json", strings.NewReader(`{"message":"conformance ping"}`))
	if err != nil {
		t.Errorf("create session: %v", err)
		return ""
	}
	defer resp.Body.Close()
	var out struct {
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Errorf("create session: decode: %v", err)
		return ""
	}
	if out.SessionID == "" {
		t.Errorf("create session: empty session_id")
		return ""
	}
	t.Logf("create session: ok (%s)", out.SessionID)
	return out.SessionID
}

func checkStream(t TestingT, c *http.Client, base, sid string) {
	resp, err := c.Get(base + "/sessions/" + sid + "/stream?since=0")
	if err != nil {
		t.Errorf("stream: %v", err)
		return
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("stream: content-type %q, want text/event-stream", ct)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if !strings.Contains(string(body), `"type":"done"`) {
		t.Errorf("stream: never saw terminal done event; got %q", string(body))
	} else {
		t.Logf("stream: ok")
	}
}

func checkGetSession(t TestingT, c *http.Client, base, sid string) {
	resp, err := c.Get(base + "/sessions/" + sid)
	if err != nil {
		t.Errorf("get session: %v", err)
		return
	}
	defer resp.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Errorf("get session: decode: %v", err)
		return
	}
	if _, ok := m["status"]; !ok {
		t.Errorf("get session: missing status")
	} else {
		t.Logf("get session: ok")
	}
}

func checkListSessions(t TestingT, c *http.Client, base string) {
	resp, err := c.Get(base + "/sessions")
	if err != nil {
		t.Errorf("list sessions: %v", err)
		return
	}
	defer resp.Body.Close()
	var rows []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		t.Errorf("list sessions: decode: %v", err)
		return
	}
	t.Logf("list sessions: ok (%d)", len(rows))
}

// CLIResult adapts Run for command-line use; see runtimectl.
var _ = fmt.Sprintf
```

- [ ] **Step 4:** `go test ./conformance/ -v` → PASS (good passes, broken fails).

- [ ] **Step 5: Add `RUNTIME_TOKEN` header + `conformance` command to `cmd/runtimectl/main.go`.** Modify the existing CLI: (a) a shared `do(req)` helper that sets `Authorization: Bearer $RUNTIME_TOKEN` when set and uses a default client; route all `http.Get`/`http.Post` through helpers that add the header; (b) add a `conformance` subcommand. The cleanest minimal change:

Add an authed client helper and use it everywhere:
```go
func authdGet(url string) (*http.Response, error) {
	req, _ := http.NewRequest("GET", url, nil)
	addAuth(req)
	return http.DefaultClient.Do(req)
}
func authdPost(url, ctype string, body io.Reader) (*http.Response, error) {
	req, _ := http.NewRequest("POST", url, body)
	req.Header.Set("Content-Type", ctype)
	addAuth(req)
	return http.DefaultClient.Do(req)
}
func addAuth(req *http.Request) {
	if tok := os.Getenv("RUNTIME_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
}
```
Replace the existing `http.Get(...)`/`http.Post(...)` calls in `fetchAgents`, `listSessions`, `stream`, `invoke` with `authdGet`/`authdPost` (add `io` import). Add the command:
```go
	case "conformance":
		runConformance(base, resolveAgent(base, agent))
```
And:
```go
// cliT adapts conformance.TestingT to stdout, tracking failure.
type cliT struct{ failed bool }

func (c *cliT) Errorf(f string, a ...any) { c.failed = true; fmt.Printf("FAIL: "+f+"\n", a...) }
func (c *cliT) Fatalf(f string, a ...any) { c.failed = true; fmt.Printf("FATAL: "+f+"\n", a...) }
func (c *cliT) Logf(f string, a ...any)   { fmt.Printf("ok: "+f+"\n", a...) }

func runConformance(base, agent string) {
	t := &cliT{}
	conformance.Run(t, base+"/agents/"+agent)
	if t.failed {
		fmt.Fprintln(os.Stderr, "conformance: FAILED")
		os.Exit(1)
	}
	fmt.Println("conformance: PASSED")
}
```
Add `"github.com/sausheong/runtime/conformance"` to imports and update the usage string to include `conformance`.

- [ ] **Step 6:** `go build ./... && go vet ./...` → clean; `go test ./...` → green.

- [ ] **Step 7: Commit.**

```bash
git add conformance/ cmd/runtimectl/main.go
git commit -m "feat: contract conformance suite + runtimectl conformance + bearer-token CLI"
```

---

## Task 5: Read-only web console

**Files:** `console/console.go`, `console/templates/*.html`, `console/static/*`, `console/console_test.go`, and the mount in `cmd/runtimed/main.go`.

- [ ] **Step 1: Failing console test** — `console/console_test.go`:

```go
package console

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sausheong/runtime/controlplane"
	"github.com/sausheong/runtime/internal/config"
)

func testReg() *controlplane.Registry {
	cfg := &config.Config{Agents: []config.AgentConfig{
		{ID: "support", Name: "Support", Model: "test/scripted", ListenAddr: "127.0.0.1:9001"},
	}}
	return controlplane.NewRegistry(cfg, "/bin/agentd", "dsn")
}

func TestConsole_Overview(t *testing.T) {
	h := Handler(testReg())
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/ui")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(body), "Support") {
		t.Fatalf("overview missing agent; code=%d body=%q", resp.StatusCode, body)
	}
}
```
(Add `"io"` import.) Note: `Handler(reg)` returns an `http.Handler` serving the `/ui*` routes; the agent-session data comes from the control-plane API the console is mounted beside, but the OVERVIEW page renders directly from the registry, so this test needs no live agents.

- [ ] **Step 2:** `go test ./console/` → FAIL.

- [ ] **Step 3: Implement** `console/console.go` with embedded templates:

```go
// Package console serves the read-only operator web UI.
package console

import (
	"embed"
	"html/template"
	"net/http"

	"github.com/sausheong/runtime/controlplane"
)

//go:embed templates/*.html static/*
var assets embed.FS

var tmpl = template.Must(template.ParseFS(assets, "templates/*.html"))

// Handler returns the console's HTTP handler, serving /ui (overview),
// /ui/agents/{id} (sessions), /ui/agents/{id}/sessions/{sid} (live view), and
// /ui/static/*. Read-only: it renders from the registry and links to the
// control-plane API + SSE endpoints the console is mounted beside.
func Handler(reg *controlplane.Registry) http.Handler {
	mux := http.NewServeMux()

	mux.Handle("GET /ui/static/", http.StripPrefix("/ui/", http.FileServerFS(assets)))

	mux.HandleFunc("GET /ui", func(w http.ResponseWriter, _ *http.Request) {
		render(w, "overview.html", map[string]any{"Agents": reg.List()})
	})

	mux.HandleFunc("GET /ui/agents/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if _, ok := reg.Get(id); !ok {
			http.NotFound(w, r)
			return
		}
		render(w, "agent.html", map[string]any{"AgentID": id})
	})

	mux.HandleFunc("GET /ui/agents/{id}/sessions/{sid}", func(w http.ResponseWriter, r *http.Request) {
		render(w, "session.html", map[string]any{
			"AgentID":   r.PathValue("id"),
			"SessionID": r.PathValue("sid"),
		})
	})

	return mux
}

func render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
```

- [ ] **Step 4: Templates.** Create `console/templates/overview.html`:

```html
<!doctype html><html><head><title>Runtime — Agents</title>
<link rel="stylesheet" href="/ui/static/style.css"></head><body>
<h1>Agents</h1>
<table><thead><tr><th>ID</th><th>Name</th><th>Model</th></tr></thead><tbody>
{{range .Agents}}
<tr><td><a href="/ui/agents/{{.ID}}">{{.ID}}</a></td><td>{{.Name}}</td><td>{{.Model}}</td></tr>
{{end}}
</tbody></table>
</body></html>
```
Create `console/templates/agent.html` (loads sessions via the API with JS):

```html
<!doctype html><html><head><title>Runtime — {{.AgentID}}</title>
<link rel="stylesheet" href="/ui/static/style.css"></head><body>
<p><a href="/ui">&larr; agents</a></p>
<h1>Sessions — {{.AgentID}}</h1>
<table id="sessions"><thead><tr><th>Session</th><th>Status</th><th>Turns</th></tr></thead><tbody></tbody></table>
<script>const AGENT={{.AgentID}};</script>
<script src="/ui/static/app.js"></script>
<script>loadSessions();</script>
</body></html>
```
Create `console/templates/session.html` (live SSE view):

```html
<!doctype html><html><head><title>Runtime — session</title>
<link rel="stylesheet" href="/ui/static/style.css"></head><body>
<p><a href="/ui/agents/{{.AgentID}}">&larr; sessions</a></p>
<h1>Session {{.SessionID}}</h1>
<pre id="events"></pre>
<script>const AGENT={{.AgentID}}, SID={{.SessionID}};</script>
<script src="/ui/static/app.js"></script>
<script>streamSession();</script>
</body></html>
```
NOTE: `{{.AgentID}}` etc. inside a `<script>` as a bare value — Go's `html/template` will JS-escape it correctly when the action is in a script context, BUT to be safe and explicit, wrap as a quoted string: `const AGENT={{.AgentID}};` actually emits the value JS-escaped; prefer `const AGENT="{{.AgentID}}";` — verify the rendered output is valid JS in the test or a manual check. Use the quoted form.

- [ ] **Step 5: Static assets.** Create `console/static/style.css` (minimal, readable):

```css
body{font:14px system-ui,sans-serif;margin:2rem;color:#222}
h1{font-size:1.3rem}
table{border-collapse:collapse;width:100%}
th,td{text-align:left;padding:.4rem .8rem;border-bottom:1px solid #eee}
a{color:#2a6;text-decoration:none}
pre{background:#f6f6f6;padding:1rem;border-radius:6px;white-space:pre-wrap}
```
Create `console/static/app.js`:

```javascript
async function loadSessions() {
  const res = await fetch(`/agents/${AGENT}/sessions`, {credentials: 'same-origin'});
  const rows = await res.json();
  const tb = document.querySelector('#sessions tbody');
  tb.innerHTML = '';
  (rows || []).forEach(s => {
    const tr = document.createElement('tr');
    tr.innerHTML = `<td><a href="/ui/agents/${AGENT}/sessions/${s.id}">${s.id}</a></td>`
      + `<td>${s.status}</td><td>${s.turn_count}</td>`;
    tb.appendChild(tr);
  });
}

function streamSession() {
  const out = document.getElementById('events');
  const es = new EventSource(`/agents/${AGENT}/sessions/${SID}/stream?since=0`, {withCredentials: true});
  es.onmessage = e => { out.textContent += e.data + "\n"; };
  es.onerror = () => { es.close(); };
}
```
NOTE: `EventSource` sends cookies with `withCredentials: true` (same-origin), so the auth middleware's cookie path authenticates the live stream. The console relies on the `runtime_token` cookie being set — see Step 6 (token entry).

- [ ] **Step 6: Token entry (only meaningful when auth is on).** Add a tiny `GET /ui/login` page + `POST /ui/login` that sets the `runtime_token` cookie, and exempt `/ui/login` + `/ui/static/` from auth. Two parts:
  (a) In `console/console.go` add:
```go
	mux.HandleFunc("GET /ui/login", func(w http.ResponseWriter, _ *http.Request) {
		render(w, "login.html", nil)
	})
	mux.HandleFunc("POST /ui/login", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		http.SetCookie(w, &http.Cookie{Name: "runtime_token", Value: r.FormValue("token"), Path: "/", HttpOnly: true})
		http.Redirect(w, r, "/ui", http.StatusSeeOther)
	})
```
  with `console/templates/login.html`:
```html
<!doctype html><html><head><title>Runtime — login</title>
<link rel="stylesheet" href="/ui/static/style.css"></head><body>
<h1>Enter API token</h1>
<form method="post" action="/ui/login">
<input name="token" type="password" placeholder="token" autofocus>
<button type="submit">Continue</button></form>
</body></html>
```
  (b) In `controlplane/auth.go`, extend the exempt check to also allow the login page + static assets (so a tokenless browser can reach them):
```go
		if len(tokens) == 0 || r.URL.Path == "/healthz" ||
			r.URL.Path == "/ui/login" || strings.HasPrefix(r.URL.Path, "/ui/static/") {
			next.ServeHTTP(w, r)
			return
		}
```
  And when auth IS on and a `/ui*` request arrives without a valid token, redirect to `/ui/login` instead of returning bare 401 (nicer UX). Refine the deny path:
```go
		tok := extractToken(r)
		label, ok := tokens[tok]
		if !ok {
			if strings.HasPrefix(r.URL.Path, "/ui") {
				http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
				return
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
```
  Update `auth_test.go` if needed for the redirect behavior on `/ui` paths (add a case: `/ui` without token → 303 to `/ui/login`). Keep API paths returning 401.

- [ ] **Step 7: Mount the console in `cmd/runtimed/main.go`.** Replace the `// NOTE (Task 5)` line with mounting the console routes onto the same mux BEFORE wrapping with auth. Since `console.Handler` returns its own mux, register it as a fallthrough: change the handler composition to route `/ui*` to the console and everything else to the API. Simplest: build a top-level mux:
```go
	apiMux := controlplane.NewAPI(reg)
	consoleH := console.Handler(reg)
	root := http.NewServeMux()
	root.Handle("/ui", consoleH)
	root.Handle("/ui/", consoleH)
	root.Handle("/", apiMux)
	handler := controlplane.AuthMiddleware(root, tokens)
```
Add the `console` import. (The console's internal mux already registers the specific `/ui...` patterns.)

- [ ] **Step 8: Verify** `go build ./... && go vet ./... && go test ./...` → green (config, auth incl. redirect, conformance, console, store, agentruntime, controlplane).

- [ ] **Step 9: Commit.**

```bash
git add console/ controlplane/auth.go controlplane/auth_test.go cmd/runtimed/main.go
git commit -m "feat(console): read-only web console (/ui) with token login + live SSE view"
```

---

## Task 6: /agents health field

**Files:** `controlplane/registry.go`, `controlplane/api.go`, tests.

- [ ] **Step 1:** Add a best-effort health probe surfaced in `GET /agents`. In `controlplane/api.go`, change the `GET /agents` handler to probe each agent's `/healthz` (short timeout, concurrent) and include a `healthy` bool per agent. Define a small response type rather than reusing `AgentInfo` directly:

```go
	mux.HandleFunc("GET /agents", func(w http.ResponseWriter, r *http.Request) {
		type agentStatus struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Model   string `json:"model"`
			Healthy bool   `json:"healthy"`
		}
		infos := reg.List()
		out := make([]agentStatus, len(infos))
		var wg sync.WaitGroup
		client := &http.Client{Timeout: 1 * time.Second}
		for i, info := range infos {
			out[i] = agentStatus{ID: info.ID, Name: info.Name, Model: info.Model}
			ap, _ := reg.Get(info.ID)
			wg.Add(1)
			go func(i int, addr string) {
				defer wg.Done()
				resp, err := client.Get("http://" + addr + "/healthz")
				if err == nil {
					out[i].Healthy = resp.StatusCode == 200
					resp.Body.Close()
				}
			}(i, ap.Addr)
		}
		wg.Wait()
		_ = json.NewEncoder(w).Encode(out)
	})
```
Add `"sync"` and `"time"` imports. Update the existing `TestRouter_DispatchAndList` assertion for `/agents` — it currently checks `"id":"a"` substring, which still holds (the field is still `id`); the fake backends respond 200 to `/healthz` so `healthy:true`. If the test's fake backends don't serve `/healthz`, `healthy` is just false — the `"id":"a"`/`"id":"b"` assertions still pass. Verify the router test still passes; adjust only if it asserted an exact JSON shape (it asserts substrings, so it's fine).

- [ ] **Step 2: Update the console overview** to show health. In `overview.html` add a Health column: `<td>{{if .Healthy}}●{{else}}○{{end}}</td>` — BUT `reg.List()` returns `[]AgentInfo` which has no Healthy field. Two options: (a) keep the console overview health-free (links only) — simplest, and the per-agent health is visible via the API/`runtimectl agents`; or (b) have the console call `GET /agents` via JS to fill health. For M3 scope, choose (a): leave the overview rendering from the registry without health, and document that health is available via `GET /agents` and the CLI. (Do NOT overbuild the console.) So this step is: no console change; just ensure the API field exists.

- [ ] **Step 3:** `go build ./... && go vet ./... && go test ./...` → green.

- [ ] **Step 4: Commit.**

```bash
git add controlplane/
git commit -m "feat(controlplane): per-agent health in GET /agents"
```

---

## Task 7: Full-stack Docker Compose + Dockerfile

**Files:** `deploy/Dockerfile`, `deploy/docker-compose.full.yml`.

- [ ] **Step 1: `deploy/Dockerfile`** (multi-stage; builds both binaries). NOTE: the runtime module depends on `../harness` via a replace directive, so the build context must include BOTH repos. Document this in the file header and set the compose build context accordingly.

```dockerfile
# Build context must be the PARENT directory containing both `runtime/` and
# `harness/` (the runtime module replaces github.com/sausheong/harness => ../harness).
# Build from the projects root: docker build -f runtime/deploy/Dockerfile .
FROM golang:1.25 AS build
WORKDIR /src
COPY harness/ ./harness/
COPY runtime/ ./runtime/
WORKDIR /src/runtime
RUN go build -o /out/runtimed ./cmd/runtimed && go build -o /out/agentd ./cmd/runtimed/../agentd

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y ca-certificates && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=build /out/runtimed /app/runtimed
COPY --from=build /out/agentd /app/agentd
COPY runtime/runtime.yaml /app/runtime.yaml
ENV RUNTIME_AGENTD_BIN=/app/agentd
EXPOSE 8080
CMD ["/app/runtimed"]
```
(Fix the agentd build path to `./cmd/agentd`.) Verify the build locally if Docker is available; if Docker isn't available in the environment, ensure the Dockerfile is correct by inspection and note it wasn't built.

- [ ] **Step 2: `deploy/docker-compose.full.yml`:**

```yaml
# Full stack: Postgres + control plane. Build from the projects root:
#   docker compose -f runtime/deploy/docker-compose.full.yml up --build
services:
  postgres:
    image: pgvector/pgvector:pg16
    environment:
      POSTGRES_USER: runtime
      POSTGRES_PASSWORD: runtime
      POSTGRES_DB: runtime
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U runtime"]
      interval: 2s
      timeout: 3s
      retries: 20
  runtimed:
    build:
      context: ../..            # projects root (contains runtime/ and harness/)
      dockerfile: runtime/deploy/Dockerfile
    depends_on:
      postgres:
        condition: service_healthy
    environment:
      RUNTIME_PG_DSN: postgres://runtime:runtime@postgres:5432/runtime?sslmode=disable
      RUNTIME_CTL_ADDR: ":8080"
    ports:
      - "8080:8080"
```

- [ ] **Step 3:** If Docker is available: `docker compose -f deploy/docker-compose.full.yml build` (or the documented root-context command) to confirm it builds. If not available, validate YAML/Dockerfile by inspection and note Docker wasn't run. Do NOT block the milestone on Docker availability.

- [ ] **Step 4: Commit.**

```bash
git add deploy/Dockerfile deploy/docker-compose.full.yml
git commit -m "feat(deploy): full-stack Dockerfile + docker-compose.full.yml"
```

---

## Task 8: Integration tests (auth, console, conformance)

**Files:** `test/operability_test.go` (`//go:build integration`).

- [ ] **Step 1: Write `test/operability_test.go`** with three tests reusing the package's existing helpers (`dsn`, `mustExec`, and the runtimed-spawning pattern from `multiagent_test.go` — reuse `waitURL`, `buildAgentd`-style builds; do NOT redeclare existing helpers). The tests:

1. **`TestAuthEnforced`** — write a config with ONE agent and a `tokens:` entry; start runtimed; assert `GET /agents` without a token → 401, with `Authorization: Bearer <token>` → 200, and `/healthz` without token → 200 (exempt).
2. **`TestConsoleOverview`** — same running stack (auth on); `GET /ui/login` (no token) → 200; `GET /ui` with the token cookie → 200 and body contains the agent id.
3. **`TestConformanceThroughControlPlane`** — with the stack up and a token, run `conformance.Run` against `base+"/agents/"+id` using an authed http client wrapper (set the cookie or header); assert zero failures. (Import the `conformance` package; pass a `*recorder`-style TestingT that fails the go test on any Errorf.)

Reuse the two-agent startup scaffold from `multiagent_test.go` but with a single agent + tokens. Key snippets:

```go
//go:build integration

package test

import (
	"context"
	"database/sql"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/sausheong/runtime/conformance"
)

func startRuntimed(t *testing.T, cfgBody string) (base string, stop func()) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil { t.Fatal(err) }
	mustExec(t, db, `DROP TABLE IF EXISTS session_events, sessions, agents, markers CASCADE`)
	mustExec(t, db, `DROP SCHEMA IF EXISTS dbos CASCADE`)
	db.Close()

	tmp := t.TempDir()
	agentd := filepath.Join(tmp, "agentd")
	if out, err := exec.Command("go", "build", "-o", agentd, "../cmd/agentd").CombinedOutput(); err != nil {
		t.Fatalf("build agentd: %v\n%s", err, out)
	}
	runtimed := filepath.Join(tmp, "runtimed")
	if out, err := exec.Command("go", "build", "-o", runtimed, "../cmd/runtimed").CombinedOutput(); err != nil {
		t.Fatalf("build runtimed: %v\n%s", err, out)
	}
	cfgPath := filepath.Join(tmp, "runtime.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0o644); err != nil { t.Fatal(err) }

	ctlAddr := "127.0.0.1:8230"
	cmd := exec.Command(runtimed)
	cmd.Env = append(os.Environ(),
		"RUNTIME_PG_DSN="+dsn, "RUNTIME_CTL_ADDR="+ctlAddr,
		"RUNTIME_AGENTD_BIN="+agentd, "RUNTIME_CONFIG="+cfgPath,
	)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	// Run runtimed in its own process group so we can reap it AND its agentd
	// grandchildren on teardown (otherwise orphaned children hold the test's
	// stdout pipe open and `go test` hangs). This mirrors multiagent_test.go,
	// which uses `&syscall.SysProcAttr{Setpgid: true}` + a negative-pid kill.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil { t.Fatal(err) }
	base = "http://" + ctlAddr
	waitURL(t, base+"/healthz", 15*time.Second)
	return base, func() {
		// negative pid → signal the whole group (runtimed + agentd children)
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	}
}
```
IMPORTANT: check `multiagent_test.go` for an existing process-group kill helper (`setpgid`/`SysProcAttr` + group-kill) and REUSE it (the M2 test added one to avoid orphaned grandchildren). If it exists with a different name, use that; do not redeclare. If the agent must be healthy through the router before conformance, also `waitURL(base+"/agents/<id>/healthz")` — but that path is auth-gated, so pass the token (use a cookie/header on the probe). For the auth-on tests, probe `/healthz` (exempt) for readiness and give the agent a couple seconds, or temporarily probe the agent's own addr.

Then the three tests:
```go
const oneAgentTokenCfg = `agents:
  - {id: solo, name: Solo, model: test/scripted, listen_addr: 127.0.0.1:8231}
tokens:
  - {token: "t0ken", label: "test"}
`

func TestAuthEnforced(t *testing.T) {
	base, stop := startRuntimed(t, oneAgentTokenCfg)
	defer stop()
	// no token -> 401
	resp, _ := http.Get(base + "/agents")
	if resp.StatusCode != 401 { t.Fatalf("no token: %d, want 401", resp.StatusCode) }
	resp.Body.Close()
	// healthz exempt
	resp, _ = http.Get(base + "/healthz")
	if resp.StatusCode != 200 { t.Fatalf("healthz: %d", resp.StatusCode) }
	resp.Body.Close()
	// with token -> 200
	req, _ := http.NewRequest("GET", base+"/agents", nil)
	req.Header.Set("Authorization", "Bearer t0ken")
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 200 { t.Fatalf("with token: %d, want 200", resp.StatusCode) }
	resp.Body.Close()
}

func TestConsoleOverview(t *testing.T) {
	base, stop := startRuntimed(t, oneAgentTokenCfg)
	defer stop()
	resp, _ := http.Get(base + "/ui/login")
	if resp.StatusCode != 200 { t.Fatalf("login page: %d", resp.StatusCode) }
	resp.Body.Close()
	req, _ := http.NewRequest("GET", base+"/ui", nil)
	req.AddCookie(&http.Cookie{Name: "runtime_token", Value: "t0ken"})
	resp, _ = http.DefaultClient.Do(req)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "solo") {
		t.Fatalf("overview: code=%d body=%q", resp.StatusCode, body)
	}
}

func TestConformanceThroughControlPlane(t *testing.T) {
	base, stop := startRuntimed(t, oneAgentTokenCfg)
	defer stop()
	// allow the agent a moment to be routable; the agent's contract is gated, so use a token-bearing client
	time.Sleep(2 * time.Second)
	tc := &tConf{t: t}
	// conformance.Run uses http.DefaultClient internally with no auth, so for the
	// gated path we run it against an authed transport: simplest is to set the
	// cookie via a custom client. Since Run builds its own client, instead point
	// it at the agent through the proxy with the token in a cookie is not
	// possible without Run support — so assert via a token header by running
	// against the agent path with a RoundTripper that injects the header.
	conformance.Run(tc, base+"/agents/solo")
	if tc.failed { t.Fatal("conformance failed through control plane") }
}

type tConf struct{ t *testing.T; failed bool }
func (c *tConf) Errorf(f string, a ...any) { c.failed = true; c.t.Logf("FAIL "+f, a...) }
func (c *tConf) Fatalf(f string, a ...any) { c.failed = true; c.t.Logf("FATAL "+f, a...) }
func (c *tConf) Logf(f string, a ...any)   { c.t.Logf(f, a...) }
```
**Conformance-through-auth wrinkle:** `conformance.Run` builds its own `http.Client` with no auth. To run it through the gated control plane, EITHER (a) for this test use the `oneAgentNoTokenCfg` (no tokens → open mode) so conformance needs no auth — RECOMMENDED, simplest, still exercises real routing; OR (b) extend `conformance.Run` to accept an optional `*http.Client`. Choose (a): add a second const `oneAgentNoTokenCfg` (same agent, no `tokens:`) and use it for `TestConformanceThroughControlPlane`, keeping `TestAuthEnforced`/`TestConsoleOverview` on the token config. Document this choice in a comment.

- [ ] **Step 2: Run** the integration tests + regressions:
```bash
go test -tags integration ./test/ -run 'TestAuthEnforced|TestConsoleOverview|TestConformanceThroughControlPlane' -v -count=1 -timeout 180s
go test -tags integration ./test/ -run 'TestResumeAfterKill|TestMultiAgentRouting' -count=1 -timeout 200s
```
Expected: all PASS.

- [ ] **Step 3:** Confirm hermetic `go test ./...` (no tag) stays green and excludes these.

- [ ] **Step 4: Commit.**

```bash
git add test/operability_test.go
git commit -m "test: auth, console, and conformance integration tests"
```

---

## Task 9: README + full verification

- [ ] **Step 1: Update `README.md`** — add: an **Authentication** section (configure `tokens:` in runtime.yaml; `RUNTIME_TOKEN` for the CLI; open mode when none; `/healthz` exempt); a **Web console** section (`/ui`, login with a token, the three views, read-only); a **Conformance** section (`runtimectl conformance --agent <id>` and the `conformance` Go package for agent authors); update **Deployment** with the full-stack Compose (`docker compose -f deploy/docker-compose.full.yml up --build`) and the build-context caveat; add `RUNTIME_TOKEN` and `RUNTIME_LOG_FORMAT` to the env table; move the now-done items (web console, auth) OUT of the limitations list. Keep remaining limitations (RBAC/Identity, observability dashboards, pools, dynamic deploy, write-from-console).

- [ ] **Step 2: Full hermetic verification:**
```bash
go build ./... && go vet ./... && go test ./...
```

- [ ] **Step 3: Full integration verification** (Postgres up):
```bash
go test -tags integration ./test/ -count=1 -timeout 240s
```
Expected: M1 resume, M2 multi-agent, and the M3 auth/console/conformance tests all PASS.

- [ ] **Step 4: Commit.**

```bash
git add README.md
git commit -m "docs: M3 operability — auth, console, conformance, full-stack deploy"
```

---

## Definition of Done (Milestone 3)

- [ ] Control plane enforces bearer-token auth when tokens are configured (header or cookie); open mode with a warning when not; `/healthz` exempt.
- [ ] `runtimectl` sends `RUNTIME_TOKEN`; `runtimectl conformance --agent <id>` runs the contract suite and exits non-zero on failure.
- [ ] Read-only web console at `/ui`: agents overview → sessions → live SSE session view; token login via cookie.
- [ ] `conformance` package runs under `go test` against any agent (good passes, broken fails).
- [ ] Structured `slog` logging across runtimed/controlplane/agentruntime; bounded shutdown; 503 on agent-unavailable; per-agent health in `GET /agents`.
- [ ] Full-stack Dockerfile + docker-compose.full.yml.
- [ ] All hermetic tests + vet green; integration tests (incl. M1/M2 regressions) green.
