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

// echoSpec is a tiny OpenAPI 3.0 doc whose one operation (echoHeaders) declares
// a header param X-Runtime-Tenant. servers: [] forces base_url from config. The
// declared header param lets us prove a caller cannot override an enriched
// header: sending header_X-Runtime-Tenant is rejected by the REST adapter.
const echoSpec = `
openapi: 3.0.3
info: {title: Echo, version: "1.0"}
servers: []
paths:
  /echo:
    get:
      operationId: echoHeaders
      summary: Echo received request headers as JSON
      parameters:
        - {name: X-Runtime-Tenant, in: header, required: false, schema: {type: string}}
      responses: {"200": {description: ok}}
`

// fakeEchoSrv serves the spec at /openapi.yaml and echoes the request headers it
// receives as a JSON object at /echo. Header canonicalization is Go's default
// (Textproto MIME canonical form: X-Runtime-Tenant, X-Runtime-User).
func fakeEchoSrv(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/openapi.yaml" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/yaml")
			_, _ = w.Write([]byte(echoSpec))
		case r.URL.Path == "/echo" && r.Method == http.MethodGet:
			hdrs := map[string]string{}
			for k, v := range r.Header {
				if len(v) > 0 {
					hdrs[k] = v[0]
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"headers": hdrs})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestEnrichmentInjectsClaims boots the whole stack with identity ENFORCED and
// an openapi upstream carrying enrich: {tenant: X-Runtime-Tenant, subject:
// X-Runtime-User}. An acme operator drives the echo tool; the echoed headers
// must carry the platform-set tenant and subject. A caller attempt to set
// X-Runtime-Tenant via a declared header_* arg is rejected — the platform value
// is inviolable.
func TestEnrichmentInjectsClaims(t *testing.T) {
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

	// Identity: acme tenant with a known operator key. The key's ID is the
	// principal Subject that enrichment injects as X-Runtime-User.
	st, err := identity.NewStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTenant(ctx, "acme", "Acme"); err != nil {
		t.Fatal(err)
	}
	acmeOp, _ := identity.MintServiceKey()
	if err := st.InsertServiceKey(ctx, acmeOp.ID, "acme", acmeOp.Hash, identity.RoleOperator, "acme-op"); err != nil {
		t.Fatal(err)
	}

	api := fakeEchoSrv(t)

	tmp := t.TempDir()
	agentd := buildBin(t, tmp, "agentd")
	runtimed := buildBin(t, tmp, "runtimed")

	// File config: an acme-owned agent + the openapi echo upstream scoped to
	// acme, with the enrich map wired in the servers entry. base_url is required
	// because the spec declares servers: [].
	cfgPath := filepath.Join(tmp, "runtime.yaml")
	cfg := "agents:\n" +
		"  - {id: a1, name: A1, model: test/scripted, listen_addr: 127.0.0.1:8493, tenant: acme}\n" +
		"gateway:\n" +
		"  servers:\n" +
		"    - name: echo\n" +
		"      openapi: " + api.URL + "/openapi.yaml\n" +
		"      base_url: " + api.URL + "\n" +
		"      tenants: [acme]\n" +
		"      enrich: {tenant: X-Runtime-Tenant, subject: X-Runtime-User}\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	ctlAddr := "127.0.0.1:8492"
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

	// Acme federates the generated REST tool.
	acme := connectWhenFederated(t, base, acmeOp.Plaintext, "echo__echoHeaders")

	// (1) Drive the tool as acme; the echoed headers must carry the enriched
	// tenant and subject.
	var env restEnvelope
	callJSON(t, acme, "echo__echoHeaders", nil, &env)
	if env.Status != 200 {
		t.Fatalf("echoHeaders envelope status = %d, want 200", env.Status)
	}
	var echoed struct {
		Headers map[string]string `json:"headers"`
	}
	if err := json.Unmarshal(env.Body, &echoed); err != nil {
		t.Fatalf("echo body is not the expected JSON (%v): %s", err, env.Body)
	}
	if got := echoed.Headers["X-Runtime-Tenant"]; got != "acme" {
		t.Fatalf("X-Runtime-Tenant = %q, want acme; headers=%+v", got, echoed.Headers)
	}
	if got := echoed.Headers["X-Runtime-User"]; got != acmeOp.ID {
		t.Fatalf("X-Runtime-User = %q, want %q; headers=%+v", got, acmeOp.ID, echoed.Headers)
	}

	// (2) A caller cannot override an enriched header: sending
	// header_X-Runtime-Tenant is rejected by the REST adapter (the platform
	// value is inviolable), surfaced as an MCP tool error.
	res, err := acme.CallTool(ctx, &sdk.CallToolParams{
		Name:      "echo__echoHeaders",
		Arguments: map[string]any{"header_X-Runtime-Tenant": "evilcorp"},
	})
	if err != nil {
		t.Fatalf("override call transport error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("caller override of an enriched header must be rejected: %+v", res.Content)
	}
	if len(res.Content) == 0 {
		t.Fatal("override rejection must carry content citing gateway enrichment, got empty content")
	}
	tc, ok := res.Content[0].(*sdk.TextContent)
	if !ok {
		t.Fatalf("override rejection content must be text, got: %T", res.Content[0])
	}
	if !strings.Contains(tc.Text, "enrichment") {
		t.Fatalf("override rejection should cite gateway enrichment, got: %q", tc.Text)
	}
}
