//go:build integration

package test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sausheong/runtime/internal/identity"
)

// oauth2Spec is a tiny OpenAPI 3.0 doc with a single GET operation (whoami)
// that echoes the Authorization header the upstream received. servers: [] forces
// the request base URL to come from the config base_url.
const oauth2Spec = `
openapi: 3.0.3
info: {title: Orders, version: "1.0"}
servers: []
paths:
  /whoami:
    get:
      operationId: whoami
      summary: Echo the received Authorization header as JSON
      responses: {"200": {description: ok}}
`

// oauth2DropQueries resets every table this test can touch. tenants is dropped
// with CASCADE (it owns secrets + gateway_upstreams via FKs), but the child
// re-CREATEs its tables IF NOT EXISTS on boot, so we drop the dependents
// explicitly first to guarantee a clean slate across reruns.
var oauth2DropQueries = []string{
	`DROP TABLE IF EXISTS gateway_upstreams CASCADE`,
	`DROP TABLE IF EXISTS secrets CASCADE`,
	`DROP TABLE IF EXISTS gateway_quotas CASCADE`,
	`DROP TABLE IF EXISTS gateway_policies CASCADE`,
	`DROP TABLE IF EXISTS service_keys CASCADE`,
	`DROP TABLE IF EXISTS identity_users CASCADE`,
	`DROP TABLE IF EXISTS tenants CASCADE`,
}

// oauth2ResetDB clears the runtime + identity + gateway tables so a boot starts
// from a known state. Mirrors quotaResetDB but also clears secrets/upstreams.
func oauth2ResetDB(t *testing.T, db *sql.DB) {
	t.Helper()
	mustExec(t, db, `DROP TABLE IF EXISTS markers`)
	mustExec(t, db, `CREATE TABLE markers (id BIGSERIAL PRIMARY KEY, ran_at TIMESTAMPTZ)`)
	mustExec(t, db, `DROP TABLE IF EXISTS session_events, sessions, agents CASCADE`)
	mustExec(t, db, `DROP SCHEMA IF EXISTS dbos CASCADE`)
	for _, q := range oauth2DropQueries {
		mustExec(t, db, q)
	}
}

// oauth2TokenSrv is a fake OAuth2 client_credentials token endpoint. Each
// successful hit mints a NEW, monotonically distinct access token ("tok-N") and
// increments hits — so a single mint reused within its TTL keeps hits at 1,
// while a re-mint (generation bump) both advances hits and yields a new token.
// When fail is set it returns 500 so the manager fails to mint (fail-closed).
type oauth2TokenSrv struct {
	*httptest.Server
	hits atomic.Int32
	fail atomic.Bool
}

