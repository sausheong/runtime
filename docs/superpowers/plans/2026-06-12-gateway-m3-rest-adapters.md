# Gateway M3 — REST/OpenAPI → Tool Adapters Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Point `runtime.yaml` at an OpenAPI 3.x document and every selected operation becomes an ordinary federated gateway tool — tenant-filtered, searchable, metered, callable by any gateway-enabled agent with zero agent-side changes.

**Architecture:** A third transport inside the existing gateway Manager: `dialOpenAPI` (new dialFunc branch keyed on `cfg.OpenAPI != ""`) fetches+parses the spec with kin-openapi, generates one `tool.Tool` per selected operation, and returns a `restConn` implementing the existing `upstreamConn` seam (Tools/Ping/Close). Supervision, tenant views, M2 search, and M1 metrics all operate on `[]tool.Tool`/`upstreamConn` and apply unchanged. Spec: `docs/superpowers/specs/2026-06-12-gateway-m3-rest-adapters-design.md`.

**Tech Stack:** Go 1.25, `github.com/getkin/kin-openapi/openapi3`, httptest for all hermetic tests.

---

## Context for every task

- Branch: `gateway-m3` off `master`.
- Code facts:
  - `internal/gateway/connect.go` defines `upstreamConn{Tools() []tool.Tool; Ping(ctx) error; Close() error}`, `dialFunc func(ctx, config.GatewayServer) (upstreamConn, error)`, and `dialHarness` (the production dialer).
  - `internal/gateway/manager.go`: `NewManager` sets `dial: dialHarness`; `supervise` calls `renameTools(conn.Tools())` — `renameTools` does `strings.TrimPrefix(t.Name(), "mcp__")`, a NO-OP for REST tool names (`<server>__<op>` has no `mcp__` prefix), so REST tools pass through unchanged. Rely on this; pin with a test (Task 5).
  - `internal/config/config.go`: `GatewayServer{Name, Command, Args, Env, URL, Headers, Tenants, ForwardTenant}`; Validate() has the exactly-one-of `command`/`url` rule at ~line 184 and the `__`-in-name ban; `expandEnvMap` handles `${VAR}`.
  - harness `tool.Tool` interface: `Name() / Description() / Parameters() json.RawMessage / Execute(ctx, input json.RawMessage) (ToolResult, error) / IsConcurrencySafe(input) bool`. `tool.ToolResult{Output, Error, Metadata, Images}` — tool-level errors go in `ToolResult.Error` (string), Go errors are transport-level.
  - Gateway metrics: `Handler.toolHandler` wraps `t.Execute` — REST tools get `runtime_gateway_tool_calls_total` for free.
- Run per task: `go test ./internal/gateway/ ./internal/config/ -count=1`; full sweep before merge.
- Spec section references (§) are to the design doc above.

---

### Task 1: Config — `openapi:`, `base_url:`, `operations:` + validation

**Files:**
- Modify: `internal/config/config.go` (GatewayServer struct ~line 79, Validate ~line 184)
- Test: `internal/config/config_test.go` (append)

- [ ] **Step 1: Write failing tests** (append to `internal/config/config_test.go`, matching its existing test style — read a couple of neighbors first):

```go
func TestGatewayServerOpenAPIValid(t *testing.T) {
	cfg := minimalConfigWithGateway(t, `
  servers:
    - name: orders
      openapi: ./specs/orders.yaml
      base_url: http://localhost:9000
      operations: ["listOrders", "GET /orders/*"]
`)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid openapi server rejected: %v", err)
	}
	s := cfg.Gateway.Servers[0]
	if s.OpenAPI != "./specs/orders.yaml" || s.BaseURL != "http://localhost:9000" || len(s.Operations) != 2 {
		t.Fatalf("fields not parsed: %+v", s)
	}
}

func TestGatewayServerExactlyOneTransport(t *testing.T) {
	cases := []struct{ name, body string; wantErr bool }{
		{"openapi only", "openapi: ./s.yaml", false},
		{"command only", "command: ./bin", false},
		{"url only", "url: http://x/mcp", false},
		{"openapi+url", "openapi: ./s.yaml\n      url: http://x/mcp", true},
		{"openapi+command", "openapi: ./s.yaml\n      command: ./bin", true},
		{"none", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := minimalConfigWithGateway(t, "\n  servers:\n    - name: s1\n      "+tc.body+"\n")
			err := cfg.Validate()
			if tc.wantErr && err == nil {
				t.Fatal("want error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("want ok, got %v", err)
			}
		})
	}
}

func TestGatewayOpenAPIFieldRules(t *testing.T) {
	cases := []struct{ name, body string; wantSub string }{
		{"forward_tenant rejected", "openapi: ./s.yaml\n      forward_tenant: true", "forward_tenant"},
		{"base_url needs openapi", "command: ./bin\n      base_url: http://x", "base_url"},
		{"operations needs openapi", "url: http://x/mcp\n      operations: [\"a\"]", "operations"},
		{"bad operation pattern", "openapi: ./s.yaml\n      operations: [\"get /x\"]", "operations"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := minimalConfigWithGateway(t, "\n  servers:\n    - name: s1\n      "+tc.body+"\n")
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("want error containing %q, got %v", tc.wantSub, err)
			}
		})
	}
}
```

If no helper like `minimalConfigWithGateway` exists in config_test.go, write one: builds a Config by yaml.Unmarshal of a minimal agents section + the given gateway fragment. Match how existing gateway-config tests construct configs — REUSE their helper if one exists.

- [ ] **Step 2: Run, verify FAIL** — `go test ./internal/config/ -run 'OpenAPI|ExactlyOneTransport|FieldRules' -count=1` → unknown field errors.

- [ ] **Step 3: Implement.** GatewayServer gains (after ForwardTenant):

```go
	// OpenAPI declares a REST upstream: a path or URL to an OpenAPI 3.x
	// document whose operations become gateway tools (third transport,
	// mutually exclusive with Command and URL).
	OpenAPI string `yaml:"openapi"`
	// BaseURL overrides the spec's servers[0] entry as the request base.
	// Only valid with OpenAPI. Required at dial time if the spec declares
	// no usable server entry.
	BaseURL string `yaml:"base_url"`
	// Operations is an optional allowlist: operationIds or "METHOD /glob"
	// patterns (path.Match syntax). Empty ⇒ all operations. Only valid
	// with OpenAPI.
	Operations []string `yaml:"operations"`
```

