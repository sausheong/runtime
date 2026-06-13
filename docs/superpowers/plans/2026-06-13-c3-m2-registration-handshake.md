# C3 M2 — Registration Handshake Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a remote/scheduled `agentd` pull its full environment (DSN, identity, opt-in feature vars, and brokered per-tenant secrets) from the control plane at boot via an authenticated registration handshake, closing the C2 M2 spawn-time-secrets limitation.

**Architecture:** A pull handshake. agentd, when `RUNTIME_REGISTRATION_URL`+`_TOKEN` are set, `POST`s `/register` with a per-agent identity-backed bearer and its `$HOSTNAME` ordinal; the control plane verifies the token (→ `agent_id`), validates the ordinal, computes the per-replica **env delta** (the entries `buildEnv` adds on top of the inherited env — never runtimed's own `os.Environ()`), and returns it as JSON. agentd `os.Setenv`s the delta then runs its unchanged `os.Getenv` startup path. `buildEnv` is split into `envDelta` (network-safe) so the inherited-env leak is structurally impossible.

**Tech Stack:** Go 1.25, `net/http`, `encoding/json`, Postgres (identity store reuse: `MintServiceKey`/`ParseServiceKey`/`VerifyKey`, bcrypt), Helm.

**Spec:** `docs/superpowers/specs/2026-06-13-c3-m2-registration-handshake-design.md`

---

## File Structure

- `controlplane/proxy.go` — split `buildEnv` into `envDelta` + `buildEnv` (Task 1).
- `internal/identity/schema.sql`, `internal/identity/store.go` — `registration_tokens` table + CRUD (Task 2).
- `controlplane/register.go` (new) — `POST /register` handler, request/response types, token verify + `Registry.Replica` + `envDelta` (Task 3).
- `controlplane/admin.go` — `AdminStore` gains registration-token methods; `RegisterAdmin` mounts `/admin/register-tokens` (Task 4).
- `cmd/runtimed/main.go` — construct/mount; access-log fields (Task 4).
- `cmd/runtimectl/main.go` — `register mint|list|revoke` (Task 5).
- `cmd/agentd/register.go` (new) + `cmd/agentd/main.go` — `fetchRegistration` prelude (Task 6).
- `test/registration_handshake_test.go` (new) — integration (Task 7).
- `deploy/charts/runtime/templates/{agent-statefulset,secret}.yaml`, `values.yaml` (Task 8).
- `deploy/charts/runtime/test.sh`, `README.md`, `ROADMAP.md` (Task 9).

**Conventions (all tasks):** `go` CLI is ground truth (ignore IDE/LSP `replace ../harness` diagnostics). Integration tests: `//go:build integration`, `package test`, Postgres.app at `postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable`, self-clean DB + `dbos` schema; scripted model `test/scripted` (no LLM key). gofmt-clean before commit. Commit trailer: `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`. No secrets or secret-names in logs/spans.

---

## Task 1: Split `buildEnv` into a network-safe `envDelta`

**Files:**
- Modify: `controlplane/proxy.go:66-112` (`buildEnv`)
- Test: `controlplane/proxy_test.go` (add)

- [ ] **Step 1: Write the failing test**

Add to `controlplane/proxy_test.go`:

```go
func TestEnvDeltaExcludesInheritedEnv(t *testing.T) {
	// A sentinel in the process env must NOT appear in the delta — the delta is
	// the ONLY thing the registration endpoint returns over the network.
	t.Setenv("RUNTIME_C3M2_SENTINEL", "leak-me")
	ap := AgentProcess{
		AgentID: "a1", Addr: "127.0.0.1:8081", PGDSN: "dsn://x",
		Tenant: "t1", ReplicaIndex: 2, DBOSVMID: "a1#2",
	}
	delta, err := ap.envDelta(context.Background())
	if err != nil {
		t.Fatalf("envDelta: %v", err)
	}
	joined := strings.Join(delta, "\n")
	if strings.Contains(joined, "RUNTIME_C3M2_SENTINEL") || strings.Contains(joined, "leak-me") {
		t.Fatalf("delta leaked inherited env:\n%s", joined)
	}
	// The control vars MUST be present.
	for _, want := range []string{
		"RUNTIME_PG_DSN=dsn://x", "RUNTIME_AGENT_ID=a1",
		"RUNTIME_AGENT_TENANT=t1", "RUNTIME_AGENT_REPLICA=2", "DBOS__VMID=a1#2",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("delta missing %q:\n%s", want, joined)
		}
	}
}

func TestBuildEnvIsEnvironPlusDelta(t *testing.T) {
	t.Setenv("RUNTIME_C3M2_SENTINEL2", "keep")
	ap := AgentProcess{AgentID: "a1", Addr: "127.0.0.1:8081", PGDSN: "dsn://x", Tenant: "t1"}
	full, err := ap.buildEnv(context.Background())
	if err != nil {
		t.Fatalf("buildEnv: %v", err)
	}
	joined := strings.Join(full, "\n")
	// buildEnv = os.Environ() + delta, so the inherited sentinel IS present here.
	if !strings.Contains(joined, "RUNTIME_C3M2_SENTINEL2=keep") {
		t.Fatalf("buildEnv dropped inherited env")
	}
	if !strings.Contains(joined, "RUNTIME_AGENT_ID=a1") {
		t.Fatalf("buildEnv dropped delta")
	}
}
```

Ensure `proxy_test.go` imports `context` and `strings` (add if missing).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./controlplane/ -run 'TestEnvDelta|TestBuildEnvIsEnviron' -v`
Expected: FAIL — `ap.envDelta undefined`.

- [ ] **Step 3: Refactor `buildEnv`**

In `controlplane/proxy.go`, replace the `buildEnv` method (lines 62-112) with a delta extraction plus a thin `buildEnv`. The body that currently appends to `os.Environ()` moves into `envDelta`, starting from an empty slice:

```go
// envDelta returns ONLY the entries buildEnv adds on top of the inherited
// process environment: the RUNTIME_* control vars, the opt-in feature vars, and
// (if a broker is set) the tenant's decrypted secrets. It NEVER includes
// os.Environ(), so it is safe to serialize across the network to a remote agent
// (the registration handshake). A broker error fails closed.
func (a AgentProcess) envDelta(ctx context.Context) ([]string, error) {
	env := []string{
		"RUNTIME_PG_DSN=" + a.PGDSN,
		"RUNTIME_LISTEN_ADDR=" + a.Addr,
		"RUNTIME_AGENT_ID=" + a.AgentID,
		"RUNTIME_AGENT_KIND=" + a.Kind,
		"RUNTIME_AGENT_TENANT=" + a.Tenant,
		"RUNTIME_AGENT_REPLICA=" + strconv.Itoa(a.ReplicaIndex),
		"DBOS__VMID=" + a.DBOSVMID,
	}
	if a.Memory {
		env = append(env, "RUNTIME_AGENT_MEMORY=1")
	} else {
		env = append(env, "RUNTIME_AGENT_MEMORY=")
	}
	if a.GatewayOn {
		u := a.GatewayURL
		if a.GatewaySearch {
			u += "?mode=search"
		}
		env = append(env, "RUNTIME_GATEWAY_URL="+u)
		if a.GatewayKey != "" {
			env = append(env, "RUNTIME_GATEWAY_KEY="+a.GatewayKey)
		} else {
			env = append(env, "RUNTIME_GATEWAY_KEY=")
		}
	} else {
		env = append(env, "RUNTIME_GATEWAY_URL=", "RUNTIME_GATEWAY_KEY=")
	}
	if a.broker != nil {
		secrets, err := a.broker.SecretsFor(ctx, a.Tenant)
		if err != nil {
			return nil, err
		}
		for name, val := range secrets {
			env = append(env, name+"="+val)
		}
	}
	return env, nil
}

// buildEnv assembles the full child environment for a LOCAL spawn: the inherited
// operator env, then envDelta on top (so the delta shadows any inherited var of
// the same name). buildEnv = os.Environ() + envDelta.
func (a AgentProcess) buildEnv(ctx context.Context) ([]string, error) {
	delta, err := a.envDelta(ctx)
	if err != nil {
		return nil, err
	}
	return append(os.Environ(), delta...), nil
}
```

Keep the existing doc comment intent. `SpawnFunc` is unchanged (still calls `buildEnv`).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./controlplane/ -run 'TestEnvDelta|TestBuildEnvIsEnviron|TestSpawn' -v && go vet ./controlplane/`
Expected: PASS (existing spawn tests still green — local behavior unchanged).

- [ ] **Step 5: Commit**

```bash
gofmt -w controlplane/proxy.go controlplane/proxy_test.go
git add controlplane/proxy.go controlplane/proxy_test.go
git commit -m "refactor(controlplane): split buildEnv into network-safe envDelta

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 2: `registration_tokens` store

**Files:**
- Modify: `internal/identity/schema.sql` (append table)
- Modify: `internal/identity/store.go` (add row type + 4 methods + `ErrNoRegToken`)
- Test: `internal/identity/regtoken_integration_test.go` (new, `//go:build integration`)

- [ ] **Step 1: Write the failing test**

Create `internal/identity/regtoken_integration_test.go`:

```go
//go:build integration

package identity

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func dsn() string {
	if v := os.Getenv("RUNTIME_PG_DSN"); v != "" {
		return v
	}
	return "postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable"
}

func TestRegistrationTokenCRUD(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("pgx", dsn())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	// Self-clean.
	_, _ = db.ExecContext(ctx, `DROP TABLE IF EXISTS registration_tokens`)

	st, err := NewStore(ctx, db)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	mk, err := MintServiceKey()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if err := st.InsertRegistrationToken(ctx, mk.ID, "agent-x", mk.Hash); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// Active lookup resolves agent_id + hash.
	agentID, hash, err := st.ActiveRegTokenByID(ctx, mk.ID)
	if err != nil || agentID != "agent-x" || hash != mk.Hash {
		t.Fatalf("active lookup: agent=%q hash=%q err=%v", agentID, hash, err)
	}
	// List shows it, never the secret.
	rows, err := st.ListRegistrationTokens(ctx)
	if err != nil || len(rows) != 1 || rows[0].AgentID != "agent-x" || rows[0].Revoked {
		t.Fatalf("list: %+v err=%v", rows, err)
	}
	// Revoke → active lookup fails closed.
	if err := st.RevokeRegistrationToken(ctx, mk.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, _, err := st.ActiveRegTokenByID(ctx, mk.ID); err != ErrNoRegToken {
		t.Fatalf("want ErrNoRegToken after revoke, got %v", err)
	}
	rows, _ = st.ListRegistrationTokens(ctx)
	if len(rows) != 1 || !rows[0].Revoked {
		t.Fatalf("list after revoke: %+v", rows)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -tags integration ./internal/identity/ -run TestRegistrationTokenCRUD -v`
Expected: FAIL — `st.InsertRegistrationToken undefined`.

- [ ] **Step 3: Add the schema**

Append to `internal/identity/schema.sql`:

```sql
CREATE TABLE IF NOT EXISTS registration_tokens (
    token_id    TEXT PRIMARY KEY,
    agent_id    TEXT NOT NULL,
    hash        TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at  TIMESTAMPTZ
);
```

- [ ] **Step 4: Add the row type, error, and methods**

In `internal/identity/store.go`, add to the `var (...)` error block:

```go
	ErrNoRegToken = errors.New("identity: no such registration token")
```

Add a row type near the other read models:

```go
// RegTokenRow is the listing read model for a registration token (never the secret).
type RegTokenRow struct {
	TokenID string
	AgentID string
	Revoked bool
}
```

Add the four methods (after the service-key methods):

```go
// InsertRegistrationToken stores a minted registration token's hash, bound to an agent.
func (s *Store) InsertRegistrationToken(ctx context.Context, tokenID, agentID, hash string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO registration_tokens (token_id, agent_id, hash) VALUES ($1,$2,$3)`,
		tokenID, agentID, hash)
	return err
}