func newOAuth2TokenSrv(t *testing.T) *oauth2TokenSrv {
	t.Helper()
	ts := &oauth2TokenSrv{}
	ts.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ts.fail.Load() {
			http.Error(w, `{"error":"server_error"}`, http.StatusInternalServerError)
			return
		}
		n := ts.hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"access_token":"tok-%d","token_type":"Bearer","expires_in":3600}`, n)
	}))
	t.Cleanup(ts.Close)
	return ts
}

// oauth2UpstreamSrv is the fake REST upstream: it serves the OpenAPI spec at
// /openapi.yaml and, at /whoami, RECORDS + echoes the Authorization header it
// received. hits counts /whoami requests so we can prove fail-closed never
// dispatches an uncredentialed request.
type oauth2UpstreamSrv struct {
	*httptest.Server
	mu       sync.Mutex
	lastAuth string
	hits     atomic.Int32
}

func newOAuth2UpstreamSrv(t *testing.T) *oauth2UpstreamSrv {
	t.Helper()
	up := &oauth2UpstreamSrv{}
	up.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/openapi.yaml" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/yaml")
			_, _ = w.Write([]byte(oauth2Spec))
		case r.URL.Path == "/whoami" && r.Method == http.MethodGet:
			up.hits.Add(1)
			up.mu.Lock()
			up.lastAuth = r.Header.Get("Authorization")
			up.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"auth": r.Header.Get("Authorization")})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(up.Close)
	return up
}

// newSecretKey returns a fresh 32-byte key and its std-base64 encoding for the
// RUNTIME_SECRETS_KEYS env of the child runtimed. The in-process broker built
// over the raw bytes seals credentials the child (same key) can decrypt.
func newSecretKey(t *testing.T) (raw []byte, b64 string) {
	t.Helper()
	raw = make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("gen secret key: %v", err)
	}
	return raw, base64.StdEncoding.EncodeToString(raw)
}

// inProcessBroker builds a broker over key so a credential can be seeded BEFORE
// the child boots (the child, given the same key via RUNTIME_SECRETS_KEYS,
// decrypts it). Mirrors the seed-before-boot pattern in secrets_e2e_test.go.
func inProcessBroker(t *testing.T, ctx context.Context, db *sql.DB, key []byte) *identity.Broker {
	t.Helper()
	st, err := identity.NewStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := identity.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	kr, err := identity.NewKeyring(map[string]*identity.Cipher{"v1": cipher}, "v1", "v1")
	if err != nil {
		t.Fatal(err)
	}
	return identity.NewBroker(st, kr)
}

// bootRuntimed builds+starts a runtimed child with the broker enabled (secrets
// key env) and returns its base URL after /healthz is up. Cleanup kills the
// process group.
func bootRuntimed(t *testing.T, tmp, cfgPath, ctlAddr, keyB64 string) string {
	t.Helper()
	agentd := buildBin(t, tmp, "agentd")
	runtimed := buildBin(t, tmp, "runtimed")
	cmd := exec.Command(runtimed)
	cmd.Env = append(os.Environ(),
		"RUNTIME_PG_DSN="+dsn,
		"RUNTIME_CTL_ADDR="+ctlAddr,
		"RUNTIME_AGENTD_BIN="+agentd,
		"RUNTIME_CONFIG="+cfgPath,
		"RUNTIME_SECRETS_KEYS=v1:"+keyB64,
		"RUNTIME_SECRETS_PRIMARY=v1",
	)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); _, _ = cmd.Process.Wait() })
	base := "http://" + ctlAddr
	waitURL(t, base+"/healthz", 15*time.Second)
	return base
}

// whoamiAuth drives the orders__whoami tool once as sess and returns the
// Authorization header the upstream recorded (via the REST envelope body).
func whoamiAuth(t *testing.T, sess *sdk.ClientSession) string {
	t.Helper()
	var env restEnvelope
	callJSON(t, sess, "orders__whoami", nil, &env)
	if env.Status != 200 {
		t.Fatalf("whoami envelope status = %d, want 200 (body=%s)", env.Status, env.Body)
	}
	var body struct {
		Auth string `json:"auth"`
	}
	if err := json.Unmarshal(env.Body, &body); err != nil {
		t.Fatalf("whoami body is not the expected JSON (%v): %s", err, env.Body)
	}
	return body.Auth
}

// TestOAuth2CredentialEndToEnd boots the whole stack with the broker enabled and
// an openapi upstream whose cred_secret is an oauth2 client_credentials cred. It
// proves the three happy-path spec cases against a live runtimed:
//   - (1) mint → inject: the upstream receives "Bearer tok-...";
//   - (2) reuse within TTL: a second call does NOT re-hit the token endpoint;
//   - (3) live rotation: after the acme admin rewrites the cred (generation
//     bump), the next call mints and uses a NEW token.
func TestOAuth2CredentialEndToEnd(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatalf("ping postgres (%s): %v", dsn, err)
	}
	oauth2ResetDB(t, db)
	t.Cleanup(func() {
		cdb, err := sql.Open("pgx", dsn)
		if err != nil {
			return
		}
		defer cdb.Close()
		for _, q := range oauth2DropQueries {
			_, _ = cdb.Exec(q)
		}
	})

	key, keyB64 := newSecretKey(t)

	// Identity: acme tenant with an ADMIN key. Admin ≥ operator, so it can both
	// drive the gateway tool AND POST /admin/secrets to rotate the cred live.
	st, err := identity.NewStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTenant(ctx, "acme", "Acme"); err != nil {
		t.Fatal(err)
	}
	acmeAdmin, _ := identity.MintServiceKey()
	if err := st.InsertServiceKey(ctx, acmeAdmin.ID, "acme", acmeAdmin.Hash, identity.RoleAdmin, "acme-admin"); err != nil {
		t.Fatal(err)
	}

	tokenSrv := newOAuth2TokenSrv(t)
	up := newOAuth2UpstreamSrv(t)

	// Seed the oauth2 credential BEFORE boot, sealed under the same key the child
	// will use. The child decrypts it and mints tokens against tokenSrv.
	broker := inProcessBroker(t, ctx, db, key)
	if err := broker.SetOAuth2(ctx, "acme", "orders_oauth", identity.OAuth2Config{
		TokenURL:     tokenSrv.URL + "/token",
		ClientID:     "orders-client",
		ClientSecret: "s3cr3t",
		Scopes:       []string{"orders.read"},
	}); err != nil {
		t.Fatalf("seed oauth2 credential: %v", err)
	}

	// File config: acme-owned agent + the openapi upstream scoped to acme with the
	// oauth2 cred attached. base_url required (spec declares servers: []).
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "runtime.yaml")
	cfg := "agents:\n" +
		"  - {id: a1, name: A1, model: test/scripted, listen_addr: 127.0.0.1:8481, tenant: acme}\n" +
		"gateway:\n" +
		"  servers:\n" +
		"    - name: orders\n" +
		"      openapi: " + up.URL + "/openapi.yaml\n" +
		"      base_url: " + up.URL + "\n" +
		"      tenants: [acme]\n" +
		"      cred_secret: orders_oauth\n" +
		"      cred_header: Authorization\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	base := bootRuntimed(t, tmp, cfgPath, "127.0.0.1:8480", keyB64)

	acme := connectWhenFederated(t, base, acmeAdmin.Plaintext, "orders__whoami")

	// (1) Mint → inject: the upstream must have received a Bearer token minted
	// from the oauth2 cred (never a static/empty header).
	auth1 := whoamiAuth(t, acme)
	if !strings.HasPrefix(auth1, "Bearer tok-") {
		t.Fatalf("case 1 mint→inject: upstream Authorization = %q, want \"Bearer tok-...\"", auth1)
	}
	if got := tokenSrv.hits.Load(); got != 1 {
		t.Fatalf("case 1: token endpoint hits = %d after first call, want 1", got)
	}

	// (2) Reuse within TTL: a second call must reuse the cached token — the token
	// endpoint is NOT hit again, and the upstream sees the SAME token.
	auth2 := whoamiAuth(t, acme)
	if got := tokenSrv.hits.Load(); got != 1 {
		t.Fatalf("case 2 reuse: token endpoint hits = %d after second call, want 1 (token must be cached)", got)
	}
	if auth2 != auth1 {
		t.Fatalf("case 2 reuse: second call auth = %q, want same cached token %q", auth2, auth1)
	}

	// (3) Live rotation: the acme admin rewrites the cred via the admin API (this
	// runs against the CHILD's own broker, so ITS generation bumps). The next
	// call must re-mint and inject a NEW token.
	adminPost(t, "127.0.0.1:8480", acmeAdmin.Plaintext, "/admin/secrets", map[string]any{
		"name":          "orders_oauth",
		"type":          identity.CredTypeOAuth2,
		"token_url":     tokenSrv.URL + "/token",
		"client_id":     "orders-client-v2",
		"client_secret": "s3cr3t",
		"scopes":        []string{"orders.read"},
		"tenant":        "acme",
	}, http.StatusOK)

	// Poll: the generation bump is read live by gate #5 on the next call, but use
	// a short deadline to be robust against fresh-session propagation.
	rotated := false
	deadline := time.Now().Add(10 * time.Second)
	var auth3 string
	for time.Now().Before(deadline) {
		sess := connectGatewayAs(t, base, acmeAdmin.Plaintext)
		auth3 = whoamiAuth(t, sess)
		_ = sess.Close()
		if auth3 != auth1 {
			rotated = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !rotated {
		t.Fatalf("case 3 rotation: after admin cred rewrite the call must use a NEW token, still saw %q", auth3)
	}
	if !strings.HasPrefix(auth3, "Bearer tok-") {
		t.Fatalf("case 3 rotation: post-rotate Authorization = %q, want \"Bearer tok-...\"", auth3)
	}
	if got := tokenSrv.hits.Load(); got < 2 {
		t.Fatalf("case 3 rotation: token endpoint hits = %d, want ≥2 (re-mint after rotation)", got)
	}
}

// TestOAuth2CredentialFailClosed boots the stack with the token endpoint always
// returning 500. The tool call must fail CLOSED: the MCP result is an error
// carrying "credential unavailable", the upstream is NEVER dispatched to, and
// the fail-closed metric is emitted.
func TestOAuth2CredentialFailClosed(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatalf("ping postgres (%s): %v", dsn, err)
	}
	oauth2ResetDB(t, db)
	t.Cleanup(func() {
		cdb, err := sql.Open("pgx", dsn)
		if err != nil {
			return
		}
		defer cdb.Close()
		for _, q := range oauth2DropQueries {
			_, _ = cdb.Exec(q)
		}
	})

	key, keyB64 := newSecretKey(t)

	st, err := identity.NewStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTenant(ctx, "acme", "Acme"); err != nil {
		t.Fatal(err)
	}
	acmeOp, _ := identity.MintServiceKey()
	if err := st.InsertServiceKey(ctx, acmeOp.ID, "acme", acmeOp.Hash, identity.RoleOperator, "acme-op"); err != nil {
		t.Fatal(err)
	}

	tokenSrv := newOAuth2TokenSrv(t)
	tokenSrv.fail.Store(true) // every mint attempt 500s
	up := newOAuth2UpstreamSrv(t)

	broker := inProcessBroker(t, ctx, db, key)
	if err := broker.SetOAuth2(ctx, "acme", "orders_oauth", identity.OAuth2Config{
		TokenURL:     tokenSrv.URL + "/token",
		ClientID:     "orders-client",
		ClientSecret: "s3cr3t",
	}); err != nil {
		t.Fatalf("seed oauth2 credential: %v", err)
	}

	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "runtime.yaml")
	cfg := "agents:\n" +
		"  - {id: a1, name: A1, model: test/scripted, listen_addr: 127.0.0.1:8483, tenant: acme}\n" +
		"gateway:\n" +
		"  servers:\n" +
		"    - name: orders\n" +
		"      openapi: " + up.URL + "/openapi.yaml\n" +
		"      base_url: " + up.URL + "\n" +
		"      tenants: [acme]\n" +
		"      cred_secret: orders_oauth\n" +
		"      cred_header: Authorization\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	base := bootRuntimed(t, tmp, cfgPath, "127.0.0.1:8482", keyB64)

	acme := connectWhenFederated(t, base, acmeOp.Plaintext, "orders__whoami")

	// (4) Fail closed: the mint fails (token endpoint 500), so the tool call must
	// come back IsError with "credential unavailable" and NEVER dispatch upstream.
	res, err := acme.CallTool(ctx, &sdk.CallToolParams{Name: "orders__whoami", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("call transport error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("case 4 fail-closed: call must be an error when the credential cannot be minted: %+v", res.Content)
	}
	if len(res.Content) == 0 {
		t.Fatal("case 4 fail-closed: error result must carry content")
	}
	tc, ok := res.Content[0].(*sdk.TextContent)
	if !ok {
		t.Fatalf("case 4 fail-closed: content must be text, got %T", res.Content[0])
	}
	if !strings.Contains(tc.Text, "credential unavailable") {
		t.Fatalf("case 4 fail-closed: error must cite \"credential unavailable\", got %q", tc.Text)
	}
	if got := up.hits.Load(); got != 0 {
		t.Fatalf("case 4 fail-closed: upstream received %d requests, want 0 (must never be called without the credential)", got)
	}

	// Metric: the fail-closed path increments runtime_gateway_credential_errors_total.
	metrics := getBody(t, base+"/metrics", nil, 200)
	if !strings.Contains(metrics, `runtime_gateway_credential_errors_total{server="orders",tenant="acme"}`) {
		t.Fatalf("case 4 fail-closed: /metrics missing the credential-error series:\n%s", metrics)
	}
}

// TestOAuth2CredentialOpenAPIOnly proves an oauth2 credential can only be
// attached to an openapi upstream: registering a url: (MCP-over-HTTP) upstream
// that names the oauth2 cred is rejected at the registration API with a 4xx
// error mentioning "openapi" (Task 4's checkOAuth2Openapi). The cred must exist
// first so credType resolves to oauth2.
func TestOAuth2CredentialOpenAPIOnly(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatalf("ping postgres (%s): %v", dsn, err)
	}
	oauth2ResetDB(t, db)
	t.Cleanup(func() {
		cdb, err := sql.Open("pgx", dsn)
		if err != nil {
			return
		}
		defer cdb.Close()
		for _, q := range oauth2DropQueries {
			_, _ = cdb.Exec(q)
		}
	})

	key, keyB64 := newSecretKey(t)

	st, err := identity.NewStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTenant(ctx, "acme", "Acme"); err != nil {
		t.Fatal(err)
	}
	acmeAdmin, _ := identity.MintServiceKey()
	if err := st.InsertServiceKey(ctx, acmeAdmin.ID, "acme", acmeAdmin.Hash, identity.RoleAdmin, "acme-admin"); err != nil {
		t.Fatal(err)
	}

	tokenSrv := newOAuth2TokenSrv(t)

	// Seed the oauth2 cred so the registration-time credType lookup resolves to
	// oauth2 (an unknown cred would be skipped and NOT rejected).
	broker := inProcessBroker(t, ctx, db, key)
	if err := broker.SetOAuth2(ctx, "acme", "orders_oauth", identity.OAuth2Config{
		TokenURL:     tokenSrv.URL + "/token",
		ClientID:     "orders-client",
		ClientSecret: "s3cr3t",
	}); err != nil {
		t.Fatalf("seed oauth2 credential: %v", err)
	}

	// No file upstreams needed; the broker being enabled activates the gateway so
	// /admin/upstreams is mounted. One acme agent satisfies the config requirement.
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "runtime.yaml")
	cfg := "agents:\n" +
		"  - {id: a1, name: A1, model: test/scripted, listen_addr: 127.0.0.1:8485, tenant: acme}\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	base := bootRuntimed(t, tmp, cfgPath, "127.0.0.1:8484", keyB64)
	_ = base

	// (5) OpenAPI-only: register a url: upstream naming the oauth2 cred → rejected
	// with a 4xx mentioning "openapi".
	body, _ := json.Marshal(map[string]any{
		"name":        "badmcp",
		"url":         "http://127.0.0.1:9/mcp", // any url: transport
		"cred_secret": "orders_oauth",
		"cred_header": "Authorization",
		"tenant":      "acme",
	})
	resp := authReq(t, "POST", base+"/admin/upstreams", acmeAdmin.Plaintext, strings.NewReader(string(body)))
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	rb := string(raw)
	if resp.StatusCode < 400 || resp.StatusCode >= 500 {
		t.Fatalf("case 5 openapi-only: url:+oauth2 registration must be a 4xx, got %d body=%s", resp.StatusCode, rb)
	}
	if !strings.Contains(rb, "openapi") {
		t.Fatalf("case 5 openapi-only: rejection must mention \"openapi\", got: %s", rb)
	}
}
