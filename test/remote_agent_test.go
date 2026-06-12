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
	"strings"
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

	// 1. Start a standalone "remote" agentd with an auth token — stands in for an
	//    operator-provisioned agent on another host.
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

	rmtWaitHealthy(t, "http://"+remoteAddr+"/healthz", token, 30*time.Second)

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

	rmtWaitHealthy(t, "http://"+ctlAddr+"/healthz", "", 30*time.Second)

	// 3. Both agents healthy via /agents.
	rmtWaitFor(t, 20*time.Second, func() bool {
		st := rmtGetAgents(t, ctlAddr)
		return st["local"] && st["remote"]
	}, "both agents healthy")

	// 4. Proxy a session round-trip THROUGH runtimed to the remote agent.
	resp, err := http.Post("http://"+ctlAddr+"/agents/remote/sessions", "application/json",
		strings.NewReader(`{"message":"hi"}`))
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
	//    restart (nothing should be listening on remoteAddr afterward).
	killRemote()
	rmtWaitFor(t, 20*time.Second, func() bool {
		st := rmtGetAgents(t, ctlAddr)
		return st["local"] && !st["remote"]
	}, "remote unreachable, local still healthy")

	// Wait > Supervisor.Backoff (1s) so a (wrongly) restarted agent would have
	// had time to relisten; a bounded client so a half-open port fails fast
	// rather than hanging to the test timeout.
	time.Sleep(2 * time.Second)
	noRestartClient := &http.Client{Timeout: 2 * time.Second}
	if _, err := noRestartClient.Get("http://" + remoteAddr + "/healthz"); err == nil {
		t.Fatal("something is still serving on the remote addr — runtimed must NOT restart a remote agent")
	}

	// 6. The control plane is still alive and the local agent still serves.
	resp, err = http.Post("http://"+ctlAddr+"/agents/local/sessions", "application/json",
		strings.NewReader(`{"message":"still here"}`))
	if err != nil {
		t.Fatalf("local session after remote death: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("local agent broken after remote death: status=%d", resp.StatusCode)
	}
}

func rmtGetAgents(t *testing.T, ctlAddr string) map[string]bool {
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

func rmtWaitHealthy(t *testing.T, url, token string, d time.Duration) {
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

func rmtWaitFor(t *testing.T, d time.Duration, cond func() bool, what string) {
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
