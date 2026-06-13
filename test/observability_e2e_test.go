//go:build integration

package test

import (
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
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"

	"github.com/sausheong/runtime/internal/identity"
)

// TestObservabilityE2E is the Observability M1 through-serve proof. Phase A
// (open mode) boots runtimed with TWO scripted agents and asserts: the
// X-Request-ID edge contract (generated req- ids, valid inbound ids echoed
// verbatim), the /metrics fan-out merges per-agent series under the
// authoritative agent label, the HTTP edge counter uses the matched mux
// pattern (never the raw path — cardinality guard), and the merged exposition
// parses cleanly (a malformed merge would be rejected by Prometheus
// wholesale). Phase B reboots with identity ENFORCED and asserts /metrics
// stays auth-free while rejected API requests land in the
// route="auth_rejected" bucket.
func TestObservabilityE2E(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatalf("ping postgres (is it running at %s?): %v", dsn, err)
	}

	// Durable-state cleanup: blank control-plane tables + DBOS schema, and make
	// sure no leftover identity rows flip phase A into enforced mode.
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

	// startRT boots one runtimed with the given config + control addr and
	// returns a kill func that reaps the whole process group (runtimed + agentd
	// children) and waits, so a successor can safely reuse ports.
	startRT := func(cfgName, cfg, ctlAddr string) func() {
		t.Helper()
		cfgPath := filepath.Join(tmp, cfgName)
		if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(runtimed)
		cmd.Env = append(os.Environ(),
			"RUNTIME_PG_DSN="+dsn,
			"RUNTIME_CTL_ADDR="+ctlAddr,
			"RUNTIME_AGENTD_BIN="+agentd,
			"RUNTIME_CONFIG="+cfgPath,
		)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		// Fresh process group so teardown reaps the WHOLE tree; a surviving
		// agentd grandchild keeps our stdout pipe open and blocks `go test`.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		var once bool
		kill := func() {
			if once {
				return
			}
			once = true
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
		t.Cleanup(kill)
		return kill
	}

	// ---- Phase A: open mode (no identity), two scripted agents. ----

	ctlA := "127.0.0.1:8160"
	baseA := "http://" + ctlA
	cfgA := "agents:\n" +
		"  - {id: support, name: Support, model: test/scripted, listen_addr: 127.0.0.1:8161}\n" +
		"  - {id: research, name: Research, model: test/scripted, listen_addr: 127.0.0.1:8162}\n"
	killA := startRT("runtime-a.yaml", cfgA, ctlA)

	waitURL(t, baseA+"/healthz", 15*time.Second)
	// Agents boot sequentially (DBOS schema init is serialized), so give the
	// per-agent health checks a generous window.
	waitURL(t, baseA+"/agents/support/healthz", 30*time.Second)
	waitURL(t, baseA+"/agents/research/healthz", 30*time.Second)

	// (a) X-Request-ID: a POST without the header gets a generated req-<hex>
	// id echoed on the response.
	supportSess, hdr := postSessionOn(t, baseA, "support", nil)
	if !strings.HasPrefix(hdr, "req-") {
		t.Fatalf("X-Request-ID = %q, want generated req- prefix", hdr)
	}
	// A valid caller-supplied id is echoed back verbatim.
	_, hdr2 := postSessionOn(t, baseA, "support", map[string]string{"X-Request-ID": "req-e2etest123"})
	if hdr2 != "req-e2etest123" {
		t.Fatalf("X-Request-ID = %q, want caller id echoed verbatim", hdr2)
	}

	// (b) Drive one session per agent to completion (the first support session
	// from (a) plus one on research), so both agents emit turn metrics.
	researchSess, _ := postSessionOn(t, baseA, "research", nil)
	waitSessionCompleted(t, baseA, "support", supportSess, 60*time.Second)
	waitSessionCompleted(t, baseA, "research", researchSess, 60*time.Second)

	// (c) Merged /metrics: per-agent turn series fan out under the registered
	// agent ids, the up gauge reflects the clean scrape, the HTTP edge counter
	// carries the matched mux pattern (the /agents/{id}/ subtree route), and
	// the raw path never leaks into a label value.
	body := getBody(t, baseA+"/metrics", nil, 200)
	for _, want := range []string{
		`agent_turns_total{agent="support"`,
		`agent_turns_total{agent="research"`,
		`runtime_agent_up{agent="support",replica="0"} 1`,
		`runtime_agent_up{agent="research",replica="0"} 1`,
		`runtime_http_requests_total{method="POST",route="/agents/{id}/`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("merged /metrics missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `route="/agents/support/`) {
		t.Fatalf("raw path leaked into route label:\n%s", body)
	}
	// Merge-validity proof: the merged exposition must parse cleanly — a
	// half-valid body (duplicate families, bad escaping) is rejected by
	// Prometheus wholesale.
	parser := expfmt.NewTextParser(model.UTF8Validation)
	if _, err := parser.TextToMetricFamilies(strings.NewReader(body)); err != nil {
		t.Fatalf("merged exposition does not parse: %v\n%s", err, body)
	}

	// ---- Phase B: identity ENFORCED. ----

	killA() // reap the whole phase-A tree first so phase B can reuse agent ports

	// Tenant + service-key rows flip runtimed into enforced mode at boot.
	st, err := identity.NewStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTenant(ctx, "alpha", "Alpha"); err != nil {
		t.Fatal(err)
	}
	alphaKey, err := identity.MintServiceKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.InsertServiceKey(ctx, alphaKey.ID, "alpha", alphaKey.Hash, identity.RoleOperator, "alpha-op"); err != nil {
		t.Fatal(err)
	}

	ctlB := "127.0.0.1:8163"
	baseB := "http://" + ctlB
	cfgB := "agents:\n" +
		"  - {id: support, name: Support, model: test/scripted, listen_addr: 127.0.0.1:8161, tenant: alpha}\n"
	startRT("runtime-b.yaml", cfgB, ctlB)

	// /healthz is exempt from identity, so the plain waiter works even in
	// enforced mode.
	waitURL(t, baseB+"/healthz", 15*time.Second)

	// (d) /metrics is auth-free (mounted OUTSIDE the identity chain), while the
	// API proper 401s without a credential.
	_ = getBody(t, baseB+"/metrics", nil, 200)
	_ = getBody(t, baseB+"/agents", nil, 401)

	// (e) The rejected request lands in the auth_rejected bucket: the identity
	// middleware's onReject hook calls AuthRejected(401), which increments the
	// counter under route="auth_rejected" with an empty method (no
	// per-route/per-method detail for rejected traffic) and records NO
	// duration sample. Labels render alphabetically in the exposition.
	bodyB := getBody(t, baseB+"/metrics", nil, 200)
	if !strings.Contains(bodyB, `runtime_http_requests_total{method="",route="auth_rejected",status="401"}`) {
		t.Fatalf("auth_rejected counter missing from /metrics:\n%s", bodyB)
	}
}

// postSessionOn POSTs {"message":"hello"} to /agents/{agent}/sessions with
// extra headers, returning the created session id and the response
// X-Request-ID. (Named to avoid the single-agent postSession in resume_test.)
func postSessionOn(t *testing.T, base, agent string, headers map[string]string) (string, string) {
	t.Helper()
	req, err := http.NewRequest("POST", base+"/agents/"+agent+"/sessions",
		strings.NewReader(`{"message":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST sessions %s: %v", agent, err)
	}
	defer resp.Body.Close()
	var out struct {
		SessionID string `json:"session_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.SessionID == "" {
		t.Fatalf("POST sessions %s: no session id (status %d)", agent, resp.StatusCode)
	}
	return out.SessionID, resp.Header.Get("X-Request-ID")
}

// waitSessionCompleted polls GET /agents/{agent}/sessions until the given
// session reports status "completed" or the deadline passes.
func waitSessionCompleted(t *testing.T, base, agent, sessionID string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	var last []sessRow
	for time.Now().Before(deadline) {
		last = agentSessions(t, base, agent)
		for _, r := range last {
			if r.ID == sessionID && r.Status == "completed" {
				return
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("session %s on %s never completed within %s; last rows: %+v", sessionID, agent, d, last)
}

// getBody GETs url with optional headers, asserts the status, and returns the
// body as a string.
func getBody(t *testing.T, url string, headers map[string]string, wantStatus int) string {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	if resp.StatusCode != wantStatus {
		t.Fatalf("GET %s: status %d, want %d\n%s", url, resp.StatusCode, wantStatus, raw)
	}
	return string(raw)
}