// ActiveRegTokenByID returns a non-revoked token's agent_id + hash, or ErrNoRegToken.
func (s *Store) ActiveRegTokenByID(ctx context.Context, tokenID string) (agentID, hash string, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT agent_id, hash FROM registration_tokens WHERE token_id=$1 AND revoked_at IS NULL`, tokenID).
		Scan(&agentID, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrNoRegToken
	}
	return agentID, hash, err
}

// RevokeRegistrationToken marks a token revoked. No-op if already revoked/absent.
func (s *Store) RevokeRegistrationToken(ctx context.Context, tokenID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE registration_tokens SET revoked_at=now() WHERE token_id=$1 AND revoked_at IS NULL`, tokenID)
	return err
}

// ListRegistrationTokens returns all tokens (secrets never included).
func (s *Store) ListRegistrationTokens(ctx context.Context) ([]RegTokenRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT token_id, agent_id, (revoked_at IS NOT NULL) FROM registration_tokens ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RegTokenRow
	for rows.Next() {
		var r RegTokenRow
		if err := rows.Scan(&r.TokenID, &r.AgentID, &r.Revoked); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test -tags integration ./internal/identity/ -run TestRegistrationTokenCRUD -v && go vet ./internal/identity/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
gofmt -w internal/identity/store.go
git add internal/identity/schema.sql internal/identity/store.go internal/identity/regtoken_integration_test.go
git commit -m "feat(identity): registration_tokens store (mint/verify/revoke/list)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 3: `POST /register` handler

**Files:**
- Create: `controlplane/register.go`
- Test: `controlplane/register_test.go` (new)

- [ ] **Step 1: Write the failing test**

Create `controlplane/register_test.go`:

```go
package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sausheong/runtime/internal/config"
	"github.com/sausheong/runtime/internal/identity"
)

// fakeRegTokens implements RegTokenVerifier for hermetic tests.
type fakeRegTokens struct {
	agentID, hash string
	err           error
}

func (f fakeRegTokens) ActiveRegTokenByID(_ context.Context, id string) (string, string, error) {
	if f.err != nil {
		return "", "", f.err
	}
	return f.agentID, f.hash, nil
}

// fakeBroker returns a fixed secret set (SecretBroker).
type fakeBroker struct {
	secrets map[string]string
	err     error
}

func (f fakeBroker) SecretsFor(_ context.Context, _ string) (map[string]string, error) {
	return f.secrets, f.err
}

func regTestRegistry(t *testing.T, broker SecretBroker) *Registry {
	t.Helper()
	cfg := &config.Config{Agents: []config.AgentConfig{
		{ID: "support", Name: "Support", Model: "test/scripted", Tenant: "acme",
			URL: "http://127.0.0.1:900{i}", Replicas: 2},
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg validate: %v", err)
	}
	reg := NewRegistry(cfg, "", "dsn://x")
	if broker != nil {
		reg.SetBroker(broker)
	}
	return reg
}

func post(t *testing.T, h http.Handler, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/register", bytes.NewReader(buf))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestRegister_Success(t *testing.T) {
	mk, _ := identity.MintServiceKey()
	store := fakeRegTokens{agentID: "support", hash: mk.Hash}
	broker := fakeBroker{secrets: map[string]string{"OPENAI_API_KEY": "sk-xyz"}}
	mux := http.NewServeMux()
	RegisterHandshake(mux, store, regTestRegistry(t, broker))

	rec := post(t, mux, mk.Plaintext, RegisterRequest{Ordinal: 1})
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body)
	}
	var resp RegisterResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Env["RUNTIME_AGENT_ID"] != "support" ||
		resp.Env["RUNTIME_AGENT_TENANT"] != "acme" ||
		resp.Env["RUNTIME_AGENT_REPLICA"] != "1" ||
		resp.Env["DBOS__VMID"] != "" || // remote pool: DBOSVMID empty (remote owns its id)
		resp.Env["OPENAI_API_KEY"] != "sk-xyz" {
		t.Fatalf("unexpected env: %+v", resp.Env)
	}
}

func TestRegister_MissingBearer(t *testing.T) {
	mux := http.NewServeMux()
	RegisterHandshake(mux, fakeRegTokens{}, regTestRegistry(t, nil))
	rec := post(t, mux, "", RegisterRequest{Ordinal: 0})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
}

func TestRegister_WrongSecret(t *testing.T) {
	mk, _ := identity.MintServiceKey()
	other, _ := identity.MintServiceKey()
	store := fakeRegTokens{agentID: "support", hash: mk.Hash}
	mux := http.NewServeMux()
	RegisterHandshake(mux, store, regTestRegistry(t, nil))
	rec := post(t, mux, other.Plaintext, RegisterRequest{Ordinal: 0}) // valid format, wrong secret
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
}

func TestRegister_Revoked(t *testing.T) {
	mk, _ := identity.MintServiceKey()
	store := fakeRegTokens{err: identity.ErrNoRegToken}
	mux := http.NewServeMux()
	RegisterHandshake(mux, store, regTestRegistry(t, nil))
	rec := post(t, mux, mk.Plaintext, RegisterRequest{Ordinal: 0})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
}

func TestRegister_OrdinalOutOfRange(t *testing.T) {
	mk, _ := identity.MintServiceKey()
	store := fakeRegTokens{agentID: "support", hash: mk.Hash}
	mux := http.NewServeMux()
	RegisterHandshake(mux, store, regTestRegistry(t, nil))
	rec := post(t, mux, mk.Plaintext, RegisterRequest{Ordinal: 7}) // support has replicas:2
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", rec.Code)
	}
}

func TestRegister_UnknownAgent(t *testing.T) {
	mk, _ := identity.MintServiceKey()
	store := fakeRegTokens{agentID: "ghost", hash: mk.Hash}
	mux := http.NewServeMux()
	RegisterHandshake(mux, store, regTestRegistry(t, nil))
	rec := post(t, mux, mk.Plaintext, RegisterRequest{Ordinal: 0})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", rec.Code)
	}
}

func TestRegister_BrokerErrorFailsClosed(t *testing.T) {
	mk, _ := identity.MintServiceKey()
	store := fakeRegTokens{agentID: "support", hash: mk.Hash}
	broker := fakeBroker{err: context.DeadlineExceeded}
	mux := http.NewServeMux()
	RegisterHandshake(mux, store, regTestRegistry(t, broker))
	rec := post(t, mux, mk.Plaintext, RegisterRequest{Ordinal: 0})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", rec.Code)
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("OPENAI")) {
		t.Fatalf("no partial env on broker error")
	}
}
```

> Note: confirm `config.Agent` field names (`URL`, `Replicas`, `Tenant`) match `internal/config/config.go`. If `Replicas` is named differently, adjust the test and `regTestRegistry`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./controlplane/ -run TestRegister -v`
Expected: FAIL — `RegisterHandshake undefined`.

- [ ] **Step 3: Implement the handler**

Create `controlplane/register.go`:

```go
package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/sausheong/runtime/internal/identity"
)

// RegTokenVerifier resolves a registration token id to its agent_id + bcrypt
// hash, or identity.ErrNoRegToken when absent/revoked. *identity.Store implements it.
type RegTokenVerifier interface {
	ActiveRegTokenByID(ctx context.Context, tokenID string) (agentID, hash string, err error)
}

// RegisterRequest is the agent's handshake body.
type RegisterRequest struct {
	Ordinal int `json:"ordinal"`
}

// RegisterResponse carries the env delta (KEY→VAL) the agent applies via os.Setenv.
type RegisterResponse struct {
	Env map[string]string `json:"env"`
}

// RegisterHandshake mounts POST /register. It authenticates with the agent's OWN
// per-agent registration token (NOT the identity middleware), so it is mounted
// OUTSIDE the identity chain (like /metrics). The token's binding to an agent_id
// is authoritative — the response is that agent's env delta for the claimed
// ordinal, validated fail-closed against the agent's configured replica count.
func RegisterHandshake(mux *http.ServeMux, tokens RegTokenVerifier, reg *Registry) {
	mux.HandleFunc("POST /register", func(w http.ResponseWriter, r *http.Request) {
		raw := extractToken(r) // existing helper in controlplane/auth.go
		id, secret, ok := identity.ParseServiceKey(raw)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		agentID, hash, err := tokens.ActiveRegTokenByID(r.Context(), id)
		if err != nil || !identity.VerifyKey(hash, secret) {
			// Uniform 401: no oracle distinguishing "no token" from "wrong secret".
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var body RegisterRequest
		// Tolerate an empty body (ordinal 0 default); reject only malformed JSON.
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			http.Error(w, "bad request body", http.StatusBadRequest)
			return
		}
		ap, ok := reg.Replica(agentID, body.Ordinal)
		if !ok {
			// Unknown agent OR ordinal out of [0, replicaCount). Fail closed.
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		delta, err := ap.envDelta(r.Context())
		if err != nil {
			// Broker error (e.g. undecryptable secret): fail closed, no partial env.
			slog.Error("register: envDelta failed", "agent", agentID, "tenant", ap.Tenant, "ordinal", body.Ordinal, "err", err)
			http.Error(w, "registration unavailable", http.StatusServiceUnavailable)
			return
		}
		env := make(map[string]string, len(delta))
		for _, kv := range delta {
			if i := strings.IndexByte(kv, '='); i >= 0 {
				env[kv[:i]] = kv[i+1:]
			}
		}
		// Access log: identifiers only — NEVER an env value or secret name.
		slog.Info("register", "agent", agentID, "tenant", ap.Tenant, "ordinal", body.Ordinal, "token_id", id, "vars", len(env))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(RegisterResponse{Env: env})
	})
}

```

> Reuse the EXISTING `extractToken(r *http.Request) string` in `controlplane/auth.go` (it already pulls the `Bearer ` token). Do NOT define a new helper — `register.go` is package `controlplane`, so `extractToken` is in scope and a duplicate would not compile. The `strings` import is still needed for the `IndexByte` env split.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./controlplane/ -run TestRegister -v && go vet ./controlplane/`
Expected: PASS (all 8 subtests).

- [ ] **Step 5: Commit**

```bash
gofmt -w controlplane/register.go controlplane/register_test.go
git add controlplane/register.go controlplane/register_test.go
git commit -m "feat(controlplane): POST /register handshake endpoint

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 4: Wire into runtimed (mount, admin mint/list/revoke, access log)

**Files:**
- Modify: `controlplane/admin.go` (`AdminStore` + `RegisterAdmin` + new mount)
- Modify: `cmd/runtimed/main.go` (construct + mount `/register` outside identity chain; pass agent→tenant)
- Test: `controlplane/admin_test.go` (add registration-token admin subtests; if no admin_test.go exists, create one following the fake-AdminStore pattern)

- [ ] **Step 1: Write the failing test**

Add to `controlplane/admin_test.go` (create if absent). The fake must implement the **extended** `AdminStore`. Add registration-token methods to the existing fake (or a new one) and assert:

```go
func TestAdminRegisterTokens(t *testing.T) {
	// Build a mux with RegisterAdmin, inject an admin Principal via context,
	// then: POST /admin/register-tokens {agent:"support"} → 201 + {id,plaintext};
	// minting for an agent in ANOTHER tenant as a non-superuser → 403;
	// GET /admin/register-tokens → 200 list (no secret);
	// DELETE /admin/register-tokens/{id} → 204.
}
```

`admin_test.go` already exists (package `controlplane`) with helpers `newFakeAdminStore()`, `withPrincipal(r, p)` (injects `identity.Principal` via the package-private `principalKey`), and `adminMux(s)`. Reuse them. The agent→tenant map passed to `RegisterAdmin` should map `support→acme`; test a non-superuser admin whose `TenantID=="acme"` succeeds minting for `support`, and one whose `TenantID=="other"` gets 403 for `support`.

> **Two required changes to `admin_test.go`:** (1) `adminMux(s)` calls `RegisterAdmin(mux, s)` — update it to the new 3-arg form `RegisterAdmin(mux, s, map[string]string{"support": "acme"})` (or add an `agentTenants` param). (2) `fakeAdminStore` must implement the three new `AdminStore` methods (`InsertRegistrationToken`, `ListRegistrationTokens`, `RevokeRegistrationToken`) — add an in-memory `regTokens map[string]identity.RegTokenRow` field and the methods, mirroring the existing `keys` handling.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./controlplane/ -run TestAdminRegisterTokens -v`
Expected: FAIL — new methods/routes undefined.

- [ ] **Step 3: Extend `AdminStore` and `RegisterAdmin`**

In `controlplane/admin.go`, add to the `AdminStore` interface:

```go
	InsertRegistrationToken(ctx context.Context, tokenID, agentID, hash string) error
	ListRegistrationTokens(ctx context.Context) ([]identity.RegTokenRow, error)
	RevokeRegistrationToken(ctx context.Context, tokenID string) error
```

Change `RegisterAdmin`'s signature to accept the agent→tenant map and mount the routes:

```go
func RegisterAdmin(mux *http.ServeMux, s AdminStore, agentTenants map[string]string) {
	// ... existing routes unchanged ...

	mux.HandleFunc("POST /admin/register-tokens", func(w http.ResponseWriter, r *http.Request) {
		p, ok := requireAdmin(w, r)
		if !ok {
			return
		}
		var body struct{ Agent string }
		if !decode(w, r, &body) {
			return
		}
		if body.Agent == "" {
			http.Error(w, "agent required", http.StatusBadRequest)
			return
		}
		tenant, known := agentTenants[body.Agent]
		if !known {
			http.Error(w, "unknown agent", http.StatusBadRequest)
			return
		}
		// A non-superuser admin may only mint for agents in their own tenant
		// (the token grants access to that agent's tenant's brokered secrets).
		if !p.Superuser && tenant != p.TenantID {
			http.Error(w, "forbidden: agent belongs to another tenant", http.StatusForbidden)
			return
		}
		mk, err := identity.MintServiceKey()
		if err != nil {
			serverError(w, "mint registration token", err)
			return
		}
		if err := s.InsertRegistrationToken(r.Context(), mk.ID, body.Agent, mk.Hash); err != nil {
			serverError(w, "insert registration token", err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"id": mk.ID, "plaintext": mk.Plaintext})
	})

	mux.HandleFunc("GET /admin/register-tokens", func(w http.ResponseWriter, r *http.Request) {
		p, ok := requireAdmin(w, r)
		if !ok {
			return
		}
		rows, err := s.ListRegistrationTokens(r.Context())
		if err != nil {
			serverError(w, "list registration tokens", err)
			return
		}
		// Non-superusers see only tokens for agents in their tenant.
		if !p.Superuser {
			filtered := rows[:0]
			for _, rw := range rows {
				if agentTenants[rw.AgentID] == p.TenantID {
					filtered = append(filtered, rw)
				}
			}
			rows = filtered
		}
		writeJSON(w, http.StatusOK, rows)
	})

	mux.HandleFunc("DELETE /admin/register-tokens/{id}", func(w http.ResponseWriter, r *http.Request) {
		_, ok := requireAdmin(w, r)
		if !ok {
			return
		}
		// Revoke is by token_id (globally unique); admin role suffices.
		if err := s.RevokeRegistrationToken(r.Context(), r.PathValue("id")); err != nil {
			serverError(w, "revoke registration token", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
```

- [ ] **Step 4: Wire runtimed**

In `cmd/runtimed/main.go`:

1. Update the two `RegisterAdmin` call sites — they are inside `buildRoot` (line 428). Change `buildRoot` to pass `reg.AgentTenants()`:
   - In `buildRoot`, replace `controlplane.RegisterAdmin(apiMux, adminS)` with `controlplane.RegisterAdmin(apiMux, adminS, reg.AgentTenants())`.

2. Mount `/register` OUTSIDE the identity/access-log chain, alongside `/metrics`. The cleanest seam is `mountMetrics` (it already overlays routes on the outer mux). Extend it, or add a sibling. Add a `mountRegister` overlay and apply it in BOTH the open-mode and identity-on `handler = ...` assignments (lines ~199 and ~241), wrapping the same way `/metrics` is applied via the existing outer mux at the bottom of `main`. Concretely, find where `mountMetrics(...)` is called and add the `/register` route to that same outer mux:

```go
// in mountMetrics (rename intent kept), add:
mux.Handle("POST /register", registerHandler)
```

   Build `registerHandler` once in `main` after `reg` and `idStore` exist:

```go
regMux := http.NewServeMux()
controlplane.RegisterHandshake(regMux, idStore, reg)
```

   and pass `regMux` into the outer-mux assembly so `POST /register` is served pre-identity. (Implement whichever wiring matches the existing `mountMetrics` call exactly; the invariant is: `/register` is reachable without an identity principal, authenticated solely by its own token.)

> `idStore` (`*identity.Store`) satisfies `controlplane.RegTokenVerifier` via `ActiveRegTokenByID`. Confirm the method set matches.

- [ ] **Step 5: Run tests + build**

Run: `go build ./... && go vet ./... && go test ./controlplane/ -run 'TestAdmin|TestRegister' -v`
Expected: build clean; admin + register tests PASS.

- [ ] **Step 6: Commit**

```bash
gofmt -w controlplane/admin.go cmd/runtimed/main.go controlplane/admin_test.go
git add controlplane/admin.go cmd/runtimed/main.go controlplane/admin_test.go
git commit -m "feat(runtimed): mount /register; admin mint/list/revoke registration tokens

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 5: `runtimectl register` subcommands

**Files:**
- Modify: `cmd/runtimectl/main.go` (dispatch + `runRegister`)

- [ ] **Step 1: Add the subcommand dispatch**

In `main()`'s switch (near `case "admin":`), add:

```go
	case "register":
		runRegister(base, os.Args[2:])
```

- [ ] **Step 2: Implement `runRegister`**

Add (modeled on `runAdmin`, reusing `mustAdminPost`/`mustAdminGet`/`mustAdminDelete`):

```go
func runRegister(base string, args []string) {
	if len(args) < 1 {
		registerUsage()
	}
	switch args[0] {
	case "mint":
		// register mint --agent <id>
		agent := flagValue(args[1:], "--agent", "")
		if agent == "" {
			registerUsage()
		}
		out := mustAdminPost(base, "/admin/register-tokens", map[string]string{"agent": agent})
		var resp struct{ ID, Plaintext string }
		if err := json.Unmarshal(out, &resp); err != nil || resp.Plaintext == "" {
			fmt.Fprintf(os.Stderr, "token created but plaintext missing: %s\n", out)
			os.Exit(1)
		}
		fmt.Printf("%s\n(store this now — shown once; set it as RUNTIME_REGISTRATION_TOKEN on agent %s)\n", resp.Plaintext, agent)
	case "list", "ls":
		fmt.Print(string(mustAdminGet(base, "/admin/register-tokens")))
	case "revoke":
		// register revoke <token-id>
		if len(args) < 2 {
			registerUsage()
		}
		mustAdminDelete(base, "/admin/register-tokens/"+args[1])
		fmt.Printf("registration token %s revoked\n", args[1])
	default:
		registerUsage()
	}
}

func registerUsage() {
	fmt.Fprintln(os.Stderr, "usage: runtimectl register <mint --agent <id>|list|revoke <token-id>>")
	os.Exit(2)
}
```

- [ ] **Step 3: Build + smoke**

Run: `go build ./cmd/runtimectl/ && go vet ./cmd/runtimectl/`
Then: `go run ./cmd/runtimectl register` → prints usage, exits 2.
Expected: build clean; usage printed.

- [ ] **Step 4: Commit**

```bash
gofmt -w cmd/runtimectl/main.go
git add cmd/runtimectl/main.go
git commit -m "feat(runtimectl): register mint|list|revoke subcommands

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 6: agentd `fetchRegistration` prelude

**Files:**
- Create: `cmd/agentd/register.go`
- Modify: `cmd/agentd/main.go` (call `fetchRegistration()` first)
- Test: `cmd/agentd/register_test.go` (new)

- [ ] **Step 1: Write the failing test**

Create `cmd/agentd/register_test.go`:

```go
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestOrdinalFromHostname(t *testing.T) {
	cases := map[string]int{"support-0": 0, "support-3": 3, "x-y-12": 12, "nohyphen": 0, "": 0}
	for host, want := range cases {
		if got := ordinalFromHostname(host); got != want {
			t.Fatalf("ordinalFromHostname(%q)=%d want %d", host, got, want)
		}
	}
}

func TestFetchRegistrationSetsEnv(t *testing.T) {
	var gotOrdinal int
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		var body struct{ Ordinal int }
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotOrdinal = body.Ordinal
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"env": map[string]string{"RUNTIME_PG_DSN": "dsn://fetched", "OPENAI_API_KEY": "sk-1"},
		})
	}))
	defer srv.Close()

	t.Setenv("RUNTIME_REGISTRATION_URL", srv.URL)
	t.Setenv("RUNTIME_REGISTRATION_TOKEN", "svk-abc.def")
	t.Setenv("HOSTNAME", "support-2")
	// Ensure target var starts empty.
	os.Unsetenv("RUNTIME_PG_DSN")

	fetchRegistration()

	if gotAuth != "Bearer svk-abc.def" {
		t.Fatalf("auth header = %q", gotAuth)
	}
	if gotOrdinal != 2 {
		t.Fatalf("ordinal = %d want 2", gotOrdinal)
	}
	if os.Getenv("RUNTIME_PG_DSN") != "dsn://fetched" || os.Getenv("OPENAI_API_KEY") != "sk-1" {
		t.Fatalf("env not applied: DSN=%q KEY=%q", os.Getenv("RUNTIME_PG_DSN"), os.Getenv("OPENAI_API_KEY"))
	}
}

func TestFetchRegistrationNoopWhenUnset(t *testing.T) {
	os.Unsetenv("RUNTIME_REGISTRATION_URL")
	os.Unsetenv("RUNTIME_REGISTRATION_TOKEN")
	// Must not panic / must not block.
	fetchRegistration()
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/agentd/ -run 'TestOrdinal|TestFetchRegistration' -v`
Expected: FAIL — `ordinalFromHostname`/`fetchRegistration` undefined.

- [ ] **Step 3: Implement**

Create `cmd/agentd/register.go`:

```go
package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// ordinalFromHostname extracts the StatefulSet ordinal from a pod name
// ("<statefulset>-<ordinal>"). Returns 0 when there is no numeric suffix.
func ordinalFromHostname(host string) int {
	i := strings.LastIndexByte(host, '-')
	if i < 0 || i == len(host)-1 {
		return 0
	}
	n, err := strconv.Atoi(host[i+1:])
	if err != nil {
		return 0
	}
	return n
}

// fetchRegistration, when RUNTIME_REGISTRATION_URL and _TOKEN are both set,
// POSTs to the control plane and os.Setenv's every returned pair into this
// process's environment, BEFORE the normal os.Getenv startup path runs. A no-op
// when either var is unset (local spawns are byte-for-byte unchanged). Fails
// hard (log.Fatal) on any error — a pod that cannot fetch its config must not
// start with a partial environment; K8s will restart it.
func fetchRegistration() {
	url := os.Getenv("RUNTIME_REGISTRATION_URL")
	token := os.Getenv("RUNTIME_REGISTRATION_TOKEN")
	if url == "" || token == "" {
		return
	}
	ordinal := ordinalFromHostname(os.Getenv("HOSTNAME"))
	reqBody, _ := json.Marshal(map[string]int{"ordinal": ordinal})
	req, err := http.NewRequest("POST", url, bytes.NewReader(reqBody))
	if err != nil {
		log.Fatalf("agentd: build registration request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Fatalf("agentd: registration handshake to %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Fatalf("agentd: registration handshake to %s: status %s", url, resp.Status)
	}
	var out struct {
		Env map[string]string `json:"env"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		log.Fatalf("agentd: decode registration response: %v", err)
	}
	for k, v := range out.Env {
		if err := os.Setenv(k, v); err != nil {
			log.Fatalf("agentd: apply registration env %s: %v", k, err)
		}
	}
}
```

In `cmd/agentd/main.go`, add `fetchRegistration()` as the FIRST line of `main()` (before `mustEnv("RUNTIME_PG_DSN")`):

```go
func main() {
	// C3 M2: when launched as a remote/scheduled pod, pull DSN + identity +
	// feature env + brokered secrets from the control plane before reading env.
	fetchRegistration()

	dsn := mustEnv("RUNTIME_PG_DSN")
	// ... unchanged ...
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./cmd/agentd/ -run 'TestOrdinal|TestFetchRegistration' -v && go vet ./cmd/agentd/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -w cmd/agentd/register.go cmd/agentd/main.go cmd/agentd/register_test.go
git add cmd/agentd/register.go cmd/agentd/main.go cmd/agentd/register_test.go
git commit -m "feat(agentd): registration handshake prelude (fetch -> setenv -> unchanged path)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 7: Integration test — full handshake end-to-end

**Files:**
- Create: `test/registration_handshake_test.go` (`//go:build integration`)

**Context:** Model on `test/remote_pool_test.go` — it builds `agentd`+`runtimed`, writes a `runtime.yaml`, runs them, and drives the control plane over HTTP with helpers (`mustExec`, `rmtWaitHealthy`, `rmtGetAgents`, `dsn`). Reuse those helpers (same package `test`). This test instead runs an agentd in **handshake mode** (only `RUNTIME_REGISTRATION_URL`+`_TOKEN` in its env) and proves it boots with brokered config it never had in its env.

- [ ] **Step 1: Write the test**

Create `test/registration_handshake_test.go`:

```go
//go:build integration

package test

import (
	"testing"
	// plus the imports the helpers/below use: context, database/sql, encoding/json,
	// net/http, os, os/exec, path/filepath, strings, time, and the pgx driver.
)

// TestRegistrationHandshake proves a remote agentd in handshake mode pulls its
// DSN + identity + a brokered tenant secret from the control plane, boots, and
// serves a conformant session — and that a revoked / wrong-tenant token fails closed.
func TestRegistrationHandshake(t *testing.T) {
	// 0. Self-clean DB (sessions/events/dbos + identity tables + registration_tokens).
	// 1. Build agentd + runtimed (go build -o ...), like remote_pool_test.
	// 2. Set up identity + a secrets keyring on the control plane:
	//    - RUNTIME_SECRETS_KEYS = "k1:<base64 32B>" ; RUNTIME_SECRETS_PRIMARY=k1
	//    - RUNTIME_ADMIN_BOOTSTRAP = "<bootstrap admin key>"  (so /admin works)
	//    - runtime.yaml with one REMOTE agent:
	//        agents:
	//          - {id: research, name: Research, model: test/scripted, url: "http://127.0.0.1:8330", auth_token: "${REG_AGENT_BEARER}"}
	//      (single remote, no {i}; tenant defaults to "default")
	// 3. Start runtimed (env: RUNTIME_PG_DSN, the keyring vars, the bootstrap key,
	//    RUNTIME_AGENTD_BIN unused here). Wait /healthz 200.
	// 4. Via admin API (bootstrap bearer):
	//    - POST /admin/secrets {name:"OPENAI_API_KEY", value:"sk-secret", tenant:"default"}
	//    - POST /admin/register-tokens {agent:"research"} -> capture {plaintext}
	// 5. Start agentd with ONLY:
	//        RUNTIME_REGISTRATION_URL=http://127.0.0.1:<ctlport>/register
	//        RUNTIME_REGISTRATION_TOKEN=<plaintext>
	//        RUNTIME_AGENT_AUTH_TOKEN=<REG_AGENT_BEARER>   (M1 bearer agentd checks on inbound)
	//        HOSTNAME=research-0
	//    NOTE: NO RUNTIME_PG_DSN, NO RUNTIME_AGENT_ID, NO OPENAI_API_KEY in its env.
	//    It must fetch all of them. Wait for its /healthz (through runtimed proxy) 200.
	// 6. Assert the control plane shows research healthy (rmtGetAgents).
	// 7. Drive a conformant session through runtimed to research (create session +
	//    stream + get), proving agentd booted with the fetched DSN/id.
	// 8. Assert the brokered secret reached the agent process: the scripted kind
	//    need not echo it, so instead assert indirectly — agentd only starts if
	//    RUNTIME_PG_DSN was fetched (step 5 omitted it), so a healthy agent already
	//    proves the delta (which includes the secret) was applied. Optionally, if a
	//    debug agent kind that echoes an env var exists, use it; otherwise the boot
	//    itself is the proof and a comment must say so.
	// 9. Negative: revoke the token (DELETE /admin/register-tokens/{id}); start a
	//    SECOND agentd (HOSTNAME=research-0, same token) and assert it EXITS non-zero
	//    within a few seconds (log.Fatal path) — capture its exit error.
	// 10. Negative (wrong tenant): create tenant "other" + an admin scoped to it;
	//     that admin minting a token for "research" (tenant default) gets 403.
}
```

> Implementation notes for the executor: pick free ports (helper in remote_pool_test or net.Listen :0 then close). The control-plane port must be known to build the registration URL. For step 8, prefer the "healthy boot proves the fetch" argument unless an env-echoing agent kind already exists — do NOT add one just for this. Keep all assertions strict; only tolerate timing with bounded waits (`rmtWaitFor`).

- [ ] **Step 2: Run the integration test**

Run: `go test -tags integration ./test/ -run TestRegistrationHandshake -v`
Expected: PASS (agent boots from a near-empty env; revoked token → agentd exits non-zero; cross-tenant mint → 403).

- [ ] **Step 3: Commit**

```bash
gofmt -w test/registration_handshake_test.go
git add test/registration_handshake_test.go
git commit -m "test(integration): registration handshake end-to-end

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 8: Chart wiring (perAgentPods handshake mode)

**Files:**
- Modify: `deploy/charts/runtime/values.yaml` (registration toggle + token key)
- Modify: `deploy/charts/runtime/templates/agent-statefulset.yaml` (registration env)
- Modify: `deploy/charts/runtime/templates/secret.yaml` (registration token key)

- [ ] **Step 1: values.yaml**

Under `secrets:`, add:

```yaml
  registrationToken: ""   # RUNTIME_REGISTRATION_TOKEN — per-agent handshake bearer
                          # (perAgentPods). When set, agent pods pull DSN + brokered
                          # secrets from the control plane at boot instead of from
                          # this Secret. Mint with: runtimectl register mint --agent <id>.
                          # NOTE: with an existingSecret, that secret must carry the
                          # RUNTIME_REGISTRATION_TOKEN key when handshake mode is used.
```

Under `scheduling:` add documentation only (no new mode — handshake activates when a registration token is present):

```yaml
  # When secrets.registrationToken (or an existingSecret carrying
  # RUNTIME_REGISTRATION_TOKEN) is set in perAgentPods mode, agent pods perform
  # the C3 M2 registration handshake: they fetch DSN + identity + brokered
  # secrets from the control plane at boot. Otherwise they read everything from
  # this chart's Secret (C2 M2 behavior).
```

- [ ] **Step 2: agent-statefulset.yaml — registration env**

In the container `env:` list (after the `RUNTIME_AGENT_AUTH_TOKEN` block, around line 77), add a registration block gated on the token being available. The handshake supplies DSN, so when handshake is active the static `RUNTIME_PG_DSN` ref must become `optional: true` (it may be absent from the Secret). Use a `$handshake` flag:

```yaml
          {{- $handshake := or $root.Values.secrets.registrationToken (and $root.Values.secrets.existingSecret true) }}
```

Place that near the top of the container block (it can be computed once per agent in the range). Then add:

```yaml
            {{- if $handshake }}
            - name: RUNTIME_REGISTRATION_URL
              value: "http://{{ include "runtime.fullname" $root }}:{{ $root.Values.service.port }}/register"
            - name: RUNTIME_REGISTRATION_TOKEN
              valueFrom:
                secretKeyRef:
                  name: {{ include "runtime.secretName" $root }}
                  key: RUNTIME_REGISTRATION_TOKEN
                  optional: true
            {{- end }}
```

And make the existing `RUNTIME_PG_DSN` ref tolerate handshake mode by adding `optional: true` to its `secretKeyRef` (lines 66-69) — in handshake mode the DSN arrives via the fetch, so the Secret need not carry it:

```yaml
            - name: RUNTIME_PG_DSN
              valueFrom:
                secretKeyRef:
                  name: {{ include "runtime.secretName" $root }}
                  key: RUNTIME_PG_DSN
                  optional: true
```

> The `$HOSTNAME` sh-wrapper (lines 49-55) is unchanged — it still exports `RUNTIME_AGENT_REPLICA`/`DBOS__VMID` as the pre-handshake fallback; `fetchRegistration` overwrites them from the validated delta. Keep it.

> The registration-token Secret key is per-release (shared template Secret), not per-agent in the chart. A single `secrets.registrationToken` value is shared by all agent pods in this chart for simplicity; document that distinct per-agent tokens require an `existingSecret` with per-agent keys (out of chart scope, note in README). This matches how `agentAuthToken` is modeled (shared).

- [ ] **Step 3: secret.yaml — registration token key**

In `deploy/charts/runtime/templates/secret.yaml`, before the closing `{{- end }}`, add:

```yaml
  {{- if .Values.secrets.registrationToken }}
  RUNTIME_REGISTRATION_TOKEN: {{ .Values.secrets.registrationToken | quote }}
  {{- end }}
```

- [ ] **Step 4: Render checks**

Run:
```bash
cd deploy/charts/runtime
helm template t . --set scheduling.mode=perAgentPods \
  --set config.agents[0].id=support --set config.agents[0].listen_addr=":8080" \
  --set secrets.pgDsn="postgres://x" --set secrets.registrationToken="svk-a.b" \
  | grep -A2 RUNTIME_REGISTRATION_URL
```
Expected: the StatefulSet carries `RUNTIME_REGISTRATION_URL` + `RUNTIME_REGISTRATION_TOKEN`.

Then confirm monolith is unchanged:
```bash
helm template t . --set config.agents[0].id=a --set config.agents[0].listen_addr=":8080" \
  --set secrets.pgDsn="postgres://x" | grep -c RUNTIME_REGISTRATION_URL
```
Expected: `0`.

- [ ] **Step 5: Commit**

```bash
git add deploy/charts/runtime/values.yaml deploy/charts/runtime/templates/agent-statefulset.yaml deploy/charts/runtime/templates/secret.yaml
git commit -m "feat(chart): registration handshake env for perAgentPods agent pods

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 9: Chart test.sh permutations, README, ROADMAP

**Files:**
- Modify: `deploy/charts/runtime/test.sh`
- Modify: `deploy/charts/runtime/README.md`
- Modify: `ROADMAP.md`

- [ ] **Step 1: test.sh permutations**

Add cases asserting:
1. perAgentPods + `secrets.registrationToken` set → StatefulSet contains `RUNTIME_REGISTRATION_URL` and a `RUNTIME_REGISTRATION_TOKEN` secretKeyRef; Secret contains the `RUNTIME_REGISTRATION_TOKEN` key.
2. perAgentPods WITHOUT a registration token → StatefulSet has NO `RUNTIME_REGISTRATION_URL` (handshake off; C2 M2 behavior preserved).
3. monolith regression → no `/register` env anywhere.

Follow the existing assertion style in `test.sh` (grep the rendered output, `pass`/`fail` helpers).

- [ ] **Step 2: README**

Add a "Registration handshake (C3 M2)" subsection under the perAgentPods docs: what it does, how to mint a token (`runtimectl register mint --agent <id>`), the `secrets.registrationToken` (or per-agent `existingSecret`) wiring, and that handshake mode lets brokered per-tenant secrets reach scheduled pods (retiring the C2 M2 limitation). Note the shared-token simplification + the existingSecret per-agent-key path.

- [ ] **Step 3: ROADMAP entry**

Add a C3 M2 DONE entry under §C3 (after the C3 M1 paragraph), summarizing: pull handshake, per-agent identity-backed token + `runtimectl register`, `buildEnv`→`envDelta` split, `/register` mounted pre-identity, fail-closed ordinal validation, per-agent-pod gateway wired for free, bearer-over-operator-TLS (mTLS still deferred). Leave the live-proof results line as a placeholder to fill after the kind proof in Final Verification.

- [ ] **Step 4: Run the chart gate**

Run: `cd deploy/charts/runtime && ./test.sh`
Expected: `ALL CHART TESTS PASSED`.

- [ ] **Step 5: Commit**

```bash
git add deploy/charts/runtime/test.sh deploy/charts/runtime/README.md ROADMAP.md
git commit -m "docs(chart,roadmap): C3 M2 handshake docs + test permutations

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Final Verification

- [ ] **Hermetic gate**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: build clean, no FAIL. (Live/integration-tagged tests excluded.)

- [ ] **Integration gate** (Postgres.app running)

Run: `go test -tags integration ./internal/identity/ ./controlplane/ ./test/ -run 'Registration|Register|RegToken' -v`
Expected: PASS (handshake end-to-end, token CRUD).

- [ ] **Chart gate**

Run: `cd deploy/charts/runtime && ./test.sh`
Expected: `ALL CHART TESTS PASSED`.

- [ ] **Final holistic review** (MANDATORY — C2 precedent: each prior K8s milestone's holistic review caught an independent install-only bug)

Dispatch a fresh code-reviewer subagent (`model: "opus"`) over the entire diff (`git diff master...HEAD`). Focus areas: (1) does the `/register` response EVER include a value from runtimed's own `os.Environ()` (re-verify the `envDelta` split end-to-end)? (2) is `/register` truly mounted OUTSIDE the identity middleware in BOTH open and identity-on modes? (3) can a token for agent A in tenant X ever yield tenant Y's secrets (token→agent_id authoritative; cross-tenant mint blocked)? (4) does agentd fail HARD (not degrade) on every handshake error path? (5) any secret/secret-name in a log line or the access log? (6) chart: monolith byte-for-byte unchanged; DSN ref `optional` only where handshake supplies it; does an `existingSecret` without the token key fail safely? Address every HOLD/Critical before proceeding.

- [ ] **Live kind proof** (MANDATORY — C2 precedent)

On a real kind cluster with bundled Postgres:
1. `make docker-image` → `kind load docker-image ...`.
2. `helm install` with `postgresql.enabled=true`, `scheduling.mode=perAgentPods`, a keyring (`secrets.secretsKeys`/`secretsPrimary`), `secrets.adminBootstrap`, one pool agent (`replicas:2`) + one single agent, and `secrets.registrationToken=<minted>`. (Mint via a one-off `runtimectl register mint` against the running control plane, then `helm upgrade --set secrets.registrationToken=...`, OR pre-mint by exec'ing runtimectl in the control-plane pod — document the exact sequence used.)
3. Set a brokered secret: `kubectl exec` the control-plane pod → `runtimectl admin secret set OPENAI_API_KEY <v> --tenant default` (or via bootstrap bearer).
4. PROVE: agent pods reach Ready having pulled DSN + the brokered secret via `/register` (the pod env carries NO `RUNTIME_PG_DSN` from the chart Secret in handshake mode — verify with `kubectl exec ... env | grep -c RUNTIME_PG_DSN` showing it came from the fetch, or inspect the pod spec shows the DSN ref `optional`); `kubectl logs` the control plane shows `register` lines with `agent`/`ordinal`/`token_id` and NO secret values.
5. `runtimectl conformance` PASSES against both the pool and single agent through the in-cluster Service.
6. Negative: `runtimectl register revoke <token-id>` then delete a pod → it CrashLoops (handshake 401 → `log.Fatal`), proving fail-closed; restore by re-minting.
7. Clean `helm uninstall` + `kind delete`.

Record the exact results into the ROADMAP C3 M2 entry's live-proof line.

- [ ] **Finish the branch**

Use superpowers:finishing-a-development-branch to verify tests and present merge options.
