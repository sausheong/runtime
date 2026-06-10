//go:build integration

package test

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestGatewayE2E boots the WHOLE stack with a gateway section: runtimed
// federates a fake Streamable HTTP MCP upstream, an external MCP client lists
// and calls a tool through /gateway/mcp, and a gateway:true agent completes a
// scripted turn. BuildRuntime runs per-turn inside the DBOS step (not at agent
// boot), so the completed turn — not the healthz — is the proof that the
// agent's gateway MCP connection works.
func TestGatewayE2E(t *testing.T) {
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

	// Fake upstream MCP server over Streamable HTTP.
	upstream := sdk.NewServer(&sdk.Implementation{Name: "fake-upstream", Version: "v0"}, nil)
	upstream.AddTool(&sdk.Tool{
		Name:        "greet",
		Description: "greets",
		InputSchema: map[string]any{"type": "object"},
	}, func(_ context.Context, _ *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "hello from upstream"}}}, nil
	})
	upSrv := httptest.NewServer(sdk.NewStreamableHTTPHandler(
		func(*http.Request) *sdk.Server { return upstream }, nil))
	defer upSrv.Close()

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

	// Config: one gateway:true agent + the fake upstream. Open mode (no
	// identity), so no agent_keys needed.
	cfgPath := filepath.Join(tmp, "runtime.yaml")
	cfg := "agents:\n" +
		"  - {id: gw-agent, name: GW, model: test/scripted, listen_addr: 127.0.0.1:8131, gateway: true}\n" +
		"gateway:\n" +
		"  servers:\n" +
		"    - {name: fake, url: " + upSrv.URL + "}\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	ctlAddr := "127.0.0.1:8130"
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

	// (a) External MCP client federates through the gateway. The upstream
	// dial is async (supervision loop with backoff), so the federated tool
	// may not be visible on the very first list — poll up to ~10s.
	cli := sdk.NewClient(&sdk.Implementation{Name: "e2e", Version: "v0"}, nil)
	sess, err := cli.Connect(context.Background(),
		&sdk.StreamableClientTransport{Endpoint: base + "/gateway/mcp"}, nil)
	if err != nil {
		t.Fatalf("connect gateway: %v", err)
	}
	defer sess.Close()
	var lt *sdk.ListToolsResult
	deadline := time.Now().Add(10 * time.Second)
	for {
		lt, err = sess.ListTools(context.Background(), &sdk.ListToolsParams{})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(lt.Tools) == 1 && lt.Tools[0].Name == "fake__greet" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("want [fake__greet], got %+v", lt.Tools)
		}
		time.Sleep(200 * time.Millisecond)
	}
	res, err := sess.CallTool(context.Background(), &sdk.CallToolParams{Name: "fake__greet"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("call errored: %+v", res.Content)
	}
	if len(res.Content) == 0 {
		t.Fatal("call returned empty content")
	}
	tc, ok := res.Content[0].(*sdk.TextContent)
	if !ok {
		t.Fatalf("want *sdk.TextContent, got %T", res.Content[0])
	}
	if tc.Text != "hello from upstream" {
		t.Fatalf("wrong result: %q", tc.Text)
	}

	// (b) Gateway status visible.
	stResp, err := http.Get(base + "/gateway/status")
	if err != nil {
		t.Fatal(err)
	}
	stRaw, err := io.ReadAll(stResp.Body)
	stResp.Body.Close()
	if err != nil {
		t.Fatalf("read status body: %v", err)
	}
	stBody := string(stRaw)
	if !strings.Contains(stBody, `"fake"`) || !strings.Contains(stBody, `"up"`) {
		t.Fatalf("status missing upstream: %s", stBody)
	}

	// (c) The gateway:true agent boots healthy and completes a scripted turn.
	// BuildRuntime (and thus the gateway MCP connect) happens per-turn inside
	// the DBOS step, so the completed turn is the connectivity proof.
	waitURL(t, base+"/agents/gw-agent/healthz", 30*time.Second)
	_, body := invokeOn(t, base, "gw-agent")
	if !strings.Contains(body, "final answer") {
		t.Fatalf("gateway agent turn did not complete:\n%s", body)
	}
}