Validate() — replace the exactly-one rule and add field rules:

```go
		set := 0
		for _, v := range []string{s.Command, s.URL, s.OpenAPI} {
			if v != "" {
				set++
			}
		}
		if set != 1 {
			return fmt.Errorf("config: gateway server %q requires exactly one of command, url, or openapi", s.Name)
		}
		if s.ForwardTenant && s.Command == "" {
			return fmt.Errorf("config: gateway server %q: forward_tenant requires a stdio (command:) upstream", s.Name)
		}
		if s.OpenAPI == "" && (s.BaseURL != "" || len(s.Operations) > 0) {
			return fmt.Errorf("config: gateway server %q: base_url/operations are only valid with openapi", s.Name)
		}
		for _, op := range s.Operations {
			if err := validateOperationPattern(op); err != nil {
				return fmt.Errorf("config: gateway server %q operations: %w", s.Name, err)
			}
		}
```

(The ForwardTenant rule generalizes the old `s.URL != ""` check — verify the old error message's test still passes; update its expected text if it asserted the exact string.)

Add:

```go
// validateOperationPattern accepts a bare operationId (no space) or
// "METHOD /glob" where METHOD is uppercase and glob is path.Match syntax.
func validateOperationPattern(p string) error {
	if p == "" {
		return fmt.Errorf("operations entry must not be empty")
	}
	method, rest, found := strings.Cut(p, " ")
	if !found {
		return nil // bare operationId
	}
	switch method {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS":
	default:
		return fmt.Errorf("operations entry %q: method must be uppercase HTTP verb", p)
	}
	if !strings.HasPrefix(rest, "/") {
		return fmt.Errorf("operations entry %q: path must start with /", p)
	}
	if _, err := path.Match(rest, "/probe"); err != nil {
		return fmt.Errorf("operations entry %q: bad glob: %w", p, err)
	}
	return nil
}
```

(import `path`.)

- [ ] **Step 4: Run, verify PASS** — `go test ./internal/config/ -count=1` (ALL config tests; fix any old exactly-one-message assertions).

- [ ] **Step 5: Commit** — `git add internal/config/ && git commit -m "feat(config): openapi/base_url/operations gateway server fields"`

---

### Task 2: Spec→tool generation (`internal/gateway/openapi.go`)

**Files:**
- Create: `internal/gateway/openapi.go`
- Test: `internal/gateway/openapi_test.go`
- Dep: `go get github.com/getkin/kin-openapi@latest`

This task builds generation ONLY (no HTTP execution — Execute lands in Task 3). Define the restTool struct with the fields Execute will need, but its Execute returns a not-wired error for now.

- [ ] **Step 1: Write failing tests** (`internal/gateway/openapi_test.go`). Use a single const testSpec (OpenAPI 3.0 YAML, inline) exercising every mapping rule:

```go
package gateway

import (
	"encoding/json"
	"strings"
	"testing"
)

const testSpec = `
openapi: 3.0.3
info: {title: Orders, version: "1.0"}
servers:
  - url: http://spec-default:8000
paths:
  /orders:
    get:
      operationId: listOrders
      summary: List all orders
      parameters:
        - {name: status, in: query, schema: {type: string}}
        - {name: limit, in: query, required: true, schema: {type: integer}}
        - {name: X-Trace, in: header, schema: {type: string}}
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
      parameters:
        - {name: id, in: path, required: true, schema: {type: string}}
      responses: {"200": {description: ok}}
    delete:
      # no operationId → fallback slug delete_orders_id
      parameters:
        - {name: id, in: path, required: true, schema: {type: string}}
      responses: {"204": {description: gone}}
  /weird__path:
    get:
      # no operationId → slug must collapse __ : get_weird_path
      responses: {"200": {description: ok}}
`

func gen(t *testing.T, baseURL string, operations []string) []restTool {
	t.Helper()
	tools, err := generateTools("orders", []byte(testSpec), baseURL, operations, nil)
	if err != nil {
		t.Fatal(err)
	}
	return tools
}

func toolNames(ts []restTool) []string {
	var out []string
	for _, tt := range ts {
		out = append(out, tt.Name())
	}
	return out
}

func TestGenerateAllOperations(t *testing.T) {
	ts := gen(t, "", nil)
	names := strings.Join(toolNames(ts), ",")
	for _, want := range []string{
		"orders__listOrders", "orders__createOrder", "orders__getOrder",
		"orders__delete_orders_id", "orders__get_weird_path",
	} {
		if !strings.Contains(names, want) {
			t.Fatalf("missing %s in %s", want, names)
		}
	}
}

func TestBaseURLResolution(t *testing.T) {
	if got := gen(t, "http://override:9000", nil)[0].baseURL; got != "http://override:9000" {
		t.Fatalf("config override ignored: %s", got)
	}
	if got := gen(t, "", nil)[0].baseURL; got != "http://spec-default:8000" {
		t.Fatalf("spec servers[0] not used: %s", got)
	}
	specNoServers := strings.Replace(testSpec, "servers:\n  - url: http://spec-default:8000\n", "", 1)
	if _, err := generateTools("orders", []byte(specNoServers), "", nil, nil); err == nil {
		t.Fatal("no base_url anywhere must be a dial error")
	}
}

func TestOperationsFilter(t *testing.T) {
	ts := gen(t, "", []string{"listOrders"})
	if len(ts) != 1 || ts[0].Name() != "orders__listOrders" {
		t.Fatalf("id filter: %v", toolNames(ts))
	}
	ts = gen(t, "", []string{"GET /orders/*"})
	if len(ts) != 1 || ts[0].Name() != "orders__getOrder" {
		t.Fatalf("glob filter: %v", toolNames(ts))
	}
	ts = gen(t, "", []string{"GET /orders"})
	if len(ts) != 1 || ts[0].Name() != "orders__listOrders" {
		t.Fatalf("exact path filter: %v", toolNames(ts))
	}
}

func TestSchemaMapping(t *testing.T) {
	var listOrders, createOrder, getOrder restTool
	for _, tt := range gen(t, "", nil) {
		switch tt.Name() {
		case "orders__listOrders":
			listOrders = tt
		case "orders__createOrder":
			createOrder = tt
		case "orders__getOrder":
			getOrder = tt
		}
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(listOrders.Parameters(), &schema); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"status", "limit", "header_X-Trace"} {
		if _, ok := schema.Properties[p]; !ok {
			t.Fatalf("listOrders missing property %s: %v", p, schema.Properties)
		}
	}
	if !contains(schema.Required, "limit") || contains(schema.Required, "status") {
		t.Fatalf("required wrong: %v", schema.Required)
	}

	if err := json.Unmarshal(createOrder.Parameters(), &schema); err != nil {
		t.Fatal(err)
	}
	if _, ok := schema.Properties["body"]; !ok || !contains(schema.Required, "body") {
		t.Fatalf("createOrder body mapping: props=%v req=%v", schema.Properties, schema.Required)
	}

	if err := json.Unmarshal(getOrder.Parameters(), &schema); err != nil {
		t.Fatal(err)
	}
	if !contains(schema.Required, "id") {
		t.Fatalf("path param not required: %v", schema.Required)
	}
}

func TestDescriptionPrefix(t *testing.T) {
	for _, tt := range gen(t, "", nil) {
		if tt.Name() == "orders__listOrders" {
			d := tt.Description()
			if !strings.HasPrefix(d, "GET /orders — ") || !strings.Contains(d, "List all orders") {
				t.Fatalf("description shape: %q", d)
			}
		}
	}
}

func TestConcurrencySafety(t *testing.T) {
	for _, tt := range gen(t, "", nil) {
		safe := tt.IsConcurrencySafe(nil)
		isGet := strings.HasPrefix(tt.method+" ", "GET ") || tt.method == "HEAD"
		if safe != isGet {
			t.Fatalf("%s (%s): safe=%v", tt.Name(), tt.method, safe)
		}
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
```

- [ ] **Step 2: Run, verify FAIL** — `go get github.com/getkin/kin-openapi@latest && go test ./internal/gateway/ -run 'Generate|BaseURL|OperationsFilter|SchemaMapping|Description|Concurrency' -count=1` → undefined generateTools/restTool.

- [ ] **Step 3: Implement `internal/gateway/openapi.go`:**

```go
package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"sort"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

const (
	maxDescriptionLen = 1024
	// largeSpecWarn nudges operators toward an operations: filter when one
	// spec floods the catalog (spec §11).
	largeSpecWarn = 50
)

// generateTools parses an OpenAPI 3.x document and returns one restTool per
// selected operation. client may be nil (tools get a default per-upstream
// client at dial; tests inject httptest clients).
func generateTools(server string, specBytes []byte, baseURL string, operations []string, client *http.Client) ([]restTool, error) {
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(specBytes)
	if err != nil {
		return nil, fmt.Errorf("openapi parse: %w", err)
	}
	if err := doc.Validate(loader.Context); err != nil {
		return nil, fmt.Errorf("openapi validate: %w", err)
	}
	if baseURL == "" {
		if len(doc.Servers) > 0 && doc.Servers[0].URL != "" {
			baseURL = doc.Servers[0].URL
		} else {
			return nil, fmt.Errorf("openapi: no base_url configured and spec declares no servers[] entry")
		}
	}
	baseURL = strings.TrimRight(baseURL, "/")

	var tools []restTool
	seen := map[string]bool{}
	// Deterministic order: sort paths, then fixed method order.
	paths := make([]string, 0, doc.Paths.Len())
	for p := range doc.Paths.Map() {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	methodOrder := []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"}
	for _, p := range paths {
		item := doc.Paths.Value(p)
		ops := item.Operations() // map[method]*Operation
		for _, method := range methodOrder {
			op, ok := ops[method]
			if !ok {
				continue
			}
			if !operationSelected(operations, op.OperationID, method, p) {
				continue
			}
			name := toolNameFor(op.OperationID, method, p)
			if seen[name] {
				slog.Warn("gateway openapi: duplicate tool name after sanitization; skipping",
					"server", server, "operation", method+" "+p, "name", name)
				continue
			}
			t, err := buildRestTool(server, name, method, p, baseURL, item, op, client)
			if err != nil {
				slog.Warn("gateway openapi: skipping unmappable operation",
					"server", server, "operation", method+" "+p, "err", err)
				continue
			}
			seen[name] = true
			tools = append(tools, t)
		}
	}
	if len(tools) > largeSpecWarn {
		slog.Warn("gateway openapi: large tool catalog from one spec — consider an operations: filter",
			"server", server, "tools", len(tools))
	}
	if len(tools) == 0 {
		slog.Warn("gateway openapi: spec produced zero tools (filter too narrow or empty spec)",
			"server", server)
	}
	return tools, nil
}

// operationSelected applies the operations allowlist (empty ⇒ all).
// Entries are bare operationIds or "METHOD /glob" (path.Match).
func operationSelected(allow []string, opID, method, specPath string) bool {
	if len(allow) == 0 {
		return true
	}
	for _, a := range allow {
		m, glob, found := strings.Cut(a, " ")
		if !found {
			if a == opID && opID != "" {
				return true
			}
			continue
		}
		if m != method {
			continue
		}
		if ok, _ := path.Match(glob, specPath); ok {
			return true
		}
	}
	return false
}

// toolNameFor is operationId, else a slug of "<method>_<path>". "__" is the
// reserved gateway separator, so any run of underscores collapses to one.
func toolNameFor(opID, method, specPath string) string {
	base := opID
	if base == "" {
		base = strings.ToLower(method) + "_" + specPath
	}
	var b strings.Builder
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := b.String()
	for strings.Contains(out, "__") {
		out = strings.ReplaceAll(out, "__", "_")
	}
	return strings.Trim(out, "_")
}

// buildRestTool maps one operation to a restTool: merged input schema
// (path/query/header_*/body) and request metadata for Execute.
func buildRestTool(server, name, method, specPath, baseURL string, item *openapi3.PathItem, op *openapi3.Operation, client *http.Client) (restTool, error) {
	props := map[string]json.RawMessage{}
	var required []string
	pathParams := map[string]bool{}
	queryParams := map[string]bool{}
	headerParams := map[string]string{} // schema property → wire header name

	// PathItem-level parameters apply to all its operations; operation-level
	// parameters override by (name, in). Merge both.
	all := append(append([]*openapi3.ParameterRef{}, item.Parameters...), op.Parameters...)
	for _, pref := range all {
		p := pref.Value
		if p == nil {
			return restTool{}, fmt.Errorf("unresolved parameter ref")
		}
		schemaJSON, err := schemaToJSON(p.Schema)
		if err != nil {
			return restTool{}, fmt.Errorf("parameter %s: %w", p.Name, err)
		}
		switch p.In {
		case "path":
			props[p.Name] = schemaJSON
			required = append(required, p.Name) // path params always required
			pathParams[p.Name] = true
		case "query":
			props[p.Name] = schemaJSON
			if p.Required {
				required = append(required, p.Name)
			}
			queryParams[p.Name] = true
		case "header":
			prop := "header_" + p.Name
			props[prop] = schemaJSON
			if p.Required {
				required = append(required, prop)
			}
			headerParams[prop] = p.Name
		default:
			// cookie etc.: skip the parameter, keep the operation
		}
	}

	hasBody := false
	if op.RequestBody != nil && op.RequestBody.Value != nil {
		mt := op.RequestBody.Value.Content.Get("application/json")
		if mt == nil {
			return restTool{}, fmt.Errorf("request body has no application/json media type")
		}
		schemaJSON, err := schemaToJSON(mt.Schema)
		if err != nil {
			return restTool{}, fmt.Errorf("request body: %w", err)
		}
		props["body"] = schemaJSON
		hasBody = true
		if op.RequestBody.Value.Required {
			required = append(required, "body")
		}
	}

	schema := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		schema["required"] = required
	}
	schemaBytes, err := json.Marshal(schema)
	if err != nil {
		return restTool{}, err
	}

	desc := method + " " + specPath + " — "
	prose := strings.TrimSpace(strings.TrimSpace(op.Summary) + " " + strings.TrimSpace(op.Description))
	desc += prose
	if len(desc) > maxDescriptionLen {
		desc = desc[:maxDescriptionLen]
	}

	return restTool{
		server: server, name: server + "__" + name,
		method: method, specPath: specPath, baseURL: baseURL,
		description: desc, schema: schemaBytes,
		pathParams: pathParams, queryParams: queryParams, headerParams: headerParams,
		hasBody: hasBody, requiredFields: required, client: client,
	}, nil
}

// schemaToJSON marshals a (resolved) schema ref; nil schema ⇒ permissive {}.
func schemaToJSON(ref *openapi3.SchemaRef) (json.RawMessage, error) {
	if ref == nil || ref.Value == nil {
		return json.RawMessage(`{}`), nil
	}
	return ref.Value.MarshalJSON()
}
```

And the restTool struct + interface stubs (Execute lands in Task 3) — put the struct in openapi.go for now; Task 3 moves Execute logic into restconn.go:

```go
// restTool is one generated REST operation exposed as a gateway tool.
type restTool struct {
	server, name, method, specPath, baseURL, description string
	schema                                               json.RawMessage
	pathParams, queryParams                              map[string]bool
	headerParams                                         map[string]string // schema prop → wire header
	hasBody                                              bool
	requiredFields                                       []string
	staticHeaders                                        map[string]string // config headers (set at dial)
	client                                               *http.Client
}

func (r restTool) Name() string                  { return r.name }
func (r restTool) Description() string           { return r.description }
func (r restTool) Parameters() json.RawMessage   { return r.schema }
func (r restTool) IsConcurrencySafe(json.RawMessage) bool {
	return r.method == "GET" || r.method == "HEAD"
}

func (r restTool) Execute(ctx context.Context, input json.RawMessage) (tool.ToolResult, error) {
	return tool.ToolResult{Error: "not wired"}, nil // Task 3
}
```

(import harness `tool`; adjust imports as the compiler dictates.)

- [ ] **Step 4: Run, verify PASS** — `go test ./internal/gateway/ -count=1` (pre-existing tests stay green). `go mod tidy`.

- [ ] **Step 5: Commit** — `git add internal/gateway/openapi.go internal/gateway/openapi_test.go go.mod go.sum && git commit -m "feat(gateway): OpenAPI spec→tool generation with filtering and schema mapping"`

---

### Task 3: restTool.Execute (`internal/gateway/restconn.go`)

**Files:**
- Create: `internal/gateway/restconn.go` (move Execute here; openapi.go keeps generation)
- Test: `internal/gateway/restconn_test.go`

- [ ] **Step 1: Write failing tests.** Use httptest.Server as the API; build restTools via `generateTools` against testSpec with `baseURL` pointed at the httptest server and the test client injected:

```go
package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sausheong/harness/tool"
)

// execTool finds a generated tool by short name and runs Execute.
func execTool(t *testing.T, srv *httptest.Server, static map[string]string, short string, input string) tool.ToolResult {
	t.Helper()
	tools, err := generateTools("orders", []byte(testSpec), srv.URL, nil, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	for i := range tools {
		if tools[i].Name() == "orders__"+short {
			tools[i].staticHeaders = static
			res, err := tools[i].Execute(context.Background(), json.RawMessage(input))
			if err != nil {
				t.Fatalf("transport error: %v", err)
			}
			return res
		}
	}
	t.Fatalf("tool %s not found", short)
	return tool.ToolResult{}
}

// envelope decodes the JSON result the agent sees.
type envelope struct {
	Status    int               `json:"status"`
	Headers   map[string]string `json:"headers"`
	Body      json.RawMessage   `json:"body"`
	Truncated bool              `json:"truncated"`
}

func decodeEnv(t *testing.T, res tool.ToolResult) envelope {
	t.Helper()
	if res.Error != "" {
		t.Fatalf("unexpected tool error: %s", res.Error)
	}
	var e envelope
	if err := json.Unmarshal([]byte(res.Output), &e); err != nil {
		t.Fatalf("bad envelope %q: %v", res.Output, err)
	}
	return e
}

func TestExecutePathQueryBody(t *testing.T) {
	var gotPath, gotQuery, gotBody, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		if r.Body != nil {
			b := make([]byte, 1024)
			n, _ := r.Body.Read(b)
			gotBody = string(b[:n])
		}
		gotCT = r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()

	e := decodeEnv(t, execTool(t, srv, nil, "getOrder", `{"id":"ord 42"}`))
	if gotPath != "/orders/ord%2042" && gotPath != "/orders/ord 42" {
		t.Fatalf("path interpolation: %s", gotPath)
	}
	if e.Status != 200 || !strings.Contains(string(e.Body), "true") {
		t.Fatalf("envelope: %+v", e)
	}

	decodeEnv(t, execTool(t, srv, nil, "listOrders", `{"limit":5,"status":"open"}`))
	if !strings.Contains(gotQuery, "limit=5") || !strings.Contains(gotQuery, "status=open") {
		t.Fatalf("query: %s", gotQuery)
	}

	decodeEnv(t, execTool(t, srv, nil, "createOrder", `{"body":{"item":"widget","qty":2}}`))
	if !strings.Contains(gotBody, `"widget"`) || gotCT != "application/json" {
		t.Fatalf("body=%s ct=%s", gotBody, gotCT)
	}
}

func TestExecuteValidationErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("request must not reach the server")
	}))
	defer srv.Close()
	cases := []struct{ name, tool, input, wantSub string }{
		{"missing required path", "getOrder", `{}`, "id"},
		{"missing required query", "listOrders", `{}`, "limit"},
		{"missing required body", "createOrder", `{}`, "body"},
		{"traversal dotdot", "getOrder", `{"id":".."}`, "path"},
		{"traversal slash", "getOrder", `{"id":"a/b"}`, "path"},
		{"traversal encoded", "getOrder", `{"id":"a%2Fb"}`, "path"},
		{"header override", "listOrders", `{"limit":1,"header_X-Trace":"t","header_Authorization":"evil"}`, "header"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			static := map[string]string{"Authorization": "Bearer real"}
			tools, _ := generateTools("orders", []byte(testSpec), srv.URL, nil, srv.Client())
			for i := range tools {
				if tools[i].Name() == "orders__"+tc.tool {
					tools[i].staticHeaders = static
					res, err := tools[i].Execute(context.Background(), json.RawMessage(tc.input))
					if err != nil {
						t.Fatalf("want tool error, got transport error %v", err)
					}
					if res.Error == "" || !strings.Contains(strings.ToLower(res.Error), tc.wantSub) {
						t.Fatalf("want error containing %q, got %q", tc.wantSub, res.Error)
					}
					return
				}
			}
			t.Fatal("tool not found")
		})
	}
}

func TestExecuteHeaderPrecedenceAndSpecHeaders(t *testing.T) {
	var gotAuth, gotTrace string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotTrace = r.Header.Get("Authorization"), r.Header.Get("X-Trace")
		fmt.Fprint(w, "{}")
	}))
	defer srv.Close()
	decodeEnv(t, execTool(t, srv, map[string]string{"Authorization": "Bearer real"},
		"listOrders", `{"limit":1,"header_X-Trace":"trace-1"}`))
	if gotAuth != "Bearer real" || gotTrace != "trace-1" {
		t.Fatalf("auth=%q trace=%q", gotAuth, gotTrace)
	}
}

func TestExecute4xxIsResultNotError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"no such order"}`, 404)
	}))
	defer srv.Close()
	e := decodeEnv(t, execTool(t, srv, nil, "getOrder", `{"id":"x"}`))
	if e.Status != 404 {
		t.Fatalf("status: %d", e.Status)
	}
}

func TestExecuteNonJSONBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "plain text")
	}))
	defer srv.Close()
	e := decodeEnv(t, execTool(t, srv, nil, "getOrder", `{"id":"x"}`))
	var s string
	if err := json.Unmarshal(e.Body, &s); err != nil || s != "plain text" {
		t.Fatalf("non-JSON body: %s", e.Body)
	}
}

func TestExecuteTruncation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(make([]byte, 2<<20)) // 2 MiB of zeros
	}))
	defer srv.Close()
	e := decodeEnv(t, execTool(t, srv, nil, "getOrder", `{"id":"x"}`))
	if !e.Truncated {
		t.Fatal("truncated flag not set")
	}
}

func TestExecuteRedirectPolicy(t *testing.T) {
	var hits int
	srv := httptest.NewServerTLS // placeholder to force thought; see note below
	_ = srv
	_ = hits
	// Same-host redirect followed:
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/orders/x" {
			http.Redirect(w, r, "/orders/final", http.StatusFound)
			return
		}
		fmt.Fprint(w, `{"redirected":true}`)
	}))
	defer target.Close()
	e := decodeEnv(t, execTool(t, target, nil, "getOrder", `{"id":"x"}`))
	if e.Status != 200 || !strings.Contains(string(e.Body), "redirected") {
		t.Fatalf("same-host redirect not followed: %+v", e)
	}
	// Cross-host redirect refused:
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("cross-host target must not be reached")
	}))
	defer other.Close()
	bouncer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusFound)
	}))
	defer bouncer.Close()
	tools, _ := generateTools("orders", []byte(testSpec), bouncer.URL, nil, nil) // nil client: dial-built client carries the policy
	for i := range tools {
		if tools[i].Name() == "orders__getOrder" {
			tools[i].client = newRestClient() // the production client constructor
			res, err := tools[i].Execute(context.Background(), json.RawMessage(`{"id":"x"}`))
			if err != nil {
				t.Fatalf("transport: %v", err)
			}
			if res.Error == "" || !strings.Contains(res.Error, "redirect") {
				t.Fatalf("cross-host redirect not refused: %+v", res)
			}
			return
		}
	}
}
```

(Clean up the placeholder lines in TestExecuteRedirectPolicy when writing the real file — the same-host/cross-host structure is the contract. NOTE: the cross-host test must use `newRestClient()` because httptest's `srv.Client()` has no redirect policy.)

- [ ] **Step 2: Run, verify FAIL** — Execute returns "not wired".

- [ ] **Step 3: Implement `internal/gateway/restconn.go`** (move Execute off the stub):

```go
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sausheong/harness/tool"
)

