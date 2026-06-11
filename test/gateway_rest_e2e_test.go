//go:build integration

package test

import (
	"context"
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

	"github.com/sausheong/runtime/internal/identity"
)

// restSpec is the OpenAPI 3.0 document the fake REST API serves at
// /openapi.yaml. servers: [] is deliberately EMPTY so the resolved request
// base must come from the config's base_url — the override path under test.
const restSpec = `
openapi: 3.0.3
info: {title: Orders, version: "1.0"}
servers: []
paths:
  /orders:
    get:
      operationId: listOrders
      summary: List all orders
      responses: {"200": {description: ok}}
    post:
      operationId: createOrder
      summary: Create an order
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              properties: {item: {type: string}, qty: {type: integer}}
              required: [item]
      responses: {"201": {description: created}}
  /orders/{id}:
    get:
      operationId: getOrder
      summary: Get one order
      parameters:
        - {name: id, in: path, required: true, schema: {type: string}}
      responses: {"200": {description: ok}}
`

// fakeRESTSrv plays BOTH roles of a real REST integration: the spec host
// (GET /openapi.yaml) and the API itself (/orders endpoints with canned
// JSON). Any other path (e.g. the restConn liveness HEAD on the base URL)
// gets a 404 — which still counts as alive for the REST ping.
func fakeRESTSrv(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/openapi.yaml" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/yaml")
			_, _ = w.Write([]byte(restSpec))
		case r.URL.Path == "/orders" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"orders":[{"id":"o1","item":"widget"},{"id":"o2","item":"gadget"}]}`))
		case r.URL.Path == "/orders" && r.Method == http.MethodPost:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"o3","created":true}`))
		case strings.HasPrefix(r.URL.Path, "/orders/") && r.Method == http.MethodGet:
			id := strings.TrimPrefix(r.URL.Path, "/orders/")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"` + id + `","item":"widget"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// restEnvelope is the REST adapter's tool-result contract: every 2xx-or-not
// HTTP exchange comes back as one JSON object with the upstream status code.
type restEnvelope struct {
	Status    int             `json:"status"`
	Truncated bool            `json:"truncated"`
	Body      json.RawMessage `json:"body"`
}

