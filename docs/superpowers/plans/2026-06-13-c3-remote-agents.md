# C3 — Remote Agents (attach instead of spawn) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let `runtimed` attach to an already-running remote `agentd` (health-check + reverse-proxy + status, no spawn/supervise) via an `agents[].url` config variant, secured by an opt-in shared bearer token.

**Architecture:** The data plane is already address-agnostic — the control plane reverse-proxies HTTP to an agent address and health-checks `GET /healthz`. This plan upgrades the dial identity from a bare `host:port` (`Addr`) to a full base URL (`BaseURL`) plus an optional bearer token (`AuthToken`), threads that through the four dial sites (reverse proxy, `/agents` health, metrics fan-out, startup gate), replaces the local `Supervisor` with a non-restarting `HealthMonitor` for remote agents, and adds optional bearer-auth middleware to `agentd`.

**Tech Stack:** Go 1.25, `net/http/httputil.ReverseProxy`, `net/url`, `crypto/subtle`, Prometheus (`internal/obs`), DBOS-backed agentd, Postgres (integration tests via Postgres.app).

**Spec:** `docs/superpowers/specs/2026-06-13-c3-remote-agents-design.md`

**Conventions (read before starting):**
- The `go` CLI is ground truth; ignore IDE/LSP diagnostics (the `replace github.com/sausheong/harness => ../harness` cross-module setup confuses them).
- Hermetic unit tests run with `go test ./...`. Integration tests use `//go:build integration` and need Postgres at `postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable` (local Postgres.app). They self-clean their DB + the `dbos` schema.
- Run `go build ./...` and `go vet ./...` before each commit.
- No secrets committed or echoed; tokens come from env, referenced as `${VAR}` in YAML.
- Commit after each task with the message shown in its final step.

---

## File Structure

**Modified:**
- `internal/config/config.go` — add `URL` + `AuthToken` fields to `AgentConfig`; remote/local validation; unified dial-identity uniqueness; `auth_token` env-expansion.
- `controlplane/proxy.go` — add `Remote`/`BaseURL`/`AuthToken` to `AgentProcess`; `baseURL()` helper; `authTransport`; change `reverseProxy` signature to take `(base, token, onError)`.
- `controlplane/registry.go` — populate the new `AgentProcess` fields from config (local vs remote).
- `controlplane/api.go` — `/agents` health check + `/agents/{id}/` proxy dial via `baseURL()` + token.
- `internal/obs/fanout.go` — `ScrapeTarget` gains `BaseURL` + `Token`; `scrapeOne` dials the base URL with the bearer.
- `internal/obs/obs.go` — add `agentReachable` gauge + `AgentReachable(agent, bool)` method.
- `cmd/runtimed/main.go` — split startup loop: `Supervisor` for local, `HealthMonitor` for remote; build `ScrapeTarget` with `BaseURL`/`Token`.
- `cmd/agentd/main.go` — read `RUNTIME_AGENT_AUTH_TOKEN`, pass to `agentruntime.Serve` via an option.
- `agentruntime/serve.go` — read auth token from env, set on `Manager`.
- `agentruntime/server.go` — `handler()` wraps the stack with `requireBearer` when the token is set.
- `controlplane/proxy_test.go` — update existing `reverseProxy(...)` call sites to the new signature.

**Created:**
- `controlplane/monitor.go` — `HealthMonitor` (poll loop, edge-triggered `OnChange`, no restart).
- `controlplane/monitor_test.go` — monitor unit tests.
- `test/remote_agent_test.go` — `//go:build integration` end-to-end remote-attach test.

---

## Task 1: Config schema + validation for remote agents

**Files:**
- Modify: `internal/config/config.go` (`AgentConfig` struct ~line 14-28; `Validate` ~line 149-244)
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `internal/config/config_test.go`:

```go
func TestLoad_RemoteAgentURL(t *testing.T) {
	t.Setenv("REMOTE_TOK", "shhh")
	p := writeTmp(t, `
agents:
  - id: local-1
    name: Local
    model: test/scripted
    listen_addr: 127.0.0.1:8101
  - id: remote-1
    name: Remote
    model: test/scripted
    url: https://agent-1.internal:8443
    auth_token: ${REMOTE_TOK}
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agents[1].URL != "https://agent-1.internal:8443" {
		t.Fatalf("url = %q", cfg.Agents[1].URL)
	}
	if cfg.Agents[1].AuthToken != "shhh" {
		t.Fatalf("auth_token not expanded: %q", cfg.Agents[1].AuthToken)
	}
}

func TestValidate_RemoteRejectsBadCombos(t *testing.T) {
	cases := map[string]string{
		"neither addr nor url": `
agents:
  - {id: a, name: A, model: m}
`,
		"both addr and url": `
agents:
  - {id: a, name: A, model: m, listen_addr: 127.0.0.1:8101, url: http://h:1}
`,
		"bad scheme": `
agents:
  - {id: a, name: A, model: m, url: ftp://h:1}
`,
		"no host": `
agents:
  - {id: a, name: A, model: m, url: "https://"}
`,
		"command on remote": `
agents:
  - {id: a, name: A, model: m, url: http://h:1, command: [x]}
`,
		"kind on remote": `
agents:
  - {id: a, name: A, model: m, url: http://h:1, kind: special}
`,
		"memory on remote": `
agents:
  - {id: a, name: A, model: m, url: http://h:1, memory: true}
`,
		"gateway on remote": `
agents:
  - {id: a, name: A, model: m, url: http://h:1, gateway: true}
gateway:
  servers:
    - {name: fs, command: x}
`,
		"auth_token without url": `
agents:
  - {id: a, name: A, model: m, listen_addr: 127.0.0.1:8101, auth_token: tok}
`,
		"duplicate url": `
agents:
  - {id: a, name: A, model: m, url: http://h:1}
  - {id: b, name: B, model: m, url: http://h:1}
`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			p := writeTmp(t, body)
			if _, err := Load(p); err == nil {
				t.Fatalf("%s: expected validation error, got nil", name)
			}
		})
	}
}

