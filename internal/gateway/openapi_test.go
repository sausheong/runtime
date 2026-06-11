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

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
