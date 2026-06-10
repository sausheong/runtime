//go:build integration

package test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
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

// fakeEmbedSrv serves the OpenAI-compatible POST /embeddings contract the
// platform embedder speaks ({"input": "<string>"} → {"data":[{"embedding":[...]}]});
// dimension 4. Deterministic: any text containing "greet" or "say hello"
// embeds to a fixed direction (so the greet tool and the test query match
// strongly); everything else gets a hash-derived vector with zero first
// component (cosine vs the greet direction ≈ 0).
func fakeEmbedSrv(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var vec []float32
		lower := strings.ToLower(req.Input)
		if strings.Contains(lower, "greet") || strings.Contains(lower, "say hello") {
			vec = []float32{1, 0, 0, 0}
		} else {
			h := sha256.Sum256([]byte(req.Input))
			vec = []float32{0, float32(h[0])/255 + 0.01, float32(h[1])/255 + 0.01, float32(h[2])/255 + 0.01}
		}
		var out struct {
			Data []struct {
				Embedding []float32 `json:"embedding"`
			} `json:"data"`
		}
		out.Data = []struct {
			Embedding []float32 `json:"embedding"`
		}{{Embedding: vec}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
}

// TestGatewaySearchE2E boots the WHOLE stack in search mode: runtimed with a
// fake upstream + fake embeddings; an external search-mode MCP client lists
// only search_tools, searches, and calls the discovered (unlisted) tool; a
// gateway:search agent boots and completes a turn — its injected URL carries
// ?mode=search, and harness BuildRuntime connects per-turn (fatal on
// failure), so the completed turn proves the search-mode endpoint accepted
// the agent.
func TestGatewaySearchE2E(t *testing.T) {
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

	upstream := sdk.NewServer(&sdk.Implementation{Name: "fake-upstream", Version: "v0"}, nil)
	upstream.AddTool(&sdk.Tool{
		Name: "greet", Description: "greets the user with a hello message",
		InputSchema: map[string]any{"type": "object"},
	}, func(_ context.Context, _ *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "hello from upstream"}}}, nil
	})
	upstream.AddTool(&sdk.Tool{
		Name: "sum", Description: "adds two numbers together",
		InputSchema: map[string]any{"type": "object"},
	}, func(_ context.Context, _ *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "42"}}}, nil
	})
	upSrv := httptest.NewServer(sdk.NewStreamableHTTPHandler(
		func(*http.Request) *sdk.Server { return upstream }, nil))
	defer upSrv.Close()

	embSrv := fakeEmbedSrv(t)
	defer embSrv.Close()

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
	cfg := "agents:\n" +
		"  - {id: s-agent, name: S, model: test/scripted, listen_addr: 127.0.0.1:8141, gateway: search}\n" +
		"gateway:\n" +
		"  servers:\n" +
		"    - {name: fake, url: " + upSrv.URL + "}\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	ctlAddr := "127.0.0.1:8140"
	cmd := exec.Command(runtimed)
	cmd.Env = append(os.Environ(),
		"RUNTIME_PG_DSN="+dsn,
		"RUNTIME_CTL_ADDR="+ctlAddr,
		"RUNTIME_AGENTD_BIN="+agentd,
		"RUNTIME_CONFIG="+cfgPath,
		"RUNTIME_EMBED_MODEL=fake-embed",
		"RUNTIME_EMBED_DIM=4",
		"OPENAI_BASE_URL="+embSrv.URL,
		"OPENAI_API_KEY=fake",
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

	cli := sdk.NewClient(&sdk.Implementation{Name: "e2e", Version: "v0"}, nil)
	sess, err := cli.Connect(context.Background(),
		&sdk.StreamableClientTransport{Endpoint: base + "/gateway/mcp?mode=search"}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer sess.Close()

	lt, err := sess.ListTools(context.Background(), &sdk.ListToolsParams{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(lt.Tools) != 1 || lt.Tools[0].Name != "search_tools" {
		t.Fatalf("want [search_tools], got %+v", lt.Tools)
	}

	// Search until the upstream's tools are connected+indexed (bounded retry:
	// the upstream dial is async; before it completes the view has no tools
	// and search legitimately returns []).
	deadline := time.Now().Add(10 * time.Second)
	var matches []struct {
		Name string `json:"name"`
	}
	for {
		res, err := sess.CallTool(context.Background(), &sdk.CallToolParams{
			Name: "search_tools", Arguments: map[string]any{"query": "say hello to someone"},
		})
		if err != nil {
			t.Fatalf("search call: %v", err)
		}
		if !res.IsError {
			txt := res.Content[0].(*sdk.TextContent).Text
			jsonPart, _, _ := strings.Cut(txt, "\n")
			matches = matches[:0]
			if err := json.Unmarshal([]byte(jsonPart), &matches); err == nil && len(matches) > 0 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("search never returned matches")
		}
		time.Sleep(300 * time.Millisecond)
	}
	if matches[0].Name != "fake__greet" {
		t.Fatalf("want fake__greet top-1, got %+v", matches)
	}

	// NOTE: a search-mode SESSION pins its server at creation. If this
	// session was created BEFORE the upstream connected, its server has no
	// catalog tools — reconnect a FRESH session for the call phase if the
	// direct call below fails with tool-not-found on the first attempt.
	res, err := sess.CallTool(context.Background(), &sdk.CallToolParams{Name: "fake__greet"})
	if err != nil || res.IsError {
		// Fresh session: the generation has bumped since ours was built.
		sess2, cerr := cli.Connect(context.Background(),
			&sdk.StreamableClientTransport{Endpoint: base + "/gateway/mcp?mode=search"}, nil)
		if cerr != nil {
			t.Fatalf("reconnect: %v", cerr)
		}
		defer sess2.Close()
		res, err = sess2.CallTool(context.Background(), &sdk.CallToolParams{Name: "fake__greet"})
		if err != nil {
			t.Fatalf("call discovered tool (fresh session): %v", err)
		}
	}
	if res.IsError {
		t.Fatalf("discovered tool errored: %+v", res.Content)
	}
	if txt := res.Content[0].(*sdk.TextContent).Text; txt != "hello from upstream" {
		t.Fatalf("wrong result: %q", txt)
	}

	waitURL(t, base+"/agents/s-agent/healthz", 30*time.Second)
	_, body := invokeOn(t, base, "s-agent")
	if !strings.Contains(body, "final answer") {
		t.Fatalf("search-mode agent turn did not complete:\n%s", body)
	}
}