const (
	restRequestTimeout = 30 * time.Second
	restMaxResponse    = 1 << 20 // 1 MiB
	restMaxRedirects   = 3
)

// newRestClient builds the per-upstream HTTP client: bounded timeout and a
// same-host-only redirect policy (a compromised upstream must not bounce the
// gateway's credentials to another host).
func newRestClient() *http.Client {
	return &http.Client{
		Timeout: restRequestTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= restMaxRedirects {
				return fmt.Errorf("too many redirects")
			}
			if req.URL.Host != via[0].URL.Host {
				return fmt.Errorf("cross-host redirect refused (%s -> %s)", via[0].URL.Host, req.URL.Host)
			}
			return nil
		},
	}
}

// Execute performs the HTTP request for one generated REST tool. HTTP 4xx/5xx
// are RESULTS (the agent reasons about them); tool errors are reserved for
// validation failures and transport problems. See spec §6.
func (r restTool) Execute(ctx context.Context, input json.RawMessage) (tool.ToolResult, error) {
	args := map[string]json.RawMessage{}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &args); err != nil {
			return tool.ToolResult{Error: "invalid arguments: " + err.Error()}, nil
		}
	}
	for _, req := range r.requiredFields {
		if _, ok := args[req]; !ok {
			return tool.ToolResult{Error: "missing required field: " + req}, nil
		}
	}

	// Path interpolation with traversal guard.
	urlPath := r.specPath
	for name := range r.pathParams {
		raw, ok := args[name]
		if !ok {
			continue // required-check above already caught missing required
		}
		val := scalarString(raw)
		if strings.Contains(val, "/") || strings.Contains(val, "..") ||
			strings.Contains(strings.ToLower(val), "%2f") || strings.Contains(strings.ToLower(val), "%2e%2e") {
			return tool.ToolResult{Error: fmt.Sprintf("invalid path parameter %q: path separators and traversal sequences are not allowed", name)}, nil
		}
		urlPath = strings.ReplaceAll(urlPath, "{"+name+"}", url.PathEscape(val))
	}

	// Query.
	q := url.Values{}
	for name := range r.queryParams {
		raw, ok := args[name]
		if !ok {
			continue
		}
		var arr []json.RawMessage
		if json.Unmarshal(raw, &arr) == nil && len(raw) > 0 && raw[0] == '[' {
			vals := make([]string, len(arr))
			for i, a := range arr {
				vals[i] = scalarString(a)
			}
			q.Set(name, strings.Join(vals, ",")) // form/comma default style
		} else {
			q.Set(name, scalarString(raw))
		}
	}

	// Body.
	var bodyReader io.Reader
	if r.hasBody {
		if raw, ok := args["body"]; ok {
			bodyReader = bytes.NewReader(raw)
		}
	}

	fullURL := r.baseURL + urlPath
	if enc := q.Encode(); enc != "" {
		fullURL += "?" + enc
	}
	req, err := http.NewRequestWithContext(ctx, r.method, fullURL, bodyReader)
	if err != nil {
		return tool.ToolResult{Error: "request build: " + err.Error()}, nil
	}

	// Headers: config statics are inviolable (case-insensitive).
	for k, v := range r.staticHeaders {
		req.Header.Set(k, v)
	}
	for prop, wireName := range r.headerParams {
		raw, ok := args[prop]
		if !ok {
			continue
		}
		if _, clash := headerClash(r.staticHeaders, wireName); clash {
			return tool.ToolResult{Error: fmt.Sprintf("header %q is set by gateway configuration and cannot be overridden", wireName)}, nil
		}
		req.Header.Set(wireName, scalarString(raw))
	}
	// Reject ANY header_* arg targeting a configured header even if the spec
	// didn't declare it (agents can only send declared ones — this is belt
	// and braces against schema-validation gaps).
	for argName := range args {
		if strings.HasPrefix(argName, "header_") {
			if _, clash := headerClash(r.staticHeaders, strings.TrimPrefix(argName, "header_")); clash {
				return tool.ToolResult{Error: fmt.Sprintf("header %q is set by gateway configuration and cannot be overridden", strings.TrimPrefix(argName, "header_"))}, nil
			}
		}
	}
	if bodyReader != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	client := r.client
	if client == nil {
		client = newRestClient()
	}
	resp, err := client.Do(req)
	if err != nil {
		// Surface redirect-policy refusals as tool errors with the cause.
		return tool.ToolResult{Error: "request failed: " + err.Error()}, nil
	}
	defer resp.Body.Close()

	limited := io.LimitReader(resp.Body, restMaxResponse+1)
	bodyBytes, err := io.ReadAll(limited)
	if err != nil {
		return tool.ToolResult{Error: "response read: " + err.Error()}, nil
	}
	truncated := false
	if len(bodyBytes) > restMaxResponse {
		bodyBytes, truncated = bodyBytes[:restMaxResponse], true
	}

	envelope := map[string]any{
		"status":    resp.StatusCode,
		"headers":   map[string]string{"content-type": resp.Header.Get("Content-Type")},
		"truncated": truncated,
	}
	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "json") && !truncated && json.Valid(bodyBytes) {
		envelope["body"] = json.RawMessage(bodyBytes)
	} else {
		envelope["body"] = string(bodyBytes)
	}
	out, err := json.Marshal(envelope)
	if err != nil {
		return tool.ToolResult{Error: "envelope encode: " + err.Error()}, nil
	}
	return tool.ToolResult{Output: string(out)}, nil
}

