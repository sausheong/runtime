//go:build integration

package test

import (
	"context"
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

// TestRemoteReplicaPoolAttach proves C2 M2: runtimed attaches to a remote agent
// declared as a {i}-templated pool (replicas: 2), round-robins new sessions
// across the two ordinals, and on one ordinal dying routes new sessions only to
// the survivor (liveness-aware NextReplica) while a session pinned to the
// survivor keeps working — no restart of the dead ordinal.
func TestRemoteReplicaPoolAttach(t *testing.T) {
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

	tmp := t.TempDir()
	agentd := filepath.Join(tmp, "agentd")
	if out, err := exec.Command("go", "build", "-o", agentd, "../cmd/agentd").CombinedOutput(); err != nil {
		t.Fatalf("build agentd: %v\n%s", err, out)
	}
	runtimed := filepath.Join(tmp, "runtimed")
	if out, err := exec.Command("go", "build", "-o", runtimed, "../cmd/runtimed").CombinedOutput(); err != nil {
		t.Fatalf("build runtimed: %v\n%s", err, out)
	}

	// Two ordinals on adjacent ports, each its own DBOS executor id pool#0/pool#1.
	// First ordinal starts alone so it creates the DBOS schema before the second
	// races on it (the same serialization runtimed does for local pools).
	// Ports must match the {i}-templated url runtimed expands below:
	// http://127.0.0.1:831{i} → ordinal 0 = :8310, ordinal 1 = :8311.
	ords := []struct {
		addr string
		vmid string
	}{
		{"127.0.0.1:8310", "pool#0"},
		{"127.0.0.1:8311", "pool#1"},
	}
	procs := make([]*exec.Cmd, len(ords))
	killed := make([]bool, len(ords))
	startOrd := func(i int) {
		c := exec.Command(agentd)
		c.Env = append(os.Environ(),
			"RUNTIME_PG_DSN="+dsn,
			"RUNTIME_LISTEN_ADDR="+ords[i].addr,
			"RUNTIME_AGENT_ID=pool",
			"RUNTIME_AGENT_TENANT=default",
			"RUNTIME_AGENT_REPLICA="+fmt.Sprint(i),
			"DBOS__VMID="+ords[i].vmid,
		)
		c.Stdout, c.Stderr = os.Stdout, os.Stderr
		c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := c.Start(); err != nil {
			t.Fatalf("start ordinal %d: %v", i, err)
		}
		procs[i] = c
	}
	killOrd := func(i int) {
		if killed[i] {
			return
		}
		killed[i] = true
		_ = syscall.Kill(-procs[i].Process.Pid, syscall.SIGKILL)
		_, _ = procs[i].Process.Wait()
	}
	startOrd(0)
	rmtWaitHealthy(t, "http://"+ords[0].addr+"/healthz", "", 30*time.Second)
	startOrd(1)
	rmtWaitHealthy(t, "http://"+ords[1].addr+"/healthz", "", 30*time.Second)
	defer func() {
		for i := range procs {
			killOrd(i)
		}
	}()

	// runtimed config: one remote POOL, {i}-templated url, replicas: 2.
	cfgPath := filepath.Join(tmp, "runtime.yaml")
	cfg := "agents:\n" +
		"  - {id: pool, name: Pool, model: test/scripted, url: \"http://127.0.0.1:831{i}\", replicas: 2}\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	ctlAddr := "127.0.0.1:8320"
	rt := exec.Command(runtimed)
	rt.Env = append(os.Environ(),
		"RUNTIME_PG_DSN="+dsn,
		"RUNTIME_CTL_ADDR="+ctlAddr,
		"RUNTIME_AGENTD_BIN="+agentd,
		"RUNTIME_CONFIG="+cfgPath,
	)
	rt.Stdout, rt.Stderr = os.Stdout, os.Stderr
	rt.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := rt.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Kill(-rt.Process.Pid, syscall.SIGKILL); _, _ = rt.Process.Wait() }()
	rmtWaitHealthy(t, "http://"+ctlAddr+"/healthz", "", 30*time.Second)

	rmtWaitFor(t, 20*time.Second, func() bool {
		return rmtGetAgents(t, ctlAddr)["pool"]
	}, "pool agent healthy")

	mkSession := func() string {
		resp, err := http.Post("http://"+ctlAddr+"/agents/pool/sessions", "application/json",
			strings.NewReader(`{"message":"hi"}`))
		if err != nil {
			t.Fatalf("create session: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("session create: status=%d body=%s", resp.StatusCode, body)
		}
		var s struct {
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal(body, &s); err != nil || s.SessionID == "" {
			t.Fatalf("no session_id: err=%v body=%s", err, body)
		}
		return s.SessionID
	}

	// mkSessionTry is the tolerant variant used only inside the post-kill
	// convergence loop: a 503 ("agent unavailable") is EXPECTED for up to one
	// health-poll interval after ordinal 1 dies, while runtimed's NextReplica
	// still round-robins onto the not-yet-detected-dead replica. The wait loop
	// retries until the bitmap flips and new sessions route only to ordinal 0.
	mkSessionTry := func() bool {
		resp, err := http.Post("http://"+ctlAddr+"/agents/pool/sessions", "application/json",
			strings.NewReader(`{"message":"hi"}`))
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}

	// Distribution: several sessions should land on BOTH ordinals (the replica
	// column records which). Query distinct replicas seen.
	for i := 0; i < 8; i++ {
		mkSession()
	}
	replicas := distinctReplicas(t, db, "pool")
	if !replicas[0] || !replicas[1] {
		t.Fatalf("sessions did not distribute across both ordinals: seen=%v", replicas)
	}

	// Pin a session to the SURVIVOR ordinal (0). Find one whose replica==0.
	pinned := sessionOnReplica(t, db, "pool", 0)
	if pinned == "" {
		t.Fatal("no session pinned to ordinal 0 to test affinity")
	}

	// Kill ordinal 1; runtimed must mark it unreachable and route new sessions
	// only to ordinal 0, WITHOUT restarting ordinal 1.
	killOrd(1)
	rmtWaitFor(t, 30*time.Second, func() bool {
		// New sessions should now all land on ordinal 0. A transient 503 here is
		// expected for up to one health-poll interval before the dead ordinal is
		// detected — keep trying until creates succeed AND land only on 0.
		if !mkSessionTry() {
			return false
		}
		return onlyReplica0Recent(t, db, "pool")
	}, "new sessions route only to ordinal 0 after ordinal 1 dies")

	// No restart: nothing should be serving on ordinal 1's port.
	time.Sleep(2 * time.Second)
	noRestart := &http.Client{Timeout: 2 * time.Second}
	if _, err := noRestart.Get("http://" + ords[1].addr + "/healthz"); err == nil {
		t.Fatal("ordinal 1 port still serving — runtimed must NOT restart a remote ordinal")
	}

	// Affinity + durability: the pinned (ordinal-0) session still streams.
	resp, err := http.Get(fmt.Sprintf("http://%s/agents/pool/sessions/%s", ctlAddr, pinned))
	if err != nil {
		t.Fatalf("get pinned session: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pinned session broken after ordinal 1 death: status=%d", resp.StatusCode)
	}
	_ = context.Background()
}

// distinctReplicas returns which replica indices appear in sessions for agentID.
func distinctReplicas(t *testing.T, db *sql.DB, agentID string) map[int]bool {
	t.Helper()
	rows, err := db.Query(`SELECT DISTINCT replica FROM sessions WHERE agent_id=$1`, agentID)
	if err != nil {
		t.Fatalf("distinctReplicas: %v", err)
	}
	defer rows.Close()
	out := map[int]bool{}
	for rows.Next() {
		var r int
		if err := rows.Scan(&r); err != nil {
			t.Fatal(err)
		}
		out[r] = true
	}
	return out
}

// sessionOnReplica returns one session id pinned to the given replica, or "".
func sessionOnReplica(t *testing.T, db *sql.DB, agentID string, replica int) string {
	t.Helper()
	var id string
	err := db.QueryRow(`SELECT id FROM sessions WHERE agent_id=$1 AND replica=$2 LIMIT 1`,
		agentID, replica).Scan(&id)
	if err == sql.ErrNoRows {
		return ""
	}
	if err != nil {
		t.Fatalf("sessionOnReplica: %v", err)
	}
	return id
}

// onlyReplica0Recent reports whether the 3 most-recently-created sessions are all
// on replica 0 (a proxy for "new sessions route only to ordinal 0").
func onlyReplica0Recent(t *testing.T, db *sql.DB, agentID string) bool {
	t.Helper()
	rows, err := db.Query(`SELECT replica FROM sessions WHERE agent_id=$1 ORDER BY created_at DESC LIMIT 3`, agentID)
	if err != nil {
		t.Fatalf("onlyReplica0Recent: %v", err)
	}
	defer rows.Close()
	any := false
	for rows.Next() {
		any = true
		var r int
		if err := rows.Scan(&r); err != nil {
			t.Fatal(err)
		}
		if r != 0 {
			return false
		}
	}
	return any
}
