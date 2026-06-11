package main

import (
	"strings"
	"testing"
)

// TestSpecEmbedsOperations is a cheap sanity check that the embedded OpenAPI
// spec carries the three operations the gateway demo depends on. Full parsing
// is covered by internal/gateway tests; the example stays stdlib-only.
func TestSpecEmbedsOperations(t *testing.T) {
	spec := string(openapiSpec)
	if !strings.Contains(spec, "openapi: 3.0.3") {
		t.Fatalf("embedded spec missing OpenAPI 3.0.3 version marker")
	}
	for _, op := range []string{"listOrders", "getOrder", "createOrder"} {
		if !strings.Contains(spec, "operationId: "+op) {
			t.Errorf("embedded spec missing operationId %q", op)
		}
	}
	if !strings.Contains(spec, "$ref: \"#/components/schemas/Order\"") {
		t.Errorf("embedded spec missing Order schema $ref (gateway inlining demo)")
	}
}
