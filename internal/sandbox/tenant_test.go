package sandbox

import (
	"encoding/json"
	"testing"
)

func TestPopTenant(t *testing.T) {
	tenant, rest, err := popTenant(json.RawMessage(`{"__rt_tenant":"acme","x":1}`))
	if err != nil || tenant != "acme" {
		t.Fatalf("got %q, %v", tenant, err)
	}
	var m map[string]any
	if err := json.Unmarshal(rest, &m); err != nil {
		t.Fatal(err)
	}
	if _, present := m["__rt_tenant"]; present {
		t.Fatal("__rt_tenant not removed from rest")
	}
	if m["x"] != float64(1) {
		t.Fatal("other args lost")
	}
}

func TestPopTenantEmptyMapsToDefault(t *testing.T) {
	for _, raw := range []string{`{"__rt_tenant":""}`, `{}`, ``, `null`} {
		tenant, _, err := popTenant(json.RawMessage(raw))
		if err != nil || tenant != "default" {
			t.Fatalf("popTenant(%q) = %q, %v; want default", raw, tenant, err)
		}
	}
}

func TestPopTenantBadJSON(t *testing.T) {
	if _, _, err := popTenant(json.RawMessage(`not json`)); err == nil {
		t.Fatal("want error")
	}
}
