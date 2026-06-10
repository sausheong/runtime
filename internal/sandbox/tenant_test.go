package sandbox

import (
	"encoding/json"
	"testing"
)

func TestPopTenant(t *testing.T) {
	tenant, present, rest, err := popTenant(json.RawMessage(`{"__rt_tenant":"acme","x":1}`))
	if err != nil || tenant != "acme" {
		t.Fatalf("got %q, %v", tenant, err)
	}
	if !present {
		t.Fatal("present = false for an explicit tenant key")
	}
	var m map[string]any
	if err := json.Unmarshal(rest, &m); err != nil {
		t.Fatal(err)
	}
	if _, hasKey := m["__rt_tenant"]; hasKey {
		t.Fatal("__rt_tenant not removed from rest")
	}
	if m["x"] != float64(1) {
		t.Fatal("other args lost")
	}
}

func TestPopTenantEmptyMapsToDefault(t *testing.T) {
	for _, tc := range []struct {
		raw         string
		wantPresent bool
	}{
		{`{"__rt_tenant":""}`, true}, // gateway open mode: key injected, empty
		{`{}`, false},
		{``, false},
		{`null`, false},
	} {
		tenant, present, _, err := popTenant(json.RawMessage(tc.raw))
		if err != nil || tenant != "default" {
			t.Fatalf("popTenant(%q) = %q, %v; want default", tc.raw, tenant, err)
		}
		if present != tc.wantPresent {
			t.Fatalf("popTenant(%q) present = %v, want %v", tc.raw, present, tc.wantPresent)
		}
	}
}

func TestPopTenantBadJSON(t *testing.T) {
	if _, _, _, err := popTenant(json.RawMessage(`not json`)); err == nil {
		t.Fatal("want error")
	}
}
