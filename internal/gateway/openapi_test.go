package gateway

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
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
	tools, _, err := generateTools("orders", []byte(testSpec), baseURL, operations, nil)
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
	if _, _, err := generateTools("orders", []byte(specNoServers), "", nil, nil); err == nil {
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

func genSpec(t *testing.T, spec string) []restTool {
	t.Helper()
	tools, _, err := generateTools("srv", []byte(spec), "http://up:1", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return tools
}

// FIX 1: op-level parameters OVERRIDE item-level by (name, in) — same param at
// both levels appears once, op-level required:false undoes item-level
// required:true; an item-only param is inherited.
func TestParamOverrideMerge(t *testing.T) {
	const spec = `
openapi: 3.0.3
info: {title: T, version: "1.0"}
paths:
  /things/{id}:
    parameters:
      - {name: id, in: path, required: true, schema: {type: string}}
      - {name: verbose, in: query, required: true, schema: {type: boolean}}
    get:
      operationId: getThing
      parameters:
        - {name: verbose, in: query, required: false, schema: {type: boolean}}
      responses: {"200": {description: ok}}
`
	ts := genSpec(t, spec)
	if len(ts) != 1 {
		t.Fatalf("want 1 tool, got %v", toolNames(ts))
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(ts[0].Parameters(), &schema); err != nil {
		t.Fatal(err)
	}
	if _, ok := schema.Properties["verbose"]; !ok {
		t.Fatalf("verbose property missing: %v", schema.Properties)
	}
	if contains(schema.Required, "verbose") {
		t.Fatalf("op-level required:false must override item-level required:true: %v", schema.Required)
	}
	// Item-only param inherited; required exactly once (no duplicate entries).
	n := 0
	for _, r := range schema.Required {
		if r == "id" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("item-level id must be inherited exactly once in required, got %d: %v", n, schema.Required)
	}
}

// FIX 2: an operationId that sanitizes to nothing falls back to the
// method_path slug; if no name can be produced at all the op is skipped.
func TestToolNameUnsanitizableFallback(t *testing.T) {
	const spec = `
openapi: 3.0.3
info: {title: T, version: "1.0"}
paths:
  /orders:
    get:
      operationId: "!!!"
      responses: {"200": {description: ok}}
`
	ts := genSpec(t, spec)
	if len(ts) != 1 || ts[0].Name() != "srv__get_orders" {
		t.Fatalf("want fallback srv__get_orders, got %v", toolNames(ts))
	}
	// No real spec can reach this (method is always alphanumeric), but the
	// guard must hold: nothing sanitizable anywhere ⇒ empty ⇒ caller skips.
	if got := toolNameFor("!!!", "", ""); got != "" {
		t.Fatalf("fully unsanitizable name must be empty, got %q", got)
	}
}

// Two operations that sanitize to the same tool name keep only the first in
// deterministic (sorted-path, fixed-method-order) iteration; the later one is
// skipped with a warning and every other operation is unaffected.
func TestDuplicateSanitizedNamesSkipLater(t *testing.T) {
	// Sorted paths: /orders < /other. GET /orders has no operationId and
	// slugs to get_orders; POST /other declares operationId get_orders and
	// collides, so it must be skipped. GET /other (listOther) survives.
	const spec = `
openapi: 3.0.3
info: {title: T, version: "1.0"}
paths:
  /orders:
    get:
      responses: {"200": {description: ok}}
  /other:
    get:
      operationId: listOther
      responses: {"200": {description: ok}}
    post:
      operationId: get_orders
      responses: {"200": {description: ok}}
`
	ts := genSpec(t, spec)
	if len(ts) != 2 {
		t.Fatalf("want 2 tools (collision skipped), got %v", toolNames(ts))
	}
	var dupes []restTool
	for _, tt := range ts {
		if tt.Name() == "srv__get_orders" {
			dupes = append(dupes, tt)
		}
	}
	if len(dupes) != 1 {
		t.Fatalf("want exactly one srv__get_orders, got %d in %v", len(dupes), toolNames(ts))
	}
	// The survivor must be the sorted-iteration-first op: GET /orders.
	if dupes[0].method != "GET" || dupes[0].specPath != "/orders" {
		t.Fatalf("wrong collision winner: %s %s", dupes[0].method, dupes[0].specPath)
	}
	if !strings.Contains(strings.Join(toolNames(ts), ","), "srv__listOther") {
		t.Fatalf("sibling op lost: %v", toolNames(ts))
	}
}

// FIX 3: only a GENUINE cycle (ancestor-path repetition, e.g. Node.children →
// Node) skips an operation; sibling operations survive.
func TestCyclicRefOperationSkipped(t *testing.T) {
	const spec = `
openapi: 3.0.3
info: {title: T, version: "1.0"}
components:
  schemas:
    Node:
      type: object
      properties:
        name: {type: string}
        children:
          type: array
          items: {$ref: '#/components/schemas/Node'}
paths:
  /nodes:
    post:
      operationId: createNode
      requestBody:
        required: true
        content:
          application/json:
            schema: {$ref: '#/components/schemas/Node'}
      responses: {"201": {description: ok}}
  /plain:
    get:
      operationId: listPlain
      responses: {"200": {description: ok}}
`
	ts := genSpec(t, spec)
	names := strings.Join(toolNames(ts), ",")
	if strings.Contains(names, "createNode") {
		t.Fatalf("cyclic-ref op must be skipped: %s", names)
	}
	if !strings.Contains(names, "listPlain") {
		t.Fatalf("sibling op must survive: %s", names)
	}
}

// findTool returns the tool with the given full name, fatal if absent.
func findTool(t *testing.T, ts []restTool, name string) restTool {
	t.Helper()
	for _, tt := range ts {
		if tt.Name() == name {
			return tt
		}
	}
	t.Fatalf("tool %s missing: %v", name, toolNames(ts))
	return restTool{}
}

// REF FIX 1: a non-cyclic nested component ref (the dominant real-world idiom)
// must NOT be skipped — it is fully inlined, with no "$ref" keys emitted.
func TestNestedRefInlined(t *testing.T) {
	const spec = `
openapi: 3.0.3
info: {title: T, version: "1.0"}
components:
  schemas:
    Customer:
      type: object
      properties:
        name: {type: string}
paths:
  /orders:
    post:
      operationId: createOrder
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              properties:
                item: {type: string}
                customer: {$ref: '#/components/schemas/Customer'}
      responses: {"201": {description: ok}}
`
	ts := genSpec(t, spec)
	tool := findTool(t, ts, "srv__createOrder")
	s := string(tool.Parameters())
	if strings.Contains(s, `"$ref"`) {
		t.Fatalf("nested component ref must be inlined, schema still has $ref: %s", s)
	}
	if !strings.Contains(s, `"name"`) {
		t.Fatalf("Customer.name must be inlined into the body schema: %s", s)
	}
}

// REF FIX 2: the SAME component referenced twice (sibling reuse) is not a
// cycle — it is inlined at both sites.
func TestSiblingRefReuseNotCyclic(t *testing.T) {
	const spec = `
openapi: 3.0.3
info: {title: T, version: "1.0"}
components:
  schemas:
    Customer:
      type: object
      properties:
        name: {type: string}
paths:
  /transfers:
    post:
      operationId: createTransfer
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              properties:
                sender: {$ref: '#/components/schemas/Customer'}
                receiver: {$ref: '#/components/schemas/Customer'}
      responses: {"201": {description: ok}}
`
	ts := genSpec(t, spec)
	tool := findTool(t, ts, "srv__createTransfer")
	var schema struct {
		Properties map[string]struct {
			Properties map[string]struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"properties"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(tool.Parameters(), &schema); err != nil {
		t.Fatal(err)
	}
	body, ok := schema.Properties["body"]
	if !ok {
		t.Fatalf("body property missing: %s", tool.Parameters())
	}
	for _, side := range []string{"sender", "receiver"} {
		p, ok := body.Properties[side]
		if !ok {
			t.Fatalf("%s missing: %s", side, tool.Parameters())
		}
		if _, ok := p.Properties["name"]; !ok {
			t.Fatalf("%s.name not inlined: %s", side, tool.Parameters())
		}
	}
	if strings.Contains(string(tool.Parameters()), `"$ref"`) {
		t.Fatalf("sibling reuse must inline, not emit $ref: %s", tool.Parameters())
	}
}

// REF FIX 3: a property literally NAMED "$ref" (a string field) is data, not
// a reference — the op survives and the property is present.
func TestPropertyNamedDollarRefSurvives(t *testing.T) {
	const spec = `
openapi: 3.0.3
info: {title: T, version: "1.0"}
paths:
  /docs:
    post:
      operationId: createDoc
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              properties:
                $ref: {type: string, description: a literal property named dollar-ref}
      responses: {"201": {description: ok}}
`
	ts := genSpec(t, spec)
	tool := findTool(t, ts, "srv__createDoc")
	var schema struct {
		Properties map[string]struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(tool.Parameters(), &schema); err != nil {
		t.Fatal(err)
	}
	if _, ok := schema.Properties["body"].Properties["$ref"]; !ok {
		t.Fatalf("literal $ref property must be present: %s", tool.Parameters())
	}
}

// REF FIX 4: petstore-style sanity check — object ref (category) plus array of
// refs (tags) plus enum/format scalars: tools generate, schemas are ref-free.
func TestPetstoreShapeRefFree(t *testing.T) {
	const spec = `
openapi: 3.0.3
info: {title: Petstore, version: "1.0"}
components:
  schemas:
    Category:
      type: object
      properties:
        id: {type: integer, format: int64}
        name: {type: string}
    Tag:
      type: object
      properties:
        id: {type: integer, format: int64}
        name: {type: string}
    Pet:
      type: object
      required: [name]
      properties:
        id: {type: integer, format: int64}
        name: {type: string}
        category: {$ref: '#/components/schemas/Category'}
        tags:
          type: array
          items: {$ref: '#/components/schemas/Tag'}
        status: {type: string, enum: [available, pending, sold]}
paths:
  /pet:
    post:
      operationId: addPet
      requestBody:
        required: true
        content:
          application/json:
            schema: {$ref: '#/components/schemas/Pet'}
      responses: {"200": {description: ok}}
  /pet/{petId}:
    get:
      operationId: getPetById
      parameters:
        - {name: petId, in: path, required: true, schema: {type: integer, format: int64}}
      responses: {"200": {description: ok}}
`
	ts := genSpec(t, spec)
	if len(ts) != 2 {
		t.Fatalf("petstore-style spec must generate both tools, got %v", toolNames(ts))
	}
	addPet := findTool(t, ts, "srv__addPet")
	s := string(addPet.Parameters())
	if strings.Contains(s, `"$ref"`) {
		t.Fatalf("petstore schema must be ref-free: %s", s)
	}
	for _, want := range []string{`"category"`, `"tags"`, `"available"`, `"int64"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("inlined petstore schema missing %s: %s", want, s)
		}
	}
}

// FIX 4: invalid example values (endemic in third-party specs) must not kill
// the upstream — examples/defaults validation is disabled.
func TestInvalidExampleStillGenerates(t *testing.T) {
	const spec = `
openapi: 3.0.3
info: {title: T, version: "1.0"}
paths:
  /orders:
    get:
      operationId: listOrders
      parameters:
        - name: limit
          in: query
          schema: {type: integer, example: "not-an-integer"}
      responses: {"200": {description: ok}}
`
	ts := genSpec(t, spec)
	if len(ts) != 1 || ts[0].Name() != "srv__listOrders" {
		t.Fatalf("invalid example must not block generation: %v", toolNames(ts))
	}
}

// FIX 5: a REQUIRED non-JSON body skips the op; an OPTIONAL non-JSON body
// just drops the body property (op usable bodyless).
func TestNonJSONBody(t *testing.T) {
	const spec = `
openapi: 3.0.3
info: {title: T, version: "1.0"}
paths:
  /upload:
    post:
      operationId: uploadFile
      requestBody:
        required: true
        content:
          application/octet-stream:
            schema: {type: string, format: binary}
      responses: {"201": {description: ok}}
  /ping:
    post:
      operationId: pingIt
      requestBody:
        content:
          text/plain:
            schema: {type: string}
      responses: {"200": {description: ok}}
`
	ts := genSpec(t, spec)
	names := strings.Join(toolNames(ts), ",")
	if strings.Contains(names, "uploadFile") {
		t.Fatalf("required non-JSON body op must be skipped: %s", names)
	}
	var ping *restTool
	for i := range ts {
		if ts[i].Name() == "srv__pingIt" {
			ping = &ts[i]
		}
	}
	if ping == nil {
		t.Fatalf("optional non-JSON body op must be kept: %s", names)
	}
	if ping.hasBody {
		t.Fatal("optional non-JSON body must be dropped (hasBody=false)")
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(ping.Parameters(), &schema); err != nil {
		t.Fatal(err)
	}
	if _, ok := schema.Properties["body"]; ok {
		t.Fatal("optional non-JSON body must not produce a body property")
	}
}

// FIX 6: prose-less descriptions have no dangling " — "; truncation never
// leaves an invalid UTF-8 tail.
func TestDescriptionTrimAndUTF8(t *testing.T) {
	for _, tt := range gen(t, "", nil) {
		if tt.Name() == "srv__delete_orders_id" || tt.Name() == "orders__delete_orders_id" {
			if got := tt.Description(); got != "DELETE /orders/{id}" {
				t.Fatalf("prose-less description must be name-only: %q", got)
			}
		}
	}
	// Multibyte rune straddling the 1024-byte cut must be repaired.
	prose := strings.Repeat("a", 1008) + strings.Repeat("€", 8) // € = 3 bytes
	spec := fmt.Sprintf(`
openapi: 3.0.3
info: {title: T, version: "1.0"}
paths:
  /long:
    get:
      operationId: longDesc
      summary: %q
      responses: {"200": {description: ok}}
`, prose)
	ts := genSpec(t, spec)
	d := ts[0].Description()
	if len(d) > maxDescriptionLen {
		t.Fatalf("description over limit: %d bytes", len(d))
	}
	if !utf8.ValidString(d) {
		t.Fatalf("truncated description is invalid UTF-8: %q", d[len(d)-8:])
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
