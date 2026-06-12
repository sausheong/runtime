//go:build integration

package test

import (
	"context"
	"database/sql"
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

// TestGatewayBrowserE2E boots the whole stack with identity ENFORCED and a
// forward_tenant browserd upstream (fake backend, no Chrome): two tenants
// federate the browser tools, tenant scoping holds, and a spoofed __rt_tenant
// is overridden.
func TestGatewayBrowserE2E(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatalf("ping postgres (is it running at %s?): %v", dsn, err)
	}
	mustExec(t, db, `DROP TABLE IF EXISTS session_events, sessions, agents CASCADE`)
	mustExec(t, db, `DROP SCHEMA IF EXISTS dbos CASCADE`)
	for _, q := range []string{
		`DROP TABLE IF EXISTS service_keys CASCADE`,
		`DROP TABLE IF EXISTS identity_users CASCADE`,
		`DROP TABLE IF EXISTS tenants CASCADE`,
	} {
		mustExec(t, db, q)
	}
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

	tmp := t.TempDir()
	agentd := filepath.Join(tmp, "agentd")
	if out, err := exec.Command("go", "build", "-o", agentd, "../cmd/agentd").CombinedOutput(); err != nil {
		t.Fatalf("build agentd: %v\n%s", err, out)
	}
	runtimed := filepath.Join(tmp, "runtimed")
	if out, err := exec.Command("go", "build", "-o", runtimed, "../cmd/runtimed").CombinedOutput(); err != nil {
		t.Fatalf("build runtimed: %v\n%s", err, out)
	}
	browserd := filepath.Join(tmp, "browserd")
	if out, err := exec.Command("go", "build", "-o", browserd, "../cmd/browserd").CombinedOutput(); err != nil {
		t.Fatalf("build browserd: %v\n%s", err, out)
	}

	cfgPath := filepath.Join(tmp, "runtime.yaml")
	cfg := "agents:\n" +
		"  - {id: a1, name: A1, model: test/scripted, listen_addr: 127.0.0.1:8181, tenant: alpha}\n" +
		"gateway:\n" +
		"  servers:\n" +
		"    - name: browser\n" +
		"      command: " + browserd + "\n" +
		"      forward_tenant: true\n" +
		"      env: {RUNTIME_BROWSER_FAKE: \"1\"}\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	ctlAddr := "127.0.0.1:8180"
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
	waitURL(t, base+"/healthz", 15*time.Second)

	alphaSess := connectWhenFederated(t, base, alphaKey.Plaintext,
		"browser__create_browser", "browser__navigate")
	betaSess := connectWhenFederated(t, base, betaKey.Plaintext,
		"browser__list_browsers")

	// alpha creates a browser WITH a spoofed __rt_tenant — gateway overrides it.
	var created struct {
		BrowserID string `json:"browser_id"`
	}
	callJSON(t, alphaSess, "browser__create_browser", map[string]any{"__rt_tenant": "beta"}, &created)
	if !strings.HasPrefix(created.BrowserID, "brw-") {
		t.Fatalf("browser_id %q lacks brw- prefix", created.BrowserID)
	}

	// beta lists: ZERO (spoof did not land under beta; list is tenant-scoped).
	var listed struct {
		Browsers []map[string]any `json:"browsers"`
	}
	callJSON(t, betaSess, "browser__list_browsers", nil, &listed)
	if len(listed.Browsers) != 0 {
		t.Fatalf("beta sees %d browsers, want 0 (tenant leak)", len(listed.Browsers))
	}

	// beta cannot use alpha's browser id — existence hidden (IsError). The fake
	// backend has no CDP endpoint, but cross-tenant Lookup fails with
	// errNoSandbox BEFORE any Chrome interaction, so this asserts tenancy, not
	// Chrome behavior.
	res, err := betaSess.CallTool(ctx, &sdk.CallToolParams{
		Name:      "browser__navigate",
		Arguments: map[string]any{"browser_id": created.BrowserID, "url": "https://example.com"},
	})
	if err != nil {
		t.Fatalf("beta cross-tenant navigate (protocol): %v", err)
	}
	if !res.IsError {
		t.Fatalf("beta navigated alpha's browser — cross-tenant access")
	}
}
