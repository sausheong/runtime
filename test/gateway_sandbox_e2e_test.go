//go:build integration

package test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sausheong/runtime/internal/identity"
)

// bearerRT is an http.RoundTripper that adds a Bearer credential to every
// request — how an MCP client authenticates to /gateway/mcp in enforced mode.
type bearerRT struct {
	token string
}

func (b bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// connectGatewayAs opens an authed MCP session to the gateway endpoint.
func connectGatewayAs(t *testing.T, base, key string) *sdk.ClientSession {
	t.Helper()
	cli := sdk.NewClient(&sdk.Implementation{Name: "sandbox-e2e", Version: "v0"}, nil)
	sess, err := cli.Connect(context.Background(), &sdk.StreamableClientTransport{
		Endpoint:   base + "/gateway/mcp",
		HTTPClient: &http.Client{Transport: bearerRT{token: key}},
	}, nil)
	if err != nil {
		t.Fatalf("connect gateway (key %s...): %v", key[:8], err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

// connectWhenFederated polls with FRESH sessions until want tools are all
// listed, returning the session that saw them. Fresh sessions per attempt
// because an MCP session pins its SDK server at creation: a session created
// before the async stdio upstream dial completes keeps the empty tool view
// forever (the generation-keyed server cache only serves NEW sessions).
func connectWhenFederated(t *testing.T, base, key string, want ...string) *sdk.ClientSession {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		sess := connectGatewayAs(t, base, key)
		lt, err := sess.ListTools(context.Background(), &sdk.ListToolsParams{})
		if err != nil {
			t.Fatalf("list tools: %v", err)
		}
		seen := map[string]bool{}
		for _, tool := range lt.Tools {
			seen[tool.Name] = true
		}
		all := true
		for _, w := range want {
			all = all && seen[w]
		}
		if all {
			return sess
		}
		_ = sess.Close()
		if time.Now().After(deadline) {
			t.Fatalf("tools %v never federated; last list: %+v", want, lt.Tools)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// callJSON calls a tool and decodes its single JSON TextContent into out.
// Fails the test on protocol error or IsError.
func callJSON(t *testing.T, sess *sdk.ClientSession, name string, args map[string]any, out any) {
	t.Helper()
	res, err := sess.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("call %s errored: %+v", name, res.Content)
	}
	if len(res.Content) != 1 {
		t.Fatalf("call %s: want exactly 1 content part, got %d", name, len(res.Content))
	}
	tc, ok := res.Content[0].(*sdk.TextContent)
	if !ok {
		t.Fatalf("call %s: want *sdk.TextContent, got %T", name, res.Content[0])
	}
	if err := json.Unmarshal([]byte(tc.Text), out); err != nil {
		t.Fatalf("call %s: result is not JSON (%v): %q", name, err, tc.Text)
	}
}

// TestGatewaySandboxE2E boots the WHOLE stack with identity ENFORCED and a
// forward_tenant sandboxd upstream (fake in-memory backend, no Docker):
// runtimed spawns sandboxd over stdio, two MCP clients authenticate to
// /gateway/mcp as different tenants, and the assertions prove federation,
// tenant scoping, and spoof-proofing — a caller-supplied __rt_tenant is
// stripped and overridden by the authenticated principal's tenant.
func TestGatewaySandboxE2E(t *testing.T) {
	ctx := context.Background()
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
	for _, q := range []string{
		`DROP TABLE IF EXISTS service_keys CASCADE`,
		`DROP TABLE IF EXISTS identity_users CASCADE`,
		`DROP TABLE IF EXISTS tenants CASCADE`,
	} {
		mustExec(t, db, q)
	}
	// Re-drop the identity tables at test end so we leave the shared DB as we
	// found it: leftover tenant/key rows make AnyConfigured() true, flipping
	// runtimed into enforced mode for sibling integration tests whose
	// unauthenticated probes then 401. Fresh connection because the deferred
	// db.Close() above runs before t.Cleanup functions.
	t.Cleanup(func() {
		cdb, err := sql.Open("pgx", dsn)
		if err != nil {
			return
		}
		defer cdb.Close()
		for _, q := range []string{
			`DROP TABLE IF EXISTS service_keys CASCADE`,
			`DROP TABLE IF EXISTS identity_users CASCADE`,
			`DROP TABLE IF EXISTS tenants CASCADE`,
		} {
			_, _ = cdb.Exec(q)
		}
	})

	// Identity: two tenants, one operator service key each. Rows in these
	// tables flip runtimed (spawned below) into ENFORCED mode.
	st, err := identity.NewStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTenant(ctx, "alpha", "Alpha"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTenant(ctx, "beta", "Beta"); err != nil {
		t.Fatal(err)
	}
	alphaKey, err := identity.MintServiceKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.InsertServiceKey(ctx, alphaKey.ID, "alpha", alphaKey.Hash, identity.RoleOperator, "alpha-op"); err != nil {
		t.Fatal(err)
	}
	betaKey, err := identity.MintServiceKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.InsertServiceKey(ctx, betaKey.ID, "beta", betaKey.Hash, identity.RoleOperator, "beta-op"); err != nil {
		t.Fatal(err)
	}

	// Build binaries.
	tmp := t.TempDir()
	agentd := filepath.Join(tmp, "agentd")
	if out, err := exec.Command("go", "build", "-o", agentd, "../cmd/agentd").CombinedOutput(); err != nil {
		t.Fatalf("build agentd: %v\n%s", err, out)
	}
	runtimed := filepath.Join(tmp, "runtimed")
	if out, err := exec.Command("go", "build", "-o", runtimed, "../cmd/runtimed").CombinedOutput(); err != nil {
		t.Fatalf("build runtimed: %v\n%s", err, out)
	}
	sandboxd := filepath.Join(tmp, "sandboxd")
	if out, err := exec.Command("go", "build", "-o", sandboxd, "../cmd/sandboxd").CombinedOutput(); err != nil {
		t.Fatalf("build sandboxd: %v\n%s", err, out)
	}

	// Config: one alpha-owned agent (not gateway-enabled, so no agent_keys
	// needed under enforced identity) + the sandboxd stdio upstream with
	// forward_tenant and the fake in-memory backend.
	cfgPath := filepath.Join(tmp, "runtime.yaml")
	cfg := "agents:\n" +
		"  - {id: a1, name: A1, model: test/scripted, listen_addr: 127.0.0.1:8151, tenant: alpha}\n" +
		"gateway:\n" +
		"  servers:\n" +
		"    - name: sandbox\n" +
		"      command: " + sandboxd + "\n" +
		"      forward_tenant: true\n" +
		"      env: {RUNTIME_SANDBOX_FAKE: \"1\"}\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	ctlAddr := "127.0.0.1:8150"
	cmd := exec.Command(runtimed)
	cmd.Env = append(os.Environ(),
		"RUNTIME_PG_DSN="+dsn,
		"RUNTIME_CTL_ADDR="+ctlAddr,
		"RUNTIME_AGENTD_BIN="+agentd,
		"RUNTIME_CONFIG="+cfgPath,
	)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	base := "http://" + ctlAddr
	// /healthz is exempt from identity, so the plain waiter works even in
	// enforced mode.
	waitURL(t, base+"/healthz", 15*time.Second)

	// Two MCP clients: one per tenant, authed via Bearer service keys on the
	// streamable transport's HTTPClient. The sandboxd stdio dial is async
	// (supervision loop), so poll with fresh sessions until the federated
	// sandbox tools appear.
	alphaSess := connectWhenFederated(t, base, alphaKey.Plaintext,
		"sandbox__create_sandbox", "sandbox__execute_code")
	betaSess := connectWhenFederated(t, base, betaKey.Plaintext,
		"sandbox__list_sandboxes", "sandbox__execute_code")

	// (a) alpha creates a sandbox WITH a spoofed __rt_tenant — the gateway
	// must strip it and inject the authenticated tenant (alpha) instead.
	var created struct {
		SandboxID string `json:"sandbox_id"`
		ExpiresAt string `json:"expires_at"`
	}
	callJSON(t, alphaSess, "sandbox__create_sandbox",
		map[string]any{"__rt_tenant": "beta"}, &created)
	if !strings.HasPrefix(created.SandboxID, "sbx-") {
		t.Fatalf("sandbox_id %q lacks sbx- prefix", created.SandboxID)
	}

	// (b) alpha executes code in its sandbox.
	var execRes struct {
		Stdout   string `json:"stdout"`
		ExitCode int    `json:"exit_code"`
	}
	callJSON(t, alphaSess, "sandbox__execute_code",
		map[string]any{"sandbox_id": created.SandboxID, "code": "print(42)"}, &execRes)

	// (c) beta lists sandboxes: ZERO. Proves both that the spoof in (a) did
	// not land the sandbox under beta and that list is tenant-scoped.
	var listed struct {
		Sandboxes []map[string]any `json:"sandboxes"`
	}
	callJSON(t, betaSess, "sandbox__list_sandboxes", nil, &listed)
	if len(listed.Sandboxes) != 0 {
		t.Fatalf("beta sees %d sandboxes, want 0 (tenant leak): %+v", len(listed.Sandboxes), listed.Sandboxes)
	}

	// (d) beta cannot use alpha's sandbox id — existence hidden (IsError).
	res, err := betaSess.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "sandbox__execute_code",
		Arguments: map[string]any{"sandbox_id": created.SandboxID, "code": "print(1)"},
	})
	if err != nil {
		t.Fatalf("beta cross-tenant exec (protocol): %v", err)
	}
	if !res.IsError {
		t.Fatalf("beta executed in alpha's sandbox — cross-tenant access: %+v", res.Content)
	}

	// (e) alpha write_file then read_file round-trip.
	var wrote struct {
		Path  string `json:"path"`
		Bytes int    `json:"bytes"`
	}
	callJSON(t, alphaSess, "sandbox__write_file",
		map[string]any{"sandbox_id": created.SandboxID, "path": "r.txt", "content": "result"}, &wrote)
	var read struct {
		Content   string `json:"content"`
		Truncated bool   `json:"truncated"`
	}
	callJSON(t, alphaSess, "sandbox__read_file",
		map[string]any{"sandbox_id": created.SandboxID, "path": "r.txt"}, &read)
	if read.Content != "result" {
		t.Fatalf("read_file content = %q, want %q", read.Content, "result")
	}
}