// scalarString renders a JSON scalar as its string form without quotes.
func scalarString(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return strings.TrimSpace(string(raw))
}

// headerClash reports whether name collides (case-insensitively) with a
// configured static header.
func headerClash(static map[string]string, name string) (string, bool) {
	for k := range static {
		if strings.EqualFold(k, name) {
			return k, true
		}
	}
	return "", false
}
```

Remove the Execute stub from openapi.go.

- [ ] **Step 4: Run, verify PASS** — `go test ./internal/gateway/ -count=1`.

- [ ] **Step 5: Commit** — `git add internal/gateway/ && git commit -m "feat(gateway): REST tool execution — URL build, header precedence, response envelope"`

---

### Task 4: restConn + dialOpenAPI + connect.go branch

**Files:**
- Modify: `internal/gateway/restconn.go` (add restConn + dialOpenAPI)
- Modify: `internal/gateway/connect.go` (branch in production dialer)
- Test: `internal/gateway/restconn_test.go` (append)

- [ ] **Step 1: Write failing tests** (append to restconn_test.go):

```go
func specServer(t *testing.T, spec string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/openapi.yaml" {
			fmt.Fprint(w, spec)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDialOpenAPIFetchesAndGenerates(t *testing.T) {
	srv := specServer(t, testSpec)
	conn, err := dialOpenAPI(context.Background(), config.GatewayServer{
		Name: "orders", OpenAPI: srv.URL + "/openapi.yaml", BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if n := len(conn.Tools()); n != 5 {
		t.Fatalf("tools = %d, want 5", n)
	}
	if err := conn.Ping(context.Background()); err != nil {
		t.Fatalf("ping against live server: %v", err)
	}
}

func TestDialOpenAPILocalFile(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/spec.yaml"
	if err := os.WriteFile(p, []byte(testSpec), 0o644); err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "{}")
	}))
	defer api.Close()
	conn, err := dialOpenAPI(context.Background(), config.GatewayServer{
		Name: "orders", OpenAPI: p, BaseURL: api.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if len(conn.Tools()) == 0 {
		t.Fatal("no tools from local file spec")
	}
}

func TestDialOpenAPIBadSpecIsDialError(t *testing.T) {
	srv := specServer(t, "not: [valid: openapi")
	if _, err := dialOpenAPI(context.Background(), config.GatewayServer{
		Name: "x", OpenAPI: srv.URL + "/openapi.yaml", BaseURL: srv.URL,
	}); err == nil {
		t.Fatal("bad spec must fail the dial (upstream down, retried with backoff)")
	}
}

func TestPingSemantics(t *testing.T) {
	status := 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
	}))
	c := &restConn{baseURL: srv.URL, client: srv.Client()}
	for _, s := range []int{200, 404, 500} {
		status = s
		if err := c.Ping(context.Background()); err != nil {
			t.Fatalf("status %d must be alive: %v", s, err)
		}
	}
	srv.Close() // transport error now
	if err := c.Ping(context.Background()); err == nil {
		t.Fatal("dead server must fail ping")
	}
}

