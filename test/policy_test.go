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
	"syscall"
	"testing"
	"time"

	"net/http/httptest"

	_ "github.com/jackc/pgx/v5/stdlib"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sausheong/runtime/internal/identity"
)

// buildBin compiles a cmd into tmp and returns its path.
func buildBin(t *testing.T, tmp, name string) string {
	t.Helper()
	bin := filepath.Join(tmp, name)
	if out, err := exec.Command("go", "build", "-o", bin, "../cmd/"+name).CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", name, err, out)
	}
	return bin
}

// policyResetDB drops the shared tables so the test starts clean and leaves the
// DB as it found it (identity rows flip runtimed into enforced mode for
// siblings otherwise).
func policyResetDB(t *testing.T, db *sql.DB) {
	t.Helper()
	mustExec(t, db, `DROP TABLE IF EXISTS markers`)
	mustExec(t, db, `CREATE TABLE markers (id BIGSERIAL PRIMARY KEY, ran_at TIMESTAMPTZ)`)
	mustExec(t, db, `DROP TABLE IF EXISTS session_events, sessions, agents CASCADE`)
	mustExec(t, db, `DROP SCHEMA IF EXISTS dbos CASCADE`)
	for _, q := range []string{
		`DROP TABLE IF EXISTS gateway_policies CASCADE`,
		`DROP TABLE IF EXISTS service_keys CASCADE`,
		`DROP TABLE IF EXISTS identity_users CASCADE`,
		`DROP TABLE IF EXISTS tenants CASCADE`,
	} {
		mustExec(t, db, q)
	}
}

// fakeUpstreamSrv returns a Streamable HTTP MCP server exposing one "run" tool
// under server name "sbx" (so federated tool name is sbx__run).
func fakePolicyUpstream(t *testing.T) string {
	t.Helper()
	up := sdk.NewServer(&sdk.Implementation{Name: "sbx-upstream", Version: "v0"}, nil)
	up.AddTool(&sdk.Tool{
		Name:        "run",
		Description: "runs code",
		InputSchema: map[string]any{"type": "object"},
	}, func(_ context.Context, _ *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "ran"}}}, nil
	})
	srv := httptest.NewServer(sdk.NewStreamableHTTPHandler(
		func(*http.Request) *sdk.Server { return up }, nil))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestPolicyLifecycle: engine on with no policies ⇒ allowed; tenant-A admin
