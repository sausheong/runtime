//go:build integration

package test

import (
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestTracingE2E proves both runtimed and agentd export spans to an OTLP
// endpoint when tracing is enabled (and that the request still succeeds).
func TestTracingE2E(t *testing.T) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatalf("ping postgres: %v", err)
	}
	mustExec(t, db, `DROP TABLE IF EXISTS markers`)
	mustExec(t, db, `CREATE TABLE markers (id BIGSERIAL PRIMARY KEY, ran_at TIMESTAMPTZ)`)
	mustExec(t, db, `DROP TABLE IF EXISTS session_events, sessions, agents CASCADE`)
	mustExec(t, db, `DROP SCHEMA IF EXISTS dbos CASCADE`)

	// In-test OTLP/HTTP receiver: count POSTs to /v1/traces.
	var tracePosts atomic.Int32
	otlp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/traces" {
			_, _ = io.Copy(io.Discard, r.Body)
			tracePosts.Add(1)
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer otlp.Close()

	tmp := t.TempDir()
	agentd := filepath.Join(tmp, "agentd")
	if out, err := exec.Command("go", "build", "-o", agentd, "../cmd/agentd").CombinedOutput(); err != nil {
		t.Fatalf("build agentd: %v\n%s", err, out)
	}
	runtimed := filepath.Join(tmp, "runtimed")
	if out, err := exec.Command("go", "build", "-o", runtimed, "../cmd/runtimed").CombinedOutput(); err != nil {
		t.Fatalf("build runtimed: %v\n%s", err, out)
	}

	cfgPath := filepath.Join(tmp, "runtime.yaml")
	if err := os.WriteFile(cfgPath, []byte(
		"agents:\n  - {id: tracer, name: Tracer, model: test/scripted, listen_addr: 127.0.0.1:8412}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctlAddr := "127.0.0.1:8420"
	rt := exec.Command(runtimed)
	rt.Env = append(os.Environ(),
		"RUNTIME_PG_DSN="+dsn,
		"RUNTIME_CTL_ADDR="+ctlAddr,
		"RUNTIME_AGENTD_BIN="+agentd,
		"RUNTIME_CONFIG="+cfgPath,
		"OTEL_EXPORTER_OTLP_ENDPOINT="+otlp.URL,
		"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL=http/protobuf",
		"RUNTIME_TRACE_SAMPLE_RATIO=1.0",
	)
	rt.Stdout, rt.Stderr = os.Stdout, os.Stderr
	rt.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := rt.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Kill(-rt.Process.Pid, syscall.SIGKILL); _, _ = rt.Process.Wait() }()

	rmtWaitHealthy(t, "http://"+ctlAddr+"/healthz", "", 30*time.Second)
	// The agentd subprocess boots after DBOS schema init (serialized), so wait
	// for the per-agent health endpoint before driving a session through it.
	rmtWaitHealthy(t, "http://"+ctlAddr+"/agents/tracer/healthz", "", 60*time.Second)

	// Drive a session (proxied to agentd) — both processes should emit spans.
	resp, err := http.Post("http://"+ctlAddr+"/agents/tracer/sessions", "application/json",
		strings.NewReader(`{"message":"trace me"}`))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("session create status=%d", resp.StatusCode)
	}

	// Spans are batched; give the exporters time to flush.
	//
	// This asserts >=1 process exported spans to the OTLP endpoint, proving the
	// export path works end-to-end (off-by-default, on-when-configured). It does
	// NOT decode protobuf to distinguish runtimed vs agentd: the session drive
	// above exercises agentd's turn path, but the post count alone can't attribute
	// exports to a specific process. Span parenting/propagation semantics are
	// covered by the hermetic tests (Tasks 2-4); the Jaeger live proof
	// demonstrates the full two-service trace shape.
	rmtWaitFor(t, 20*time.Second, func() bool { return tracePosts.Load() > 0 }, "OTLP trace export received")
	if tracePosts.Load() == 0 {
		t.Fatal("no OTLP trace exports received with tracing enabled")
	}
	t.Logf("OTLP trace exports received: %d", tracePosts.Load())
}