func TestPingHEADFallsBackToGETOn405(t *testing.T) {
	var sawGet bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" {
			w.WriteHeader(405)
			return
		}
		sawGet = true
	}))
	defer srv.Close()
	c := &restConn{baseURL: srv.URL, client: srv.Client()}
	if err := c.Ping(context.Background()); err != nil || !sawGet {
		t.Fatalf("405 HEAD must fall back to GET: err=%v sawGet=%v", err, sawGet)
	}
}

func TestManagerReconnectRefetchesSpec(t *testing.T) {
	// Serve spec A, connect via Manager with dialOpenAPI-backed dial, then
	// swap to spec B (one fewer operation), force markDown, and wait for
	// the regenerated tool list. Mirror TestManagerMarkDownTriggersRedial's
	// structure (gated dial / waitFor) — REUSE its helpers.
	// Assert: generation bumped, tool count changed from 5 to the new count.
}

func TestRenameToolsPassesRESTNamesThrough(t *testing.T) {
	tools, _ := generateTools("orders", []byte(testSpec), "http://x", nil, nil)
	if len(tools) == 0 {
		t.Fatal("no tools")
	}
	ht := make([]tool.Tool, len(tools))
	for i := range tools {
		ht[i] = tools[i]
	}
	renamed := renameTools(ht)
	for i := range renamed {
		if renamed[i].Name() != ht[i].Name() {
			t.Fatalf("REST tool renamed: %s -> %s", ht[i].Name(), renamed[i].Name())
		}
	}
}
```

(Fill in TestManagerReconnectRefetchesSpec against the REAL manager-test helpers — read manager_test.go first; the comment block is the contract. Imports: os, config.)

- [ ] **Step 2: Run, verify FAIL.**

- [ ] **Step 3: Implement** in restconn.go:

```go
// restConn is the connected form of an OpenAPI upstream. Stateless: tools
// carry their own client; Ping probes the API base URL.
type restConn struct {
	baseURL string
	client  *http.Client
	tools   []tool.Tool
}

