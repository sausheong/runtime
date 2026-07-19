//go:build integration

package test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
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

// quotaResetDB mirrors policyResetDB (drops the shared identity/gateway tables so
// the test starts clean and leaves the DB as it found it) plus gateway_quotas.
func quotaResetDB(t *testing.T, db *sql.DB) {
	t.Helper()
	mustExec(t, db, `DROP TABLE IF EXISTS markers`)
	mustExec(t, db, `CREATE TABLE markers (id BIGSERIAL PRIMARY KEY, ran_at TIMESTAMPTZ)`)
	mustExec(t, db, `DROP TABLE IF EXISTS session_events, sessions, agents CASCADE`)
	mustExec(t, db, `DROP SCHEMA IF EXISTS dbos CASCADE`)
	for _, q := range quotaDropQueries {
		mustExec(t, db, q)
	}
}

var quotaDropQueries = []string{
	`DROP TABLE IF EXISTS gateway_quotas CASCADE`,
	`DROP TABLE IF EXISTS gateway_policies CASCADE`,
	`DROP TABLE IF EXISTS service_keys CASCADE`,
	`DROP TABLE IF EXISTS identity_users CASCADE`,
	`DROP TABLE IF EXISTS tenants CASCADE`,
}

// TestQuotaLifecycle: no quota ⇒ acme flows; acme admin adds a rate quota via
// the API ⇒ acme's calls beyond the burst are rejected WITHOUT restart
// (live-reload); globex unaffected; list is tenant-scoped; a tenant-admin's
// "*" write is rejected (RBAC); delete ⇒ acme flows again; the rejection metric
// is present.
func TestQuotaLifecycle(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatalf("ping postgres (%s): %v", dsn, err)
	}
	quotaResetDB(t, db)
	t.Cleanup(func() {
		cdb, err := sql.Open("pgx", dsn)
		if err != nil {
			return
		}
		defer cdb.Close()
		for _, q := range quotaDropQueries {
			_, _ = cdb.Exec(q)
		}
	})

	// Identity: two tenants; acme has an ADMIN key (to write quotas), globex an
	// admin key too (so it can be verified independent of quota state). The
	// upstream is visible to both tenants.
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
		"  - {id: a1, name: A1, model: test/scripted, listen_addr: 127.0.0.1:8491, tenant: acme}\n" +
		"gateway:\n" +
		"  servers:\n" +
		"    - {name: sbx, url: " + up + "}\n" // no tenants: ⇒ visible to all
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	ctlAddr := "127.0.0.1:8490"
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
	defer func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); _, _ = cmd.Process.Wait() }()

	base := "http://" + ctlAddr
	waitURL(t, base+"/healthz", 15*time.Second)

	// (1) No quota ⇒ acme can call sbx__run.
	acme := connectWhenFederated(t, base, acmeAdmin.Plaintext, "sbx__run")
	if res, err := acme.CallTool(ctx, &sdk.CallToolParams{Name: "sbx__run", Arguments: map[string]any{}}); err != nil || res.IsError {
		t.Fatalf("baseline call must pass: err=%v res=%+v", err, res)
	}

	// (2) acme admin adds a rate quota on the sbx server via the API — no restart.
	adminPost(t, ctlAddr, acmeAdmin.Plaintext, "/admin/quotas",
		map[string]any{"tenant": "acme", "upstream": "sbx", "rate_per_min": 2}, http.StatusCreated)

	// (3) Poll until acme is rejected with the quota tool error, WITHOUT restart.
	// The limiter refreshes its rule set at most once per 2s (Task 2), so the new
	// quota can take up to ~2s to bind — the deadline exceeds that. Within one
	// poll iteration we burst calls on a fresh session (burst == rate == 2, so
	// the 3rd fresh-window call should trip) and look for the quota reject.
	denied := false
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && !denied {
		sess := connectGatewayAs(t, base, acmeAdmin.Plaintext)
		for i := 0; i < 6; i++ {
			res, err := sess.CallTool(ctx, &sdk.CallToolParams{Name: "sbx__run", Arguments: map[string]any{}})
			if err == nil && res.IsError && len(res.Content) > 0 {
				if tc, ok := res.Content[0].(*sdk.TextContent); ok && strings.HasPrefix(tc.Text, "quota exceeded: acme/sbx") {
					denied = true
					break
				}
			}
		}
		_ = sess.Close()
		if !denied {
			time.Sleep(200 * time.Millisecond)
		}
	}
	if !denied {
		t.Fatal("acme calls must be rejected by the acme/sbx quota after it is added")
	}

	// (4) globex is unaffected (tenant isolation).
	globex := connectWhenFederated(t, base, globexOp.Plaintext, "sbx__run")
	if res, err := globex.CallTool(ctx, &sdk.CallToolParams{Name: "sbx__run", Arguments: map[string]any{}}); err != nil || res.IsError {
		t.Fatalf("globex must be unaffected by acme's quota: err=%v res=%+v", err, res)
	}

	// (5) List is tenant-scoped.
	acmeList := getBody(t, base+"/admin/quotas", map[string]string{"Authorization": "Bearer " + acmeAdmin.Plaintext}, 200)
	if !strings.Contains(acmeList, `"Upstream":"sbx"`) || !strings.Contains(acmeList, `"Tenant":"acme"`) {
		t.Fatalf("acme quota list must contain the acme/sbx rule: %s", acmeList)
	}

	// (6) A tenant admin cannot set a "*" (superuser-only) quota ⇒ 400 with the
	// RBAC message. Use a FRESH upstream (no existing rule) so the rejection is
	// unambiguously the RBAC guard, not a PRIMARY KEY dup on (acme, sbx). The
	// handler must pass body.Tenant through to RegisterQuotaShared for this to
	// hold — if the guard is removed the "*" write would rewrite/insert and this
	// assertion fails.
	rbacBody, _ := json.Marshal(map[string]any{"tenant": "*", "upstream": "other-svc", "rate_per_min": 1})
	rbacResp := authReq(t, "POST", base+"/admin/quotas", acmeAdmin.Plaintext, bytes.NewReader(rbacBody))
	rbacRB, _ := io.ReadAll(rbacResp.Body)
	rbacResp.Body.Close()
	if rbacResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("tenant-admin '*' quota write must be rejected: status=%d body=%s", rbacResp.StatusCode, rbacRB)
	}
	if !strings.Contains(string(rbacRB), "superuser") {
		t.Fatalf("tenant-admin '*' rejection must carry the RBAC message (mentioning superuser): %s", rbacRB)
	}

	// (7) Delete the rule ⇒ acme flows again (poll; refresh throttle applies).
	authReq(t, "DELETE", base+"/admin/quotas?upstream=sbx", acmeAdmin.Plaintext, nil).Body.Close()
	restored := false
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		sess := connectGatewayAs(t, base, acmeAdmin.Plaintext)
		res, err := sess.CallTool(ctx, &sdk.CallToolParams{Name: "sbx__run", Arguments: map[string]any{}})
		_ = sess.Close()
		if err == nil && !res.IsError {
			restored = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !restored {
		t.Fatal("after delete, acme calls must be permitted again")
	}

	// (8) Rejection metric present with acme + sbx labels.
	metrics := getBody(t, base+"/metrics", nil, 200)
	if !strings.Contains(metrics, `runtime_gateway_quota_rejections_total{server="sbx",tenant="acme"}`) {
		t.Fatalf("/metrics missing the quota rejection series:\n%s", metrics)
	}
}
