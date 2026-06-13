//go:build integration

package test

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"database/sql"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestRegistrationHandshake proves C3 M2 end-to-end: a remote agentd started with
// ONLY the registration URL+token (plus the inbound M1 bearer and the operator's
// static listen addr) — NO RUNTIME_PG_DSN, NO RUNTIME_AGENT_ID, NO brokered
// secret in its env — POSTs /register, receives its full env delta (DSN, id,
// tenant, replica, and the brokered tenant secret OPENAI_API_KEY), os.Setenv's
// it, boots, and serves a conformant session through runtimed.
//
// Plus two negatives:
//   - a revoked token makes a second agentd exit non-zero (the log.Fatal path on 401);
//   - a non-superuser admin scoped to tenant "other" minting a register token for
//     agent "research" (tenant default) gets 403.
//
// RUNTIME_LISTEN_ADDR is operator/pod-provided infra config, NOT supplied by the
// handshake: a REMOTE AgentProcess sets BaseURL but leaves Addr == "" (see
// controlplane/registry.go remote branch), so envDelta returns
// RUNTIME_LISTEN_ADDR="" (proxy.go:71). agentd's fetchRegistration skips empty
// delta values so the static infra-provided bind addr survives — matching the
// chart, whose agent-statefulset.yaml sets RUNTIME_LISTEN_ADDR=":8080" statically
// while only DSN/secrets/id come from the handshake.
func TestRegistrationHandshake(t *testing.T) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatalf("ping postgres (is it running at %s?): %v", dsn, err)
	}
	// Self-clean: session/dbos state plus the identity tables runtimed's identity
	// store creates on boot (so reruns start from a clean slate).
	mustExec(t, db, `DROP TABLE IF EXISTS session_events, sessions, agents CASCADE`)
	mustExec(t, db, `DROP SCHEMA IF EXISTS dbos CASCADE`)
	mustExec(t, db, `DROP TABLE IF EXISTS registration_tokens, secrets, service_keys, identity_users, tenants CASCADE`)
	// This is the ONLY integration test that enables identity (it needs the admin
	// API for secrets + register tokens). runtimed turns identity ON whenever the
	// tenants/service_keys tables are non-empty (idStore.AnyConfigured). Sibling
	// remote tests run OPEN and break if those rows linger, so drop the identity
	// tables again on the way out — top-of-test cleanup covers reruns; this defer
	// protects siblings in the same `go test` invocation.
	defer mustExec(t, db, `DROP TABLE IF EXISTS registration_tokens, secrets, service_keys, identity_users, tenants CASCADE`)

	tmp := t.TempDir()
	agentd := filepath.Join(tmp, "agentd")
	if out, err := exec.Command("go", "build", "-o", agentd, "../cmd/agentd").CombinedOutput(); err != nil {
		t.Fatalf("build agentd: %v\n%s", err, out)
	}
	runtimed := filepath.Join(tmp, "runtimed")
	if out, err := exec.Command("go", "build", "-o", runtimed, "../cmd/runtimed").CombinedOutput(); err != nil {
		t.Fatalf("build runtimed: %v\n%s", err, out)
	}

	// Secrets keyring: a single 32-byte key, base64-encoded, named k1.
	rawKey := make([]byte, 32)
	if _, err := rand.Read(rawKey); err != nil {
		t.Fatal(err)
	}
	keys := "k1:" + base64.StdEncoding.EncodeToString(rawKey)
	const (
		bootstrap   = "boot-super-key"
		agentBearer = "research-inbound-bearer"
		agentAddr   = "127.0.0.1:8330"
		ctlAddr     = "127.0.0.1:8331"
	)

	// runtime.yaml: ONE remote agent, single (no {i}, no replicas), tenant omitted
	// ⇒ "default" (also the secret's tenant). auth_token inlined as a literal.
	cfgPath := filepath.Join(tmp, "runtime.yaml")
	cfg := "agents:\n" +
		fmt.Sprintf("  - {id: research, name: Research, model: test/scripted, url: \"http://%s\", auth_token: \"%s\"}\n",
			agentAddr, agentBearer)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	// Start runtimed with identity + secrets enabled. The remote `research` agent
	// is NOT started yet, so it is unreachable initially — runtimed boots anyway
	// (degrade-don't-fail).
	rt := exec.Command(runtimed)
	rt.Env = append(os.Environ(),
		"RUNTIME_PG_DSN="+dsn,
		"RUNTIME_CTL_ADDR="+ctlAddr,
		"RUNTIME_AGENTD_BIN="+agentd,
		"RUNTIME_CONFIG="+cfgPath,
		"RUNTIME_SECRETS_KEYS="+keys,
		"RUNTIME_SECRETS_PRIMARY=k1",
		"RUNTIME_ADMIN_BOOTSTRAP="+bootstrap,
	)
	rt.Stdout, rt.Stderr = os.Stdout, os.Stderr
	rt.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := rt.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Kill(-rt.Process.Pid, syscall.SIGKILL); _, _ = rt.Process.Wait() }()
	rmtWaitHealthy(t, "http://"+ctlAddr+"/healthz", "", 30*time.Second)

	// Admin: the "default" tenant must exist in the identity store before a
	// superuser can target it (effectiveTenant validates existence). The agent's
	// config tenant ("default") is the registry's notion; the identity store's
	// tenants table starts empty, so create it explicitly.
	adminPost(t, ctlAddr, bootstrap, "/admin/tenants",
		map[string]string{"id": "default"}, http.StatusCreated)

	// Admin: set the brokered secret + mint a registration token for research.
	adminPost(t, ctlAddr, bootstrap, "/admin/secrets",
		map[string]string{"name": "OPENAI_API_KEY", "value": "sk-secret", "tenant": "default"}, http.StatusOK)
	var minted struct {
		ID        string `json:"id"`
		Plaintext string `json:"plaintext"`
	}
	adminPostJSON(t, ctlAddr, bootstrap, "/admin/register-tokens",
		map[string]string{"agent": "research"}, http.StatusCreated, &minted)
	if minted.ID == "" || minted.Plaintext == "" {
		t.Fatalf("mint register token: empty id/plaintext: %+v", minted)
	}
	regToken := minted.Plaintext

	// handshakeEnv is agentd's MINIMAL env: PATH/HOME for the process, the static
	// infra-provided listen addr + inbound bearer, and the two handshake vars.
	// Deliberately built explicitly (NOT os.Environ()) and WITHOUT RUNTIME_PG_DSN,
	// RUNTIME_AGENT_ID, or OPENAI_API_KEY — the whole point is that agentd fetches
	// those from the control plane.
	handshakeEnv := func() []string {
		return []string{
			"PATH=" + os.Getenv("PATH"),
			"HOME=" + os.Getenv("HOME"),
			"RUNTIME_LISTEN_ADDR=" + agentAddr,
			"RUNTIME_AGENT_AUTH_TOKEN=" + agentBearer,
			"HOSTNAME=research-0",
			"RUNTIME_REGISTRATION_URL=http://" + ctlAddr + "/register",
			"RUNTIME_REGISTRATION_TOKEN=" + regToken,
		}
	}

	// Start agentd in handshake mode.
	ag := exec.Command(agentd)
	ag.Env = handshakeEnv()
	ag.Stdout, ag.Stderr = os.Stdout, os.Stderr
	ag.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := ag.Start(); err != nil {
		t.Fatalf("start handshake agentd: %v", err)
	}
	agKilled := false
	killAg := func() {
		if agKilled {
			return
		}
		agKilled = true
		_ = syscall.Kill(-ag.Process.Pid, syscall.SIGKILL)
		_, _ = ag.Process.Wait()
	}
	defer killAg()

	// research healthy via runtimed proves agentd booted with the FETCHED DSN +
	// id (it had neither in its env). Boot-from-empty-env is also the proof that
	// the FULL delta applied: agentd only reaches Serve if every Setenv (including
	// OPENAI_API_KEY) succeeded and RUNTIME_PG_DSN/RUNTIME_AGENT_ID were present —
	// none of which were in its starting env.
	// NOTE: identity is enabled here, so GET /agents and the session routes are
	// auth-gated (only /healthz is exempt). The shared rmtGetAgents helper sends
	// no bearer, so we probe with the bootstrap (superuser) bearer instead.
	rmtWaitFor(t, 30*time.Second, func() bool {
		return agentHealthyWithBearer(t, ctlAddr, bootstrap, "research")
	}, "research healthy after handshake boot")

	// Drive a conformant session through runtimed: create + fetch (with bearer).
	resp := authReq(t, "POST", "http://"+ctlAddr+"/agents/research/sessions", bootstrap,
		strings.NewReader(`{"message":"hi"}`))
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("session create: status=%d body=%s", resp.StatusCode, body)
	}
	var sess struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(body, &sess); err != nil || sess.SessionID == "" {
		t.Fatalf("no session_id: err=%v body=%s", err, body)
	}
	getResp := authReq(t, "GET", fmt.Sprintf("http://%s/agents/research/sessions/%s", ctlAddr, sess.SessionID), bootstrap, nil)
	getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("get session: status=%d", getResp.StatusCode)
	}

	// Negative 1 — revoked token. Kill the handshake agentd, revoke the token,
	// then start a SECOND agentd with the SAME minimal env (same regToken). It must
	// exit NON-ZERO within ~10s: fetchRegistration's log.Fatal on the 401.
	killAg()
	adminDelete(t, ctlAddr, bootstrap, "/admin/register-tokens/"+minted.ID, http.StatusNoContent)

	ag2 := exec.Command(agentd)
	ag2.Env = handshakeEnv()
	ag2.Stdout, ag2.Stderr = os.Stdout, os.Stderr
	ag2.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := ag2.Start(); err != nil {
		t.Fatalf("start second agentd: %v", err)
	}
	defer func() { _ = syscall.Kill(-ag2.Process.Pid, syscall.SIGKILL); _, _ = ag2.Process.Wait() }()
	waitErr := make(chan error, 1)
	go func() { waitErr <- ag2.Wait() }()
	select {
	case err := <-waitErr:
		if err == nil {
			t.Fatal("second agentd exited 0 — a revoked registration token must abort startup")
		}
		if _, ok := err.(*exec.ExitError); !ok {
			t.Fatalf("second agentd: expected exec.ExitError, got %T: %v", err, err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("second agentd did not exit within 15s after token revocation")
	}

	// Negative 2 — cross-tenant mint 403. Create tenant "other" + a NON-superuser
	// admin service key scoped to it; that admin minting a token for agent
	// "research" (tenant default) must get 403.
	adminPost(t, ctlAddr, bootstrap, "/admin/tenants",
		map[string]string{"id": "other"}, http.StatusCreated)
	var otherKey struct {
		ID        string `json:"id"`
		Plaintext string `json:"plaintext"`
	}
	adminPostJSON(t, ctlAddr, bootstrap, "/admin/keys",
		map[string]string{"role": "admin", "tenant": "other"}, http.StatusCreated, &otherKey)
	if otherKey.Plaintext == "" {
		t.Fatalf("mint other-tenant admin key: empty plaintext: %+v", otherKey)
	}
	adminPost(t, ctlAddr, otherKey.Plaintext, "/admin/register-tokens",
		map[string]string{"agent": "research"}, http.StatusForbidden)
}

// adminPost POSTs body as JSON with a bearer and asserts the status code.
func adminPost(t *testing.T, ctlAddr, bearer, path string, body any, wantStatus int) {
	t.Helper()
	adminPostJSON(t, ctlAddr, bearer, path, body, wantStatus, nil)
}

// adminPostJSON POSTs body as JSON with a bearer, asserts the status code, and
// (when out != nil) decodes the response body into out.
func adminPostJSON(t *testing.T, ctlAddr, bearer, path string, body any, wantStatus int, out any) {
	t.Helper()
	buf, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", "http://"+ctlAddr+path, bytes.NewReader(buf))
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != wantStatus {
		t.Fatalf("POST %s: status=%d want %d body=%s", path, resp.StatusCode, wantStatus, rb)
	}
	if out != nil {
		if err := json.Unmarshal(rb, out); err != nil {
			t.Fatalf("POST %s: decode response: %v body=%s", path, err, rb)
		}
	}
}

// authReq issues a request with a bearer and returns the response (body open).
func authReq(t *testing.T, method, url, bearer string, body io.Reader) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, url, body)
	req.Header.Set("Authorization", "Bearer "+bearer)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

// agentHealthyWithBearer reports whether GET /agents (auth-gated under identity)
// lists agentID as healthy, using a superuser bearer. Returns false on any
// transport/decoding hiccup so it composes with rmtWaitFor.
func agentHealthyWithBearer(t *testing.T, ctlAddr, bearer, agentID string) bool {
	t.Helper()
	req, _ := http.NewRequest("GET", "http://"+ctlAddr+"/agents", nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var arr []struct {
		ID      string `json:"id"`
		Healthy bool   `json:"healthy"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&arr); err != nil {
		return false
	}
	for _, a := range arr {
		if a.ID == agentID {
			return a.Healthy
		}
	}
	return false
}

// adminDelete sends a DELETE with a bearer and asserts the status code.
func adminDelete(t *testing.T, ctlAddr, bearer, path string, wantStatus int) {
	t.Helper()
	req, _ := http.NewRequest("DELETE", "http://"+ctlAddr+path, nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE %s: %v", path, err)
	}
	rb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != wantStatus {
		t.Fatalf("DELETE %s: status=%d want %d body=%s", path, resp.StatusCode, wantStatus, rb)
	}
}