func (c *restConn) Tools() []tool.Tool { return c.tools }

// Ping: HEAD base_url, GET fallback on 405. ANY HTTP response = alive (REST
// APIs have no standard health endpoint; a 404 still proves reachability).
// Only transport errors mark the upstream down.
func (c *restConn) Ping(ctx context.Context) error {
	for _, method := range []string{http.MethodHead, http.MethodGet} {
		req, err := http.NewRequestWithContext(ctx, method, c.baseURL, nil)
		if err != nil {
			return err
		}
		resp, err := c.client.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			return nil
		}
	}
	return nil // both answered (405 twice) — still an answer, still alive
}

func (c *restConn) Close() error { return nil }

// dialOpenAPI is the production dialer branch for openapi: upstreams. Fetches
// the spec (file or URL, with the configured headers), generates the tools,
// and returns a stateless conn. A fetch/parse failure is a dial error — the
// supervision loop retries with backoff, re-fetching each time (drift
// handling for free).
func dialOpenAPI(ctx context.Context, s config.GatewayServer) (upstreamConn, error) {
	var specBytes []byte
	var err error
	if strings.HasPrefix(s.OpenAPI, "http://") || strings.HasPrefix(s.OpenAPI, "https://") {
		specBytes, err = fetchSpec(ctx, s.OpenAPI, s.Headers)
	} else {
		specBytes, err = os.ReadFile(s.OpenAPI)
	}
	if err != nil {
		return nil, fmt.Errorf("openapi spec %s: %w", s.OpenAPI, err)
	}
	client := newRestClient()
	tools, err := generateTools(s.Name, specBytes, s.BaseURL, s.Operations, client)
	if err != nil {
		return nil, err
	}
	baseURL := s.BaseURL
	ht := make([]tool.Tool, len(tools))
	for i := range tools {
		tools[i].staticHeaders = s.Headers
		if baseURL == "" {
			baseURL = tools[i].baseURL // generateTools resolved spec servers[0]
		}
		ht[i] = tools[i]
	}
	if baseURL == "" {
		// zero tools AND no base_url: nothing to ping. Treat as dial error.
		return nil, fmt.Errorf("openapi: no base_url resolvable for %s", s.Name)
	}
	return &restConn{baseURL: baseURL, client: client, tools: ht}, nil
}