// adds a forbid via the API ⇒ next call denied WITHOUT restart; tenant B
// unaffected; list is tenant-scoped; delete ⇒ allowed again; decision metric
// present.
func TestPolicyLifecycle(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatalf("ping postgres (%s): %v", dsn, err)
	}
	policyResetDB(t, db)
	t.Cleanup(func() {
		cdb, err := sql.Open("pgx", dsn)
		if err != nil {
			return
		}
		defer cdb.Close()
		for _, q := range []string{
			`DROP TABLE IF EXISTS gateway_policies CASCADE`,
			`DROP TABLE IF EXISTS service_keys CASCADE`,
			`DROP TABLE IF EXISTS identity_users CASCADE`,
			`DROP TABLE IF EXISTS tenants CASCADE`,
		} {
			_, _ = cdb.Exec(q)
		}
	})

	// Identity: two tenants; tenant-A has an ADMIN key (to write policies) and
	// tenant-B an operator key. The upstream is visible to both tenants.
	st, err := identity.NewStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTenant(ctx, "acme", "Acme"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTenant(ctx, "globex", "Globex"); err != nil {
		t.Fatal(err)
	}
	acmeAdmin, _ := identity.MintServiceKey()
	if err := st.InsertServiceKey(ctx, acmeAdmin.ID, "acme", acmeAdmin.Hash, identity.RoleAdmin, "acme-admin"); err != nil {
		t.Fatal(err)
	}
	globexOp, _ := identity.MintServiceKey()
	if err := st.InsertServiceKey(ctx, globexOp.ID, "globex", globexOp.Hash, identity.RoleOperator, "globex-op"); err != nil {
		t.Fatal(err)
	}

	up := fakePolicyUpstream(t)

	tmp := t.TempDir()
	agentd := buildBin(t, tmp, "agentd")
	runtimed := buildBin(t, tmp, "runtimed")

	cfgPath := filepath.Join(tmp, "runtime.yaml")
	cfg := "agents:\n" +
		"  - {id: a1, name: A1, model: test/scripted, listen_addr: 127.0.0.1:8191, tenant: acme}\n" +
		"gateway:\n" +
		"  servers:\n" +
		"    - {name: sbx, url: " + up + "}\n" // no tenants: ⇒ visible to all
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	ctlAddr := "127.0.0.1:8190"
	cmd := exec.Command(runtimed)
	cmd.Env = append(os.Environ(),
		"RUNTIME_PG_DSN="+dsn,
		"RUNTIME_CTL_ADDR="+ctlAddr,
		"RUNTIME_AGENTD_BIN="+agentd,
		"RUNTIME_CONFIG="+cfgPath,
		"RUNTIME_POLICY_ENABLED=1", // engine on, empty platform layer
	)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); _, _ = cmd.Process.Wait() }()

	base := "http://" + ctlAddr
	waitURL(t, base+"/healthz", 15*time.Second)

	// (1) No policies ⇒ acme can call sbx__run.
	acme := connectWhenFederated(t, base, acmeAdmin.Plaintext, "sbx__run")
	if res, err := acme.CallTool(ctx, &sdk.CallToolParams{Name: "sbx__run", Arguments: map[string]any{}}); err != nil || res.IsError {
		t.Fatalf("baseline call must pass: err=%v res=%+v", err, res)
	}

	// (2) acme admin adds a forbid on the sbx server, via the API — no restart.
	adminPost(t, ctlAddr, acmeAdmin.Plaintext, "/admin/policies",
		map[string]string{
			"name":       "no-sbx",
			"cedar_text": `forbid (principal, action, resource) when { resource.server == "sbx" };`,
		}, http.StatusCreated)

	// Next call by acme is denied with the tenant policy id. Poll briefly: the
	// engine's per-tenant cache invalidates on the store generation, which the
	// next Evaluate observes; a fresh session avoids any client-side caching.
	denied := false
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		sess := connectGatewayAs(t, base, acmeAdmin.Plaintext)
		res, err := sess.CallTool(ctx, &sdk.CallToolParams{Name: "sbx__run", Arguments: map[string]any{}})
		if err == nil && res.IsError && len(res.Content) > 0 {
			if tc, ok := res.Content[0].(*sdk.TextContent); ok && tc.Text == "forbidden by policy: tenant/no-sbx" {
				denied = true
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !denied {
		t.Fatal("acme call must be denied by tenant/no-sbx after the forbid is added")
	}

	// (3) globex is unaffected (tenant isolation).
	globex := connectWhenFederated(t, base, globexOp.Plaintext, "sbx__run")
	if res, err := globex.CallTool(ctx, &sdk.CallToolParams{Name: "sbx__run", Arguments: map[string]any{}}); err != nil || res.IsError {
		t.Fatalf("globex must be unaffected by acme's policy: err=%v res=%+v", err, res)
	}

	// (4) List is tenant-scoped.
	acmeList := getBody(t, base+"/admin/policies", map[string]string{"Authorization": "Bearer " + acmeAdmin.Plaintext}, 200)
	if !strings.Contains(acmeList, "no-sbx") {
		t.Fatalf("acme list must contain no-sbx: %s", acmeList)
	}

	// (5) Delete ⇒ acme calls flow again.
	authReq(t, "DELETE", base+"/admin/policies/no-sbx", acmeAdmin.Plaintext, nil).Body.Close()
	restored := false
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		sess := connectGatewayAs(t, base, acmeAdmin.Plaintext)
		res, err := sess.CallTool(ctx, &sdk.CallToolParams{Name: "sbx__run", Arguments: map[string]any{}})
		if err == nil && !res.IsError {
			restored = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !restored {
		t.Fatal("after delete, acme call must be permitted again")
	}

	// (6) Decision metric present.
	metrics := getBody(t, base+"/metrics", nil, 200)
	if !strings.Contains(metrics, `runtime_gateway_policy_decisions_total{decision="deny",tenant="acme"}`) {
		t.Fatalf("/metrics missing the deny decision series:\n%s", metrics)
	}
}

// TestPolicyPlatformLayer: a platform .cedar file forbids destructive args for
// ALL tenants, is argument-precise, and cannot be removed via the tenant API.
func TestPolicyPlatformLayer(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatalf("ping postgres (%s): %v", dsn, err)
	}
	policyResetDB(t, db)
	t.Cleanup(func() {
		cdb, err := sql.Open("pgx", dsn)
		if err != nil {
			return
		}
		defer cdb.Close()
		for _, q := range []string{
			`DROP TABLE IF EXISTS gateway_policies CASCADE`,
			`DROP TABLE IF EXISTS service_keys CASCADE`,
			`DROP TABLE IF EXISTS identity_users CASCADE`,
			`DROP TABLE IF EXISTS tenants CASCADE`,
		} {
			_, _ = cdb.Exec(q)
		}
	})

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

	up := fakePolicyUpstream(t)

	tmp := t.TempDir()
	agentd := buildBin(t, tmp, "agentd")
	runtimed := buildBin(t, tmp, "runtimed")

	// Platform file: forbid any call whose code arg contains "rm -rf".
	platform := filepath.Join(tmp, "platform.cedar")
	if err := os.WriteFile(platform, []byte(
		`forbid (principal, action == Gateway::Action::"call_tool", resource)`+"\n"+
			`when { context.input has code && context.input.code like "*rm -rf*" };`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(tmp, "runtime.yaml")
	cfg := "agents:\n" +
		"  - {id: a1, name: A1, model: test/scripted, listen_addr: 127.0.0.1:8193, tenant: acme}\n" +
		"gateway:\n" +
		"  servers:\n" +
		"    - {name: sbx, url: " + up + "}\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	ctlAddr := "127.0.0.1:8192"
	cmd := exec.Command(runtimed)
	cmd.Env = append(os.Environ(),
		"RUNTIME_PG_DSN="+dsn,
		"RUNTIME_CTL_ADDR="+ctlAddr,
		"RUNTIME_AGENTD_BIN="+agentd,
		"RUNTIME_CONFIG="+cfgPath,
		"RUNTIME_POLICY_FILE="+platform,
	)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); _, _ = cmd.Process.Wait() }()

	base := "http://" + ctlAddr
	waitURL(t, base+"/healthz", 15*time.Second)
	acme := connectWhenFederated(t, base, acmeAdmin.Plaintext, "sbx__run")

	// Matching argument ⇒ denied by platform/0.
	res, err := acme.CallTool(ctx, &sdk.CallToolParams{Name: "sbx__run", Arguments: map[string]any{"code": "rm -rf /"}})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !res.IsError {
		t.Fatal("rm -rf arg must be denied by the platform policy")
	}
	if tc, ok := res.Content[0].(*sdk.TextContent); !ok || tc.Text != "forbidden by policy: platform/0" {
		t.Fatalf("deny id wrong: %+v", res.Content[0])
	}

	// Non-matching argument ⇒ allowed (argument precision).
	res, err = acme.CallTool(ctx, &sdk.CallToolParams{Name: "sbx__run", Arguments: map[string]any{"code": "echo hi"}})
	if err != nil || res.IsError {
		t.Fatalf("benign arg must pass: err=%v res=%+v", err, res)
	}

	// A tenant admin cannot remove the platform policy: DELETE is no-oracle 204
	// but the forbid still applies (the platform layer is file-only).
	authReq(t, "DELETE", base+"/admin/policies/platform%2F0", acmeAdmin.Plaintext, nil).Body.Close()
	res, err = acme.CallTool(ctx, &sdk.CallToolParams{Name: "sbx__run", Arguments: map[string]any{"code": "rm -rf /"}})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !res.IsError {
		t.Fatal("platform forbid must survive a tenant-admin delete attempt")
	}
}

// TestPolicyBootFailsOnBadPlatformFile: an unparseable platform file is a boot
// failure (fail-closed guardrails).
func TestPolicyBootFailsOnBadPlatformFile(t *testing.T) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	policyResetDB(t, db)

	tmp := t.TempDir()
	runtimed := buildBin(t, tmp, "runtimed")
	bad := filepath.Join(tmp, "bad.cedar")
	if err := os.WriteFile(bad, []byte("this is not cedar"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(tmp, "runtime.yaml")
	// A gateway section (so the policy block runs) + one agent.
	up := fakePolicyUpstream(t)
	cfg := "agents:\n" +
		"  - {id: a1, name: A1, model: test/scripted, listen_addr: 127.0.0.1:8195, tenant: acme}\n" +
		"gateway:\n  servers:\n    - {name: sbx, url: " + up + "}\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(runtimed)
	cmd.Env = append(os.Environ(),
		"RUNTIME_PG_DSN="+dsn,
		"RUNTIME_CTL_ADDR=127.0.0.1:8194",
		"RUNTIME_CONFIG="+cfgPath,
		"RUNTIME_POLICY_FILE="+bad,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := cmd.CombinedOutput()
	if err == nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		t.Fatalf("runtimed must exit non-zero on an unparseable platform file; output:\n%s", out)
	}
	if !strings.Contains(string(out), "policy") {
		t.Fatalf("boot failure should mention the policy file; output:\n%s", out)
	}
}