// TestGatewayRESTE2E boots the WHOLE stack with identity ENFORCED and an
// openapi: (REST) gateway upstream scoped to tenant alpha: runtimed fetches
// the spec over HTTP, generates orders__* tools, an alpha MCP client lists
// and calls them through /gateway/mcp (envelope JSON with upstream status),
// a beta client must not see or call them (tenant scoping), and the merged
// /metrics + /gateway/status surfaces carry the openapi transport's series
// and state.
func TestGatewayRESTE2E(t *testing.T) {
	ctx := context.Background()
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

	// Identity: two tenants, one operator service key each. Rows in these
	// tables flip runtimed (spawned below) into ENFORCED mode.
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

	// The fake REST world: spec host + API in one server.
	api := fakeRESTSrv(t)

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

	// Config: one alpha-owned agent (not gateway-enabled, so no agent_keys
	// needed under enforced identity — config requires at least one agent)
	// + the openapi upstream scoped to tenant alpha. base_url is required
	// because the spec declares servers: [].
	cfgPath := filepath.Join(tmp, "runtime.yaml")
	cfg := "agents:\n" +
		"  - {id: a1, name: A1, model: test/scripted, listen_addr: 127.0.0.1:8171, tenant: alpha}\n" +
		"gateway:\n" +
		"  servers:\n" +
		"    - name: orders\n" +
		"      openapi: " + api.URL + "/openapi.yaml\n" +
		"      base_url: " + api.URL + "\n" +
		"      tenants: [alpha]\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	ctlAddr := "127.0.0.1:8170"
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
	// /healthz is exempt from identity, so the plain waiter works even in
	// enforced mode.
	waitURL(t, base+"/healthz", 15*time.Second)

	// (a) Alpha federates the generated REST tools. The spec fetch + dial is
	// async (supervision loop), so poll with fresh sessions until all three
	// generated tools appear.
	alphaSess := connectWhenFederated(t, base, alphaKey.Plaintext,
		"orders__listOrders", "orders__getOrder", "orders__createOrder")

	// (b) listOrders ⇒ the REST envelope with the upstream's 200 and JSON body.
	var env restEnvelope
	callJSON(t, alphaSess, "orders__listOrders", nil, &env)
	if env.Status != 200 {
		t.Fatalf("listOrders envelope status = %d, want 200", env.Status)
	}
	var list struct {
		Orders []map[string]any `json:"orders"`
	}
	if err := json.Unmarshal(env.Body, &list); err != nil {
		t.Fatalf("listOrders body is not the canned JSON (%v): %s", err, env.Body)
	}
	if len(list.Orders) != 2 {
		t.Fatalf("listOrders returned %d orders, want 2: %s", len(list.Orders), env.Body)
	}

	// (c) getOrder routes the path parameter into /orders/{id}.
	env = restEnvelope{}
	callJSON(t, alphaSess, "orders__getOrder", map[string]any{"id": "o1"}, &env)
	if env.Status != 200 {
		t.Fatalf("getOrder envelope status = %d, want 200", env.Status)
	}
	var got struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(env.Body, &got); err != nil || got.ID != "o1" {
		t.Fatalf("getOrder body = %s (err %v), want id o1", env.Body, err)
	}

	// (d) Beta: the orders upstream is tenants: [alpha], so beta's view has
	// ZERO orders__* tools and calling one fails. The session is fresh and
	// created after (a) proved federation, so an empty list is scoping —
	// not a not-yet-connected race.
	betaSess := connectGatewayAs(t, base, betaKey.Plaintext)
	lt, err := betaSess.ListTools(context.Background(), &sdk.ListToolsParams{})
	if err != nil {
		t.Fatalf("beta list tools: %v", err)
	}
	for _, tool := range lt.Tools {
		if strings.HasPrefix(tool.Name, "orders__") {
			t.Fatalf("tenant-scoped tool %q leaked into beta's view", tool.Name)
		}
	}
	res, err := betaSess.CallTool(context.Background(), &sdk.CallToolParams{Name: "orders__listOrders"})
	if err == nil && !res.IsError {
		t.Fatalf("beta called a tool outside its tenant scope: %+v", res.Content)
	}

	// (e) Merged /metrics (auth-free): the gateway counters carry the orders
	// server with outcome ok, and the upstream gauge reports it up. Labels
	// render alphabetically in the exposition.
	metrics := getBody(t, base+"/metrics", nil, 200)
	for _, want := range []string{
		`runtime_gateway_tool_calls_total{outcome="ok",server="orders",tool="orders__listOrders"}`,
		`runtime_gateway_tool_calls_total{outcome="ok",server="orders",tool="orders__getOrder"}`,
		`runtime_gateway_upstream_up{server="orders"} 1`,
	} {
		if !strings.Contains(metrics, want) {
			t.Fatalf("/metrics missing %q:\n%s", want, metrics)
		}
	}

	// (f) /gateway/status as alpha: the orders upstream is up with the
	// openapi transport and all three generated tools counted.
	stBody := getBody(t, base+"/gateway/status",
		map[string]string{"Authorization": "Bearer " + alphaKey.Plaintext}, 200)
	var rows []struct {
		Name      string `json:"name"`
		Transport string `json:"transport"`
		State     string `json:"state"`
		ToolCount int    `json:"tool_count"`
	}
	if err := json.Unmarshal([]byte(stBody), &rows); err != nil {
		t.Fatalf("status decode (%v): %s", err, stBody)
	}
	found := false
	for _, r := range rows {
		if r.Name == "orders" {
			found = true
			if r.State != "up" || r.Transport != "openapi" {
				t.Fatalf("orders upstream state=%q transport=%q, want up/openapi", r.State, r.Transport)
			}
			if r.ToolCount != 3 {
				t.Fatalf("orders tool_count = %d, want 3", r.ToolCount)
			}
		}
	}
	if !found {
		t.Fatalf("orders upstream missing from /gateway/status: %s", stBody)
	}
}