// fetchSpec GETs a spec URL with a bounded timeout and the upstream's
// configured headers (spec endpoints behind the same auth work).
func fetchSpec(ctx context.Context, url string, headers map[string]string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("spec fetch: HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20)) // 8 MiB spec cap
}
```

EDGE CASE in dialOpenAPI: when the filter yields zero tools but the spec had servers[], baseURL stays "" — handle by ALSO resolving baseURL from the doc when tools are empty. Simplest correct fix: make generateTools return `(tools []restTool, resolvedBase string, err error)` and use that. ADJUST Task 2's signature accordingly when you get here (Task 2's tests call generateTools — update call sites; this is the one permitted cross-task signature change, note it in the commit).

connect.go — branch the production dialer:

```go
// dialProduction routes each transport: openapi: → REST adapter,
// command:/url: → harness MCP.
func dialProduction(ctx context.Context, s config.GatewayServer) (upstreamConn, error) {
	if s.OpenAPI != "" {
		return dialOpenAPI(ctx, s)
	}
	return hmcp.Connect(ctx, hmcp.ServerConfig{
		Name:    s.Name,
		Command: s.Command,
		Args:    s.Args,
		Env:     s.Env,
		URL:     s.URL,
		Headers: s.Headers,
	})
}
```

Rename usage: `NewManager` currently sets `dial: dialHarness` — point it at `dialProduction` (keep dialHarness as the MCP-only helper it calls, or inline; keep names clear). Also check `transportOf` (manager.go) — add an "openapi" case for status/log output.

- [ ] **Step 4: Run, verify PASS** — `go test ./internal/gateway/ -count=1 -race`.

- [ ] **Step 5: Commit** — `git add internal/gateway/ && git commit -m "feat(gateway): dialOpenAPI + restConn — REST upstreams as third transport"`

---

### Task 5: through-serve e2e

**Files:**
- Create: `test/gateway_rest_e2e_test.go`

- [ ] **Step 1: Read sibling scaffolding** — test/gateway_e2e_test.go (open-mode federation pattern) and test/gateway_sandbox_e2e_test.go (identity-on pattern, tenant keys). REUSE; copy file-local helpers if needed. Ports: 8170/8171 (siblings use 8090-8163).

- [ ] **Step 2: Write the test** (`//go:build integration`):

TestGatewayRESTE2E:
1. httptest REST API + spec endpoint (reuse testSpec shape — inline a copy; test package differs).
2. runtimed with identity ON, tenants alpha (key A) + beta (key B); gateway server `orders` with `openapi:` pointing at the httptest spec URL, `base_url` at the API, `tenants: [alpha]`.
3. External MCP client with key A on /gateway/mcp: tools/list contains `orders__listOrders`; CallTool succeeds; envelope parses with status 200.
4. Same with key B: `orders__*` absent from tools/list; calling `orders__listOrders` → tool-not-found isError.
5. GET /metrics: `runtime_gateway_tool_calls_total{server="orders"` series present with outcome="ok".
6. `/gateway/status` (key A): upstream `orders` state "up".

Follow the sibling identity-table setup + cleanup pattern exactly (t.Cleanup restoring open mode).

- [ ] **Step 3: Run** — `go test -tags integration ./test/ -run TestGatewayRESTE2E -count=1 -v -timeout 300s` until PASS, then the whole integration suite `go test -tags integration ./test/ -count=1 -timeout 900s`.

- [ ] **Step 4: Commit** — `git add test/gateway_rest_e2e_test.go && git commit -m "test(e2e): REST upstream through the gateway with identity + metrics"`

---

### Task 6: examples/rest-demo (bundled demo API)

**Files:**
- Create: `examples/rest-demo/main.go`
- Create: `examples/rest-demo/openapi.yaml`
- Create: `examples/rest-demo/README.md`

- [ ] **Step 1: Write the demo service** — a single-file Go orders API (~100 lines): in-memory store seeded with 3 orders; endpoints GET /orders (list, optional ?status=), GET /orders/{id}, POST /orders (create), GET /openapi.yaml (serves its own spec file). Port 9000 (RUNTIME_DEMO_ADDR overrides). Use only stdlib. The OpenAPI spec file describes exactly those operations with operationIds listOrders/getOrder/createOrder, full schemas, and `servers: [{url: http://localhost:9000}]`.

- [ ] **Step 2: README.md** — how to run (`go run ./examples/rest-demo`), the runtime.yaml fragment to federate it:

```yaml
gateway:
  servers:
    - name: orders
      openapi: http://localhost:9000/openapi.yaml
```

and a curl probe + expected gateway tool names.

- [ ] **Step 3: Verify** — `go build ./... && go vet ./examples/rest-demo/`; boot it, curl /orders and /openapi.yaml; OPTIONALLY point a local runtimed at it as a smoke (full live proof happens at close-out).

- [ ] **Step 4: Commit** — `git add examples/rest-demo/ && git commit -m "feat(examples): rest-demo orders API with OpenAPI spec for gateway federation"`

---

### Task 7: docs — README + ROADMAP

**Files:**
- Modify: `README.md` (MCP Gateway section + features table + status + testing)
- Modify: `ROADMAP.md` (header current-state + §B1 third-milestone entry)

- [ ] **Step 1: README** — in the MCP Gateway section add a "REST/OpenAPI upstreams" subsection: the config fragment (openapi/base_url/operations/headers), what gets generated (naming, schema mapping one-liner), the response envelope shape, the security posture bullets (config headers inviolable, SSRF containment, 4xx-as-result), and limitations (JSON bodies only, shared credentials per upstream, OpenAPI 3.x only). Features table row mentions REST APIs. Testing section gains TestGatewayRESTE2E.

- [ ] **Step 2: ROADMAP** — header "Current state" gains Gateway M3; §B1 gains a "Third milestone DONE" entry in house style (dense what-shipped paragraph + remaining B1 work: dynamic registration, resources/prompts passthrough, OAuth2 upstream auth, per-tenant credentials, console panel, rate limits). Checkpoint date line updated.

- [ ] **Step 3: Validate + commit** — `go build ./... && go vet ./... && go test ./... -count=1`; `git add README.md ROADMAP.md && git commit -m "docs: README REST-upstream section + ROADMAP Gateway M3 entry"`

---

## Self-review (done at planning time)

- **Spec coverage:** §3 config → Task 1; §5 generation → Task 2; §6 execution → Task 3; §4 dial/conn/ping + reconnect-refetch + renameTools passthrough → Task 4; e2e → Task 5; live-proof demo API → Task 6 (live proof itself at close-out per repo convention; Open-Meteo + agent turn + search discovery happen there); docs → Task 7.
- **Known judgment calls baked in:** generateTools signature may grow a resolvedBase return in Task 4 (flagged inline as the one permitted cross-task change); Ping treats double-405 as alive; cookie params skipped silently (spec says skip-param-keep-operation).
- **Type consistency:** restTool fields referenced in Task 3 (`staticHeaders`, `client`, `method`, `specPath`, `baseURL`, `pathParams`, `queryParams`, `headerParams`, `hasBody`, `requiredFields`) all declared in Task 2's struct; `newRestClient` defined Task 3, used Task 4; `dialOpenAPI` defined Task 4, used in connect branch + tests.
- **Honest gaps:** TestManagerReconnectRefetchesSpec is specified as a contract comment because it must reuse manager_test.go's gated-dial helpers (same convention as the M1 metrics plan — implementer reads the fixtures first). The e2e is described by numbered assertions rather than full code for the same reason (sibling scaffolding reuse is mandatory).