func TestValidate_AuthTokenUnsetEnvFailsClosed(t *testing.T) {
	os.Unsetenv("DEFINITELY_UNSET_TOK")
	p := writeTmp(t, `
agents:
  - {id: a, name: A, model: m, url: http://h:1, auth_token: "${DEFINITELY_UNSET_TOK}"}
`)
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for unset env var in auth_token")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/config/ -run 'Remote|AuthToken' -v`
Expected: FAIL — `URL`/`AuthToken` fields don't exist (compile error), then validation failures once fields are added.

- [ ] **Step 3: Add the struct fields**

In `internal/config/config.go`, inside `AgentConfig` (after the `Memory` field, before the `Gateway` field):

```go
	// URL marks a REMOTE agent: runtimed attaches (health-check + proxy +
	// status) instead of spawning. Full base, e.g. "https://host:8443".
	// Mutually exclusive with ListenAddr — exactly one is required.
	URL string `yaml:"url"`
	// AuthToken is an optional shared bearer for the runtimed→remote-agent hop;
	// ${VAR}-expanded at load. Only valid with URL.
	AuthToken string `yaml:"auth_token"`
```

- [ ] **Step 4: Add validation + uniqueness + expansion**

In `internal/config/config.go` `Validate`, replace the per-agent block (currently lines ~155-171, from `for i := range c.Agents {` through the closing `}` that sets `addrs[a.ListenAddr] = true`) with:

```go
	ids := map[string]bool{}
	dials := map[string]bool{} // unified: listen_addr OR url must be unique
	for i := range c.Agents {
		a := &c.Agents[i]
		if a.ID == "" || a.Name == "" || a.Model == "" {
			return fmt.Errorf("config: agent[%d] requires id, name, model", i)
		}
		// Exactly one of listen_addr / url.
		if (a.ListenAddr == "") == (a.URL == "") {
			return fmt.Errorf("config: agent %q requires exactly one of listen_addr (local) or url (remote)", a.ID)
		}
		remote := a.URL != ""
		if remote {
			u, err := url.Parse(a.URL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return fmt.Errorf("config: agent %q url must be http(s)://host[:port] (got %q)", a.ID, a.URL)
			}
			// Local-only fields can't be delivered to a process we don't spawn.
			if len(a.Command) > 0 || a.WorkDir != "" || a.Kind != "" || a.Memory || a.Gateway.Enabled() {
				return fmt.Errorf("config: remote agent %q must not set command, workdir, kind, memory, or gateway (these are spawn-time only)", a.ID)
			}
			if err := expandEnvScalar(&a.AuthToken, "agent "+a.ID+" auth_token"); err != nil {
				return err
			}
		} else if a.AuthToken != "" {
			return fmt.Errorf("config: agent %q auth_token is only valid with url (remote agents)", a.ID)
		}
		if a.Tenant == "" {
			a.Tenant = "default"
		}
		if ids[a.ID] {
			return fmt.Errorf("config: duplicate agent id %q", a.ID)
		}
		dial := a.ListenAddr
		if remote {
			dial = a.URL
		}
		if dials[dial] {
			return fmt.Errorf("config: duplicate agent dial address %q", dial)
		}
		ids[a.ID] = true
		dials[dial] = true
	}
```

Add `"net/url"` to the import block in `internal/config/config.go`.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/config/ -v`
Expected: PASS (all existing + new tests). Also run `go build ./...` (expect success).

- [ ] **Step 6: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(c3): remote-agent config schema (url + auth_token) with fail-closed validation"
```

---

## Task 2: `AgentProcess` dial identity + `authTransport` + `reverseProxy` signature

**Files:**
- Modify: `controlplane/proxy.go` (`AgentProcess` struct ~line 18-37; `reverseProxy` ~line 122-140)
- Modify: `controlplane/proxy_test.go` (existing `reverseProxy(...)` call sites at lines 21 and 45)
- Test: `controlplane/proxy_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `controlplane/proxy_test.go`:

```go
func TestAuthTransport_AddsBearerWhenSet(t *testing.T) {
	var gotAuth string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	req := httptest.NewRequest("GET", backend.URL, nil)
	at := authTransport{token: "sekret"}
	resp, err := at.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotAuth != "Bearer sekret" {
		t.Fatalf("Authorization = %q, want %q", gotAuth, "Bearer sekret")
	}
	// The caller's request must NOT be mutated (clone semantics).
	if req.Header.Get("Authorization") != "" {
		t.Fatal("authTransport leaked header onto caller's request")
	}
}

func TestAuthTransport_NoBearerWhenEmpty(t *testing.T) {
	var gotAuth string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	resp, err := authTransport{}.RoundTrip(httptest.NewRequest("GET", backend.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotAuth != "" {
		t.Fatalf("Authorization = %q, want empty", gotAuth)
	}
}

func TestBaseURL_LocalFallbackAndRemote(t *testing.T) {
	local := AgentProcess{Addr: "127.0.0.1:8101"}
	if local.baseURL() != "http://127.0.0.1:8101" {
		t.Fatalf("local baseURL = %q", local.baseURL())
	}
	remote := AgentProcess{BaseURL: "https://h:8443"}
	if remote.baseURL() != "https://h:8443" {
		t.Fatalf("remote baseURL = %q", remote.baseURL())
	}
}

func TestReverseProxy_SendsBearer(t *testing.T) {
	var gotAuth string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	rp := reverseProxy(backend.URL, "tok-123", nil)
	rec := httptest.NewRecorder()
	rp.ServeHTTP(rec, httptest.NewRequest("GET", "/sessions", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if gotAuth != "Bearer tok-123" {
		t.Fatalf("backend saw Authorization = %q", gotAuth)
	}
}
```

- [ ] **Step 2: Update the EXISTING call sites so the package compiles**

The signature changes from `reverseProxy(addr string, ...)` to `reverseProxy(base, token string, ...)`. The two existing tests pass a bare `host:port`; update them to pass a full base URL and an empty token:

In `controlplane/proxy_test.go` line ~21, change:
```go
	rp := reverseProxy("127.0.0.1:1", nil)
```
to:
```go
	rp := reverseProxy("http://127.0.0.1:1", "", nil)
```

In `controlplane/proxy_test.go` line ~45, change:
```go
	rp := reverseProxy(strings.TrimPrefix(backend.URL, "http://"), nil)
```
to:
```go
	rp := reverseProxy(backend.URL, "", nil)
```
(`backend.URL` is already a full `http://host:port`; the `strings` import may become unused — if `go vet`/build complains, remove `"strings"` from the imports only if no other test in the file uses it. Note `TestSpawnFuncCommand` uses `strings.Contains`, so keep it.)

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test ./controlplane/ -run 'AuthTransport|BaseURL|ReverseProxy' -v`
Expected: FAIL to compile — `authTransport`, `baseURL`, and the new `reverseProxy` signature don't exist yet.

- [ ] **Step 4: Implement the struct fields, helper, transport, and new signature**

In `controlplane/proxy.go`, add to the `AgentProcess` struct (after the `Memory` field):

```go
	// Remote marks an attach-only agent: no spawn, no Supervisor — runtimed
	// health-checks, proxies, and reports status, but never restarts it.
	Remote bool
	// BaseURL is the full dial base "scheme://host:port". For local agents it
	// is synthesized as "http://"+Addr; for remote agents it is the config url.
	BaseURL string
	// AuthToken is an optional shared bearer added to every request runtimed
	// makes to this agent (proxy, health, metrics). "" ⇒ no auth header.
	AuthToken string
```

Add the helper and transport (after the `AgentProcess` struct / `buildEnv`, anywhere in the file — put them just above `reverseProxy`):

```go
// baseURL returns the full dial base for the agent. Local agents (set only via
// Addr) fall back to http://Addr; remote agents carry an explicit BaseURL.
func (a AgentProcess) baseURL() string {
	if a.BaseURL != "" {
		return a.BaseURL
	}
	return "http://" + a.Addr
}

// authTransport adds a bearer token to every request. token=="" ⇒ pass through
// unchanged. The request is cloned so the caller's *http.Request is never
// mutated (the ReverseProxy reuses its outgoing request object).
type authTransport struct {
	token string
	base  http.RoundTripper // nil ⇒ http.DefaultTransport
}

func (t authTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	if t.token != "" {
		r = r.Clone(r.Context())
		r.Header.Set("Authorization", "Bearer "+t.token)
	}
	return base.RoundTrip(r)
}
```

Change `reverseProxy` (replace the whole function) to take a base URL + token:

```go
// reverseProxy builds a passthrough to the agent at base ("scheme://host:port").
// When token != "", every forwarded request carries an Authorization: Bearer
// header (remote agents). FlushInterval = -1 keeps SSE/streaming prompt.
// onError (nil ⇒ no-op) fires before each 503 served by the ErrorHandler.
func reverseProxy(base, token string, onError func()) *httputil.ReverseProxy {
	target, _ := url.Parse(base)
	rp := httputil.NewSingleHostReverseProxy(target)
	rp.Transport = authTransport{token: token}
	rp.FlushInterval = -1
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, _ error) {
		if onError != nil && r.Context().Err() == nil {
			onError()
		}
		http.Error(w, "agent unavailable", http.StatusServiceUnavailable)
	}
	rp.ModifyResponse = func(resp *http.Response) error {
		resp.Header.Del("X-Request-Id")
		return nil
	}
	return rp
}
```

(`net/url` and `net/http` are already imported in `proxy.go`.)

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./controlplane/ -v`
Expected: PASS (new + existing, including `TestReverseProxy_503OnDeadBackend` and `TestReverseProxy_DedupesRequestIDEcho`). Then `go build ./...` and `go vet ./...` (expect success).

- [ ] **Step 6: Commit**

```bash
git add controlplane/proxy.go controlplane/proxy_test.go
git commit -m "feat(c3): AgentProcess dial identity (BaseURL/AuthToken), authTransport, reverseProxy base+token"
```

---

## Task 3: Registry populates dial identity from config

**Files:**
- Modify: `controlplane/registry.go` (`NewRegistry` ~line 25-38)
- Test: `controlplane/registry_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `controlplane/registry_test.go`:

```go
func TestRegistry_RemoteAgentDialIdentity(t *testing.T) {
	cfg := &config.Config{Agents: []config.AgentConfig{
		{ID: "local", Name: "L", Model: "m", ListenAddr: "127.0.0.1:8101"},
		{ID: "remote", Name: "R", Model: "m", URL: "https://h:8443", AuthToken: "tok"},
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(cfg, "/bin/agentd", "dsn")

	l, _ := reg.Get("local")
	if l.Remote {
		t.Fatal("local agent marked Remote")
	}
	if l.baseURL() != "http://127.0.0.1:8101" || l.Addr != "127.0.0.1:8101" {
		t.Fatalf("local dial wrong: base=%q addr=%q", l.baseURL(), l.Addr)
	}

	r, _ := reg.Get("remote")
	if !r.Remote {
		t.Fatal("remote agent not marked Remote")
	}
	if r.baseURL() != "https://h:8443" {
		t.Fatalf("remote baseURL = %q", r.baseURL())
	}
	if r.AuthToken != "tok" {
		t.Fatalf("remote AuthToken = %q", r.AuthToken)
	}
	if r.Addr != "" {
		t.Fatalf("remote Addr should be empty, got %q", r.Addr)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./controlplane/ -run RemoteAgentDialIdentity -v`
Expected: FAIL — registry does not set `Remote`/`BaseURL`/`AuthToken`.

- [ ] **Step 3: Populate the new fields**

In `controlplane/registry.go` `NewRegistry`, replace the `for _, a := range cfg.Agents {` loop body's `r.agents[a.ID] = AgentProcess{...}` assignment with a local/remote split:

```go
	for _, a := range cfg.Agents {
		r.order = append(r.order, a.ID)
		ap := AgentProcess{
			AgentID: a.ID, BinPath: binPath, PGDSN: dsn,
			Kind: a.Kind, Command: a.Command, WorkDir: a.WorkDir, Tenant: a.Tenant,
			Memory: a.Memory, GatewayOn: a.Gateway.Enabled(),
			GatewaySearch: a.Gateway == config.GatewaySearch,
		}
		if a.URL != "" {
			ap.Remote = true
			ap.BaseURL = a.URL
			ap.AuthToken = a.AuthToken
		} else {
			ap.Addr = a.ListenAddr
			ap.BaseURL = "http://" + a.ListenAddr
		}
		r.agents[a.ID] = ap
		r.infos[a.ID] = AgentInfo{ID: a.ID, Name: a.Name, Model: a.Model, Tenant: a.Tenant}
	}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./controlplane/ -v`
Expected: PASS (new + all existing registry tests). Then `go build ./...`.

- [ ] **Step 5: Commit**

```bash
git add controlplane/registry.go controlplane/registry_test.go
git commit -m "feat(c3): registry wires local vs remote dial identity from config"
```

---

## Task 4: `/agents` health + `/agents/{id}/` proxy dial via baseURL + token

**Files:**
- Modify: `controlplane/api.go` (`GET /agents` handler ~line 24-64; `/agents/{id}/` handler ~line 70-84)
- Test: `controlplane/api_test.go` (the file `router_test.go` exists; add a focused test in a new `api_dial_test.go` to avoid churn)
- Create: `controlplane/api_dial_test.go`

- [ ] **Step 1: Write the failing test**

Create `controlplane/api_dial_test.go`:

```go
package controlplane

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sausheong/runtime/internal/config"
)

// A remote agent behind a bearer: /agents must dial its base URL WITH the
// token and report it healthy; a proxied request must reach it.
func TestAPI_RemoteAgentHealthAndProxyUseBearer(t *testing.T) {
	const token = "abc123"
	var sawHealthAuth, sawProxyAuth string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/healthz":
			sawHealthAuth = r.Header.Get("Authorization")
			w.WriteHeader(http.StatusOK)
		default:
			sawProxyAuth = r.Header.Get("Authorization")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		}
	}))
	defer backend.Close()

	cfg := &config.Config{Agents: []config.AgentConfig{
		{ID: "r", Name: "R", Model: "m", URL: backend.URL, AuthToken: token},
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(cfg, "bin", "dsn")
	api := NewAPI(reg, nil)

	// /agents reports healthy.
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, httptest.NewRequest("GET", "/agents", nil))
	var statuses []struct {
		ID      string `json:"id"`
		Healthy bool   `json:"healthy"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &statuses); err != nil {
		t.Fatalf("decode /agents: %v (%s)", err, rec.Body.String())
	}
	if len(statuses) != 1 || !statuses[0].Healthy {
		t.Fatalf("/agents = %+v, want one healthy", statuses)
	}
	if sawHealthAuth != "Bearer "+token {
		t.Fatalf("health check Authorization = %q", sawHealthAuth)
	}

	// Proxy a request through.
	rec = httptest.NewRecorder()
	api.ServeHTTP(rec, httptest.NewRequest("GET", "/agents/r/sessions", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ok") {
		t.Fatalf("proxy code=%d body=%q", rec.Code, rec.Body.String())
	}
	if sawProxyAuth != "Bearer "+token {
		t.Fatalf("proxy Authorization = %q", sawProxyAuth)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./controlplane/ -run RemoteAgentHealthAndProxyUseBearer -v`
Expected: FAIL — the health check dials `http://`+`ap.Addr` (empty for remote ⇒ `http:///healthz`) and the proxy uses the old signature.

- [ ] **Step 3: Update the `/agents` health check**

In `controlplane/api.go`, inside the `GET /agents` handler, the goroutine currently builds `st` and dials `"http://" + addr + "/healthz"`. Replace the per-agent dispatch so it captures the full `AgentProcess` and uses base URL + token. Change the loop body (the `for _, info := range infos {` block) to:

```go
		for _, info := range infos {
			if hasP && !p.Superuser && info.Tenant != p.TenantID {
				continue
			}
			ap, _ := reg.Get(info.ID)
			wg.Add(1)
			go func(info AgentInfo, ap AgentProcess) {
				defer wg.Done()
				st := agentStatus{ID: info.ID, Name: info.Name, Model: info.Model}
				req, _ := http.NewRequest("GET", ap.baseURL()+"/healthz", nil)
				if ap.AuthToken != "" {
					req.Header.Set("Authorization", "Bearer "+ap.AuthToken)
				}
				resp, err := client.Do(req)
				if err == nil {
					st.Healthy = resp.StatusCode == 200
					resp.Body.Close()
				}
				mu.Lock()
				out = append(out, st)
				mu.Unlock()
			}(info, ap)
		}
```

- [ ] **Step 4: Update the proxy handler**

In `controlplane/api.go`, the `/agents/{id}/` handler's last line currently reads:

```go
		reverseProxy(ap.Addr, func() { m.ProxyError(ap.AgentID) }).ServeHTTP(w, r)
```

Replace with:

```go
		reverseProxy(ap.baseURL(), ap.AuthToken, func() { m.ProxyError(ap.AgentID) }).ServeHTTP(w, r)
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./controlplane/ -v`
Expected: PASS (new + all existing router/api tests). Then `go build ./...` and `go vet ./...`.

- [ ] **Step 6: Commit**

```bash
git add controlplane/api.go controlplane/api_dial_test.go
git commit -m "feat(c3): /agents health + proxy dial remote agents via baseURL + bearer"
```

---

## Task 5: Metrics fan-out scrapes remote agents (BaseURL + token)

**Files:**
- Modify: `internal/obs/fanout.go` (`ScrapeTarget` ~line 20-24; `scrapeOne` ~line 172-198)
- Modify: `internal/obs/fanout_test.go` (existing `ScrapeTarget{Agent, Addr}` literals at lines ~53 and elsewhere)
- Test: `internal/obs/fanout_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/obs/fanout_test.go`:

```go
func TestFanout_RemoteTargetUsesBaseURLAndToken(t *testing.T) {
	const token = "scrape-tok"
	var sawAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		sawAuth = r.Header.Get("Authorization")
		fmt.Fprint(w, exposition("remote"))
	}))
	defer srv.Close()

	c := NewControlMetrics()
	h := FanoutHandler(c, func() []ScrapeTarget {
		return []ScrapeTarget{{Agent: "remote", BaseURL: srv.URL, Token: token}}
	})
	body := scrapeHandler(t, h)
	mustParseClean(t, body)
	if !strings.Contains(body, `agent_turns_total{agent="remote",outcome="completed"} 3`) {
		t.Fatalf("remote series missing:\n%s", body)
	}
	if sawAuth != "Bearer "+token {
		t.Fatalf("scrape Authorization = %q", sawAuth)
	}
}
```

- [ ] **Step 2: Update existing `ScrapeTarget` literals in the test file**

The existing tests construct `ScrapeTarget{Agent: "alpha", Addr: a1}`. Since `fakeAgent` returns a bare `host:port`, change those literals to use `BaseURL` with the `http://` scheme. In `internal/obs/fanout_test.go`, update each `ScrapeTarget{Agent: X, Addr: Y}` to `ScrapeTarget{Agent: X, BaseURL: "http://" + Y}`. (Search the file for `Addr:` within `ScrapeTarget{` — there are several across `TestFanoutMergesHealthyAgents`, `TestFanoutSkipsHangingAgent`, and others. Update all of them.)

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./internal/obs/ -run Fanout -v`
Expected: FAIL to compile — `ScrapeTarget` has no `BaseURL`/`Token` field.

- [ ] **Step 4: Update `ScrapeTarget` and `scrapeOne`**

In `internal/obs/fanout.go`, replace the `ScrapeTarget` struct:

```go
// ScrapeTarget is one agent's metrics endpoint. BaseURL is the full dial base
// "scheme://host:port"; Token (optional) is the shared bearer for remote agents.
type ScrapeTarget struct {
	Agent   string // agent id (used for up/skip series labels)
	BaseURL string // full base, e.g. "http://127.0.0.1:8101" or "https://h:8443"
	Token   string // optional bearer ("" ⇒ no auth header)
}
```

In `scrapeOne`, change the request construction (currently `http.NewRequestWithContext(ctx, "GET", "http://"+tgt.Addr+"/metrics", nil)`):

```go
	req, err := http.NewRequestWithContext(ctx, "GET", tgt.BaseURL+"/metrics", nil)
	if err != nil {
		return nil, false, "error"
	}
	if tgt.Token != "" {
		req.Header.Set("Authorization", "Bearer "+tgt.Token)
	}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/obs/ -v`
Expected: PASS (new + all existing fan-out tests). Then `go build ./...`.

- [ ] **Step 6: Commit**

```bash
git add internal/obs/fanout.go internal/obs/fanout_test.go
git commit -m "feat(c3): metrics fan-out scrapes agents via BaseURL + optional bearer"
```

---

## Task 6: `AgentReachable` metric + `HealthMonitor`

**Files:**
- Modify: `internal/obs/obs.go` (`ControlMetrics` struct ~line 30-41; `NewControlMetrics` ~line 43-89; add method near `AgentUp` ~line 113)
- Create: `controlplane/monitor.go`
- Create: `controlplane/monitor_test.go`
- Test: `controlplane/monitor_test.go`

- [ ] **Step 1: Write the failing tests**

Create `controlplane/monitor_test.go`:

```go
package controlplane

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The monitor flips reachable→unreachable and fires OnChange only on
// transitions (edge-triggered), and sends the bearer on its probe.
func TestHealthMonitor_TransitionsAndBearer(t *testing.T) {
	var up atomic.Bool
	up.Store(true)
	var sawAuth atomic.Value
	sawAuth.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth.Store(r.Header.Get("Authorization"))
		if up.Load() {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()

	var mu sync.Mutex
	var changes []bool
	hm := &HealthMonitor{
		BaseURL:  srv.URL,
		Token:    "mon-tok",
		Interval: 10 * time.Millisecond,
		OnChange: func(ok bool) { mu.Lock(); changes = append(changes, ok); mu.Unlock() },
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hm.Run(ctx)

	// First transition: unknown→reachable (true).
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(changes) >= 1 && changes[0] })

	up.Store(false) // flip down
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(changes) >= 2 && !changes[len(changes)-1]
	})

	mu.Lock()
	n := len(changes)
	mu.Unlock()
	if n > 5 {
		t.Fatalf("OnChange fired %d times — not edge-triggered (should fire only on transitions)", n)
	}
	if a := sawAuth.Load().(string); a != "Bearer mon-tok" {
		t.Fatalf("probe Authorization = %q", a)
	}
}

func TestHealthMonitor_StopsOnCtxCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	hm := &HealthMonitor{BaseURL: srv.URL, Interval: 5 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { hm.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./controlplane/ -run HealthMonitor -v`
Expected: FAIL to compile — `HealthMonitor` does not exist.

- [ ] **Step 3: Implement `HealthMonitor`**

Create `controlplane/monitor.go`:

```go
package controlplane

import (
	"context"
	"net/http"
	"time"
)

// HealthMonitor polls a remote agent's /healthz until ctx is cancelled,
// reporting reachability transitions via OnChange (edge-triggered). It NEVER
// restarts the agent — runtimed does not own a remote process; it only
// observes. This is the remote-agent counterpart to Supervisor.
type HealthMonitor struct {
	BaseURL  string                // full base "scheme://host:port"
	Token    string                // optional bearer ("" ⇒ none)
	Interval time.Duration         // poll period (default 10s)
	OnChange func(reachable bool)  // fired only when reachability flips; nil ⇒ no-op
}

// Run polls until ctx is cancelled. The first observation always fires OnChange
// (unknown→reachable or unknown→unreachable).
func (h *HealthMonitor) Run(ctx context.Context) {
	interval := h.Interval
	if interval == 0 {
		interval = 10 * time.Second
	}
	client := &http.Client{Timeout: 2 * time.Second}
	var last int // 0=unknown, 1=reachable, -1=unreachable
	probe := func() {
		ok := h.healthy(ctx, client)
		cur := -1
		if ok {
			cur = 1
		}
		if cur != last {
			last = cur
			if h.OnChange != nil {
				h.OnChange(ok)
			}
		}
	}
	probe() // immediate first check, no initial Interval delay
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			probe()
		}
	}
}

// healthy reports whether GET BaseURL+"/healthz" returns 200.
func (h *HealthMonitor) healthy(ctx context.Context, client *http.Client) bool {
	req, err := http.NewRequestWithContext(ctx, "GET", h.BaseURL+"/healthz", nil)
	if err != nil {
		return false
	}
	if h.Token != "" {
		req.Header.Set("Authorization", "Bearer "+h.Token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
```

- [ ] **Step 4: Add the `AgentReachable` metric**

In `internal/obs/obs.go`, add a field to `ControlMetrics` (after `agentUp`):

```go
	agentReachable *prometheus.GaugeVec
```

In `NewControlMetrics`, register it (after the `agentUp` block, before `agentRestarts`):

```go
	c.agentReachable = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "runtime_agent_reachable",
		Help: "1 when a remote agent's /healthz was reachable on the last monitor poll (remote agents only).",
	}, []string{"agent"})
```

Add it to the `c.reg.MustRegister(...)` call (append `c.agentReachable`):

```go
	c.reg.MustRegister(c.httpRequests, c.httpDuration, c.agentUp, c.agentReachable, c.agentRestarts,
		c.proxyErrors, c.gwCalls, c.gwDuration, c.gwUp, c.scrapeSkips)
```

Add the method (after `AgentUp`):

```go
// AgentReachable sets the remote-agent reachability gauge (1/0) on each
// HealthMonitor transition. Nil-safe like the other helpers.
func (c *ControlMetrics) AgentReachable(agent string, reachable bool) {
	if c == nil {
		return
	}
	v := 0.0
	if reachable {
		v = 1
	}
	c.agentReachable.WithLabelValues(agent).Set(v)
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./controlplane/ ./internal/obs/ -v`
Expected: PASS (monitor tests + all obs tests). Then `go build ./...` and `go vet ./...`.

- [ ] **Step 6: Commit**

```bash
git add controlplane/monitor.go controlplane/monitor_test.go internal/obs/obs.go
git commit -m "feat(c3): HealthMonitor (poll, no-restart, edge-triggered) + runtime_agent_reachable metric"
```

---

## Task 7: agentd optional bearer-auth middleware

**Files:**
- Modify: `agentruntime/serve.go` (`Manager` struct ~line 23-34; `Serve` ~line 285-292 where Manager is built)
- Modify: `agentruntime/server.go` (`handler()` ~line 16-28)
- Modify: `cmd/agentd/main.go` (env read + pass to Serve)
- Test: `agentruntime/server_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `agentruntime/server_test.go`:

```go
func TestRequireBearer(t *testing.T) {
	const token = "agent-tok"
	mkSrv := func(tok string) *httptest.Server {
		m := newTestManager()
		m.authToken = tok
		return httptest.NewServer(m.handler())
	}

	t.Run("no token configured: open (back-compat)", func(t *testing.T) {
		srv := mkSrv("")
		defer srv.Close()
		resp, err := http.Get(srv.URL + "/healthz")
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("healthz open: err=%v status=%v", err, resp.StatusCode)
		}
	})

	t.Run("token set: 401 without header, including /healthz and /metrics", func(t *testing.T) {
		srv := mkSrv(token)
		defer srv.Close()
		for _, path := range []string{"/healthz", "/metrics", "/sessions"} {
			resp, err := http.Get(srv.URL + path)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("%s without token: status=%d, want 401", path, resp.StatusCode)
			}
		}
	})

	t.Run("token set: 200 with correct bearer", func(t *testing.T) {
		srv := mkSrv(token)
		defer srv.Close()
		req, _ := http.NewRequest("GET", srv.URL+"/healthz", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("healthz with token: err=%v status=%v", err, resp.StatusCode)
		}
	})

	t.Run("token set: 401 with wrong bearer", func(t *testing.T) {
		srv := mkSrv(token)
		defer srv.Close()
		req, _ := http.NewRequest("GET", srv.URL+"/healthz", nil)
		req.Header.Set("Authorization", "Bearer wrong")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("healthz wrong token: status=%d, want 401", resp.StatusCode)
		}
	})
}
```

Note: this test calls `m.handler()` (not `newMux()`), because the auth wrapper lives in `handler()`.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./agentruntime/ -run RequireBearer -v`
Expected: FAIL to compile — `Manager` has no `authToken` field.

- [ ] **Step 3: Add the `authToken` field + middleware**

In `agentruntime/serve.go`, add to the `Manager` struct (after `metrics`):

```go
	// authToken, when non-empty, requires every inbound request to carry
	// Authorization: Bearer <authToken>. "" ⇒ no auth (local/loopback agents).
	authToken string
```

In `agentruntime/server.go`, add imports `"crypto/subtle"` and `"strings"` (check existing imports first; add only what's missing), and replace `handler()`:

```go
func (m *Manager) handler() http.Handler {
	mux := m.newMux()
	logged := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" && r.URL.Path != "/metrics" {
			slog.Info("http",
				"method", r.Method,
				"path", r.URL.Path,
				"request_id", obs.RequestIDFromContext(r.Context()))
		}
		mux.ServeHTTP(w, r)
	})
	var h http.Handler = logged
	if m.authToken != "" {
		h = requireBearer(m.authToken, logged)
	}
	return obs.RequestID(h)
}

// requireBearer rejects any request whose Authorization header is not exactly
// "Bearer <token>" with 401, using a constant-time compare. It guards ALL
// paths (including /healthz and /metrics): a remote agent's probe endpoints are
// on the same routable port, and runtimed sends the token on those too.
func requireBearer(token string, next http.Handler) http.Handler {
	want := "Bearer " + token
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
```

(`strings` may not be needed — the implementation above uses `subtle` only. Add `"crypto/subtle"` to imports; do NOT add `"strings"` unless used.)

- [ ] **Step 4: Wire the env var through `Serve` and `main`**

In `agentruntime/serve.go` `Serve`, set the field when building the `Manager` (the `m := &Manager{...}` literal ~line 285). Add a line reading the env var before the literal and set it:

```go
	m := &Manager{
		agentID:     cfg.Spec.ID,
		cfg:         cfg,
		dbosCtx:     dctx,
		st:          st,
		metrics:     obs.NewAgentMetrics(cfg.Spec.ID),
		subscribers: map[string][]chan WireEvent{},
		authToken:   os.Getenv("RUNTIME_AGENT_AUTH_TOKEN"),
	}
```

(`os` is already imported in `serve.go`.) No change is needed in `cmd/agentd/main.go` — `Serve` reads the env directly, matching the existing convention that operator/env concerns are read at the edge inside `Serve`, not threaded through the builder. (Confirm by re-reading the top of `Serve`: `RUNTIME_PG_DSN` and `RUNTIME_LISTEN_ADDR` are read there the same way.)

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./agentruntime/ -v`
Expected: PASS (new + existing). Then `go build ./...` and `go vet ./...`.

- [ ] **Step 6: Commit**

```bash
git add agentruntime/serve.go agentruntime/server.go agentruntime/server_test.go
git commit -m "feat(c3): agentd optional bearer-auth middleware (RUNTIME_AGENT_AUTH_TOKEN), guards all paths"
```

---

## Task 8: runtimed startup — supervise local, monitor remote

**Files:**
- Modify: `cmd/runtimed/main.go` (`mountMetrics` target builder ~line 218-225; startup loop ~line 244-252)

- [ ] **Step 1: Update the metrics target builder**

In `cmd/runtimed/main.go`, the `mountMetrics` closure currently builds `obs.ScrapeTarget{Agent: ap.AgentID, Addr: ap.Addr}`. Change it to base URL + token:

```go
	handler = mountMetrics(handler, cm, func() []obs.ScrapeTarget {
		var ts []obs.ScrapeTarget
		for _, info := range reg.List() {
			ap, _ := reg.Get(info.ID)
			ts = append(ts, obs.ScrapeTarget{Agent: ap.AgentID, BaseURL: ap.baseURL(), Token: ap.AuthToken})
		}
		return ts
	})
```

Note: `ap.baseURL()` is unexported but in the same module — `cmd/runtimed` imports `controlplane`, and `baseURL()` is a method on the exported `AgentProcess` type. Since `baseURL` is lowercase it is NOT accessible from package `main`. **Add an exported accessor** to `controlplane/proxy.go`:

```go
// DialBase returns the agent's full dial base URL (exported for runtimed's
// metrics target builder).
func (a AgentProcess) DialBase() string { return a.baseURL() }
```

Then use `ap.DialBase()` in `cmd/runtimed/main.go` instead of `ap.baseURL()`.

- [ ] **Step 2: Split the startup loop**

In `cmd/runtimed/main.go`, replace the agent-start loop (`for _, info := range reg.List() {` ... through its closing `}`, ~line 244-252) with:

```go
	for _, info := range reg.List() {
		ap, _ := reg.Get(info.ID)
		if ap.Remote {
			id := ap.AgentID
			hm := &controlplane.HealthMonitor{
				BaseURL: ap.DialBase(), Token: ap.AuthToken,
				OnChange: func(ok bool) { cm.AgentReachable(id, ok) },
			}
			go hm.Run(ctx)
			slog.Info("monitoring remote agent", "agent", ap.AgentID, "url", ap.DialBase())
			continue
		}
		sup := &controlplane.Supervisor{Spawn: ap.SpawnFunc(), Backoff: time.Second, OnRestart: func() { cm.AgentRestart(ap.AgentID) }}
		go sup.Run(ctx)
		slog.Info("supervising agent", "agent", ap.AgentID, "addr", ap.Addr)
		if err := waitAgentHealthy(ctx, ap.Addr, 30*time.Second); err != nil {
			slog.Warn("agent not healthy yet; continuing", "agent", ap.AgentID, "err", err)
		}
	}
```

(The `id := ap.AgentID` local avoids the classic loop-variable capture in the `OnChange` closure. Note Go 1.22+ per-iteration loop vars make this safe anyway, but the explicit local is clearer and matches the existing `OnRestart` capture which already closes over `ap`.)

- [ ] **Step 3: Build and vet**

Run: `go build ./... && go vet ./...`
Expected: success (no test for `main` — it is exercised by the integration test in Task 9).

- [ ] **Step 4: Run the full hermetic suite**

Run: `go test ./...`
Expected: PASS across all packages.

- [ ] **Step 5: Commit**

```bash
git add cmd/runtimed/main.go controlplane/proxy.go
git commit -m "feat(c3): runtimed supervises local agents, monitors remote agents (no restart); metrics via DialBase"
```

---

## Task 9: Integration test — remote attach end-to-end

**Files:**
- Create: `test/remote_agent_test.go` (`//go:build integration`)

This test starts a standalone `agentd` (with an auth token) as a stand-in remote host, then a `runtimed` whose config attaches to it via `url:` + matching `auth_token`, and proves attach, proxy round-trip, fail-closed auth, and degrade-don't-fail (kill the agentd → unreachable, no restart, local agent unaffected).

- [ ] **Step 1: Write the integration test**

Create `test/remote_agent_test.go`:

```go
//go:build integration

package test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestRemoteAgentAttach proves C3: runtimed attaches to a separately-started
// agentd via url: (no spawn), secured by a shared bearer; it proxies, reports
// status, and on the remote dying marks it unreachable WITHOUT restarting,
// while a co-located LOCAL agent keeps working.
func TestRemoteAgentAttach(t *testing.T) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatalf("ping postgres (is it running at %s?): %v", dsn, err)
	}
	mustExec(t, db, `DROP TABLE IF EXISTS markers`)
	mustExec(t, db, `CREATE TABLE markers (id BIGSERIAL PRIMARY KEY, ran_at TIMESTAMPTZ)`)
	mustExec(t, db, `DROP TABLE IF EXISTS session_events, sessions, agents CASCADE`)
	mustExec(t, db, `DROP SCHEMA IF EXISTS dbos CASCADE`)

	tmp := t.TempDir()
	agentd := filepath.Join(tmp, "agentd")
	if out, err := exec.Command("go", "build", "-o", agentd, "../cmd/agentd").CombinedOutput(); err != nil {
		t.Fatalf("build agentd: %v\n%s", err, out)
	}
	runtimed := filepath.Join(tmp, "runtimed")
	if out, err := exec.Command("go", "build", "-o", runtimed, "../cmd/runtimed").CombinedOutput(); err != nil {
		t.Fatalf("build runtimed: %v\n%s", err, out)
	}

	const token = "remote-shared-tok"
	remoteAddr := "127.0.0.1:8211"

	// 1. Start a standalone "remote" agentd with an auth token. This stands in
	//    for an operator-provisioned agent on another host.
	remote := exec.Command(agentd)
	remote.Env = append(os.Environ(),
		"RUNTIME_PG_DSN="+dsn,
		"RUNTIME_LISTEN_ADDR="+remoteAddr,
		"RUNTIME_AGENT_ID=remote",
		"RUNTIME_AGENT_TENANT=default",
		"RUNTIME_AGENT_AUTH_TOKEN="+token,
	)
	remote.Stdout, remote.Stderr = os.Stdout, os.Stderr
	remote.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := remote.Start(); err != nil {
		t.Fatalf("start remote agentd: %v", err)
	}
	remoteKilled := false
	killRemote := func() {
		if remoteKilled {
			return
		}
		remoteKilled = true
		_ = syscall.Kill(-remote.Process.Pid, syscall.SIGKILL)
		_, _ = remote.Process.Wait()
	}
	defer killRemote()

	// Wait for the remote to be healthy (with the token).
	waitHealthyAuth(t, "http://"+remoteAddr+"/healthz", token, 30*time.Second)

	// 2. Config: one LOCAL spawned agent + one REMOTE attached agent.
	cfgPath := filepath.Join(tmp, "runtime.yaml")
	cfg := "agents:\n" +
		"  - {id: local, name: Local, model: test/scripted, listen_addr: 127.0.0.1:8212}\n" +
		fmt.Sprintf("  - {id: remote, name: Remote, model: test/scripted, url: \"http://%s\", auth_token: \"${REMOTE_TOK}\"}\n", remoteAddr)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	ctlAddr := "127.0.0.1:8220"
	rt := exec.Command(runtimed)
	rt.Env = append(os.Environ(),
		"RUNTIME_PG_DSN="+dsn,
		"RUNTIME_CTL_ADDR="+ctlAddr,
		"RUNTIME_AGENTD_BIN="+agentd,
		"RUNTIME_CONFIG="+cfgPath,
		"REMOTE_TOK="+token,
	)
	rt.Stdout, rt.Stderr = os.Stdout, os.Stderr
	rt.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := rt.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Kill(-rt.Process.Pid, syscall.SIGKILL); _, _ = rt.Process.Wait() }()

	waitHealthy(t, "http://"+ctlAddr+"/healthz", 30*time.Second)

	// 3. Both agents healthy via /agents.
	waitFor(t, 20*time.Second, func() bool {
		st := getAgents(t, ctlAddr)
		return st["local"] && st["remote"]
	}, "both agents healthy")

	// 4. Proxy a session round-trip THROUGH runtimed to the remote agent.
	resp, err := http.Post("http://"+ctlAddr+"/agents/remote/sessions", "application/json",
		stringsNewReader(`{"message":"hi"}`))
	if err != nil {
		t.Fatalf("create session on remote: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("remote session create: status=%d body=%s", resp.StatusCode, body)
	}
	var sess struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(body, &sess); err != nil || sess.SessionID == "" {
		t.Fatalf("no session_id: err=%v body=%s", err, body)
	}

	// 5. Degrade-don't-fail: kill the remote agentd. runtimed must stay up, the
	//    LOCAL agent keeps working, and the remote goes unreachable WITHOUT a
	//    restart (no new agentd listening on remoteAddr).
	killRemote()
	waitFor(t, 20*time.Second, func() bool {
		st := getAgents(t, ctlAddr)
		return st["local"] && !st["remote"]
	}, "remote unreachable, local still healthy")

	// No restart: nothing should be listening on remoteAddr now (runtimed does
	// not own that process). Give it a moment, then confirm /healthz fails.
	time.Sleep(2 * time.Second)
	if _, err := http.Get("http://" + remoteAddr + "/healthz"); err == nil {
		t.Fatal("something is still serving on the remote addr — runtimed must NOT restart a remote agent")
	}

	// 6. The control plane is still alive and the local agent still serves.
	resp, err = http.Post("http://"+ctlAddr+"/agents/local/sessions", "application/json",
		stringsNewReader(`{"message":"still here"}`))
	if err != nil {
		t.Fatalf("local session after remote death: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("local agent broken after remote death: status=%d", resp.StatusCode)
	}
}

// --- helpers (kept local to this file to avoid touching shared harness) ---

func getAgents(t *testing.T, ctlAddr string) map[string]bool {
	t.Helper()
	resp, err := http.Get("http://" + ctlAddr + "/agents")
	if err != nil {
		return map[string]bool{}
	}
	defer resp.Body.Close()
	var arr []struct {
		ID      string `json:"id"`
		Healthy bool   `json:"healthy"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&arr); err != nil {
		return map[string]bool{}
	}
	out := map[string]bool{}
	for _, a := range arr {
		out[a.ID] = a.Healthy
	}
	return out
}

func waitHealthy(t *testing.T, url string, d time.Duration) {
	t.Helper()
	waitHealthyAuth(t, url, "", d)
}

func waitHealthyAuth(t *testing.T, url, token string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest("GET", url, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if resp, err := client.Do(req); err == nil {
			code := resp.StatusCode
			resp.Body.Close()
			if code == 200 {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("not healthy within %s: %s", d, url)
}

func waitFor(t *testing.T, d time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s: %s", d, what)
}
```

- [ ] **Step 2: Resolve helper-name collisions with the shared `test` package**

The `test` package already has integration helpers (`mustExec`, `dsn`, possibly `stringsNewReader`/`waitFor`). Before running, grep for collisions:

Run: `grep -rn "func waitFor\|func waitHealthy\|func getAgents\|func stringsNewReader\|func mustExec\|^	dsn " test/*.go`

- If `mustExec` and `dsn` already exist (they do — used by `multiagent_test.go`): do NOT redefine them; the new file uses them as-is.
- If `waitFor`, `waitHealthy`, `waitHealthyAuth`, `getAgents`, or `stringsNewReader` already exist in the package, RENAME the ones in this file (e.g. `waitForRemote`, `getAgentsRemote`) to avoid duplicate-definition compile errors. If `stringsNewReader` does not exist, add it to this file:

```go
func stringsNewReader(s string) *strings.Reader { return strings.NewReader(s) }
```
(and add `"strings"` to imports). Prefer `strings.NewReader` inline if simpler.

- [ ] **Step 3: Run the integration test**

Run: `go test -tags integration ./test/ -run TestRemoteAgentAttach -v`
Expected: PASS. Watch the log for: remote agentd healthy, runtimed "monitoring remote agent", both healthy, remote session created, after kill → remote unreachable + local healthy, no restart, local session still 200.

Troubleshooting:
- If `/agents` never shows remote healthy: confirm the bearer is wired (a 401 on health ⇒ unhealthy). Check `RUNTIME_AGENT_AUTH_TOKEN` on the remote matches `${REMOTE_TOK}`.
- If ports clash with other integration tests, bump `remoteAddr`/`ctlAddr`/local `listen_addr` to unused ports.

- [ ] **Step 4: Commit**

```bash
git add test/remote_agent_test.go
git commit -m "test(c3): integration — remote attach, proxy round-trip, degrade-don't-fail, no restart"
```

---

## Task 10: Docs — runtime.yaml example, README, ROADMAP

**Files:**
- Modify: `README.md` (agent-config / remote section)
- Modify: `ROADMAP.md` (mark C3 M1 done; note deferred items)
- Modify: `runtime.yaml` (add a commented remote-agent example) — VERIFY it stays valid (it must still load; a commented block is inert)

- [ ] **Step 1: Add a remote-agent example to README**

In `README.md`, find the agent-configuration section (search for `listen_addr`). Add a subsection after it:

````markdown
### Remote agents (attach instead of spawn)

An agent entry with `url:` instead of `listen_addr:` is **remote**: runtimed
attaches to an already-running, contract-conformant `agentd` (health-check,
reverse-proxy, status) but does **not** spawn or restart it. The remote agent's
process and environment (`RUNTIME_PG_DSN`, `RUNTIME_LISTEN_ADDR`,
`RUNTIME_AGENT_ID`, `RUNTIME_AGENT_TENANT`, and optionally
`RUNTIME_AGENT_AUTH_TOKEN`) are provisioned by whoever runs that host (systemd,
a Kubernetes Deployment + Secret, `docker run -e`, …).

```yaml
agents:
  - id: remote-1
    name: Remote Agent
    model: test/scripted
    tenant: acme
    url: https://agent-1.internal:8443   # remote: attached + monitored
    auth_token: ${REMOTE_1_TOKEN}        # optional shared bearer for the hop
```

- **Mutually exclusive with `listen_addr`** — an agent is either local (spawned)
  or remote (attached), never both.
- **`auth_token`** (optional, `${VAR}`-expanded) is a shared bearer runtimed
  sends on every request (proxy, health, metrics). The remote `agentd` enforces
  it via `RUNTIME_AGENT_AUTH_TOKEN`; a mismatch shows the agent as `unreachable`.
- **Scheme** `http://` or `https://` — TLS is the operator's choice (real cert,
  service mesh, or ingress).
- **Lifecycle:** a remote agent that is down never blocks runtimed startup and is
  never restarted; it shows as unhealthy/`unreachable` and proxying it returns
  `503` until it comes back. Spawn-time-only fields (`command`, `kind`,
  `memory`, `gateway`) are rejected on a remote agent.
````

- [ ] **Step 2: Add a commented example to runtime.yaml**

In `runtime.yaml`, append a commented block (inert — must not change loading):

```yaml
# Remote agent example (C3): attach to an already-running agentd instead of
# spawning it. Uncomment and adjust; the remote agentd must be started
# separately with RUNTIME_AGENT_AUTH_TOKEN matching auth_token below.
#  - id: remote-1
#    name: Remote Agent
#    model: test/scripted
#    url: https://agent-1.internal:8443
#    auth_token: ${REMOTE_1_TOKEN}
```

Verify it still loads: `go run ./cmd/runtimed` is not safe (needs PG); instead confirm with a parse check:

Run: `go test ./internal/config/ -run TestLoad -v` (existing tests cover loading; the commented block is inert so no new test needed). Also eyeball that the block is fully commented.

- [ ] **Step 3: Mark C3 M1 done in ROADMAP**

In `ROADMAP.md`, under the `### C3. Remote agents (attach instead of spawn)` heading, add a `**C3 M1 — DONE (2026-06-13).**` paragraph summarizing: `url:`+`auth_token:` schema; operator-provisioned secrets (handshake deferred to C3 M2); opt-in bearer (mTLS deferred); attach+monitor (no restart, degrade-don't-fail); the four dial sites upgraded to base URL+token; agentd optional bearer middleware; `runtime_agent_reachable` metric; hermetic + integration tests; live proof. Note remaining C3: registration handshake (M2), mTLS, and that per-agent-pod scheduling (C2) now unblocked.

Keep it consistent in tone with the existing C2 M1 entry. (The exact prose is the implementer's to write from this task's bullet list; it is documentation, not code.)

- [ ] **Step 4: Build, vet, full hermetic test**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add README.md ROADMAP.md runtime.yaml
git commit -m "docs(c3): remote-agent README section, runtime.yaml example, ROADMAP M1 done"
```

---

## Final verification (after all tasks)

- [ ] Hermetic suite green: `go test ./... && go vet ./...`
- [ ] Integration test green: `go test -tags integration ./test/ -run TestRemoteAgentAttach -v` (needs Postgres.app)
- [ ] Live proof (manual, the milestone gate — see spec §6): build both binaries, start a remote `agentd` with a token, start `runtimed` with a mixed local+remote config, run `runtimectl conformance` against the remote agent through the proxy, verify `/agents` distinguishes both, kill the remote → unreachable + local unaffected + no restart, restart it → back to reachable, and prove a mismatched token shows `unreachable`.
- [ ] Then proceed to `finishing-a-development-branch` (merge to `master`).

---

## Self-Review (completed by plan author)

**Spec coverage:**
- §1 scope (operator-provisioned, opt-in bearer, attach+monitor, degrade-don't-fail) → Tasks 1–9.
- §2 config schema + validation → Task 1.
- §3 dial identity + four sites: `AgentProcess`/`authTransport`/`reverseProxy` (Task 2), registry (Task 3), `/agents`+proxy (Task 4), fan-out (Task 5), startup gate/metrics builder (Task 8).
- §4 lifecycle: `HealthMonitor` + metric (Task 6), startup split (Task 8).
- §5 agentd bearer middleware (Task 7).
- §6 testing + live proof: hermetic across Tasks 1–7, integration (Task 9), live proof (final verification).
- Deferred items recorded in docs (Task 10).

**Type/signature consistency:** `reverseProxy(base, token, onError)` defined in Task 2 and called in Task 4. `ScrapeTarget{Agent, BaseURL, Token}` defined in Task 5, consumed in Task 8. `AgentProcess.{Remote,BaseURL,AuthToken}` defined in Task 2, set in Task 3, read in Tasks 4/8. `HealthMonitor` defined in Task 6, used in Task 8. `AgentReachable` defined in Task 6, called in Task 8. `Manager.authToken` defined in Task 7. The unexported `baseURL()` (Task 2) is surfaced as exported `DialBase()` (Task 8) for package `main`.

**Known cross-file edits flagged for implementers:** existing `reverseProxy(...)` call sites in `proxy_test.go` (Task 2 Step 2); existing `ScrapeTarget{...Addr}` literals in `fanout_test.go` (Task 5 Step 2); helper-name collisions in the `test` package (Task 9 Step 2).
