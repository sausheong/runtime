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

func TestPopReserved(t *testing.T) {
	raw := json.RawMessage(`{"__rt_tenant":"acme","__rt_session":"s1","code":"x"}`)
	tenant, present, session, rest, err := popReserved(raw)
	if err != nil {
		t.Fatal(err)
	}
	if tenant != "acme" || !present || session != "s1" {
		t.Fatalf("got tenant=%q present=%v session=%q", tenant, present, session)
	}
	var m map[string]any
	json.Unmarshal(rest, &m)
	if _, ok := m["__rt_tenant"]; ok {
		t.Error("__rt_tenant leaked into rest")
	}
	if _, ok := m["__rt_session"]; ok {
		t.Error("__rt_session leaked into rest")
	}
	if m["code"] != "x" {
		t.Error("payload dropped")
	}
}

func TestPopReservedAbsentSessionOK(t *testing.T) {
	// tenant present, session absent → session "" and no error (tenant-scoped path).
	_, present, session, _, err := popReserved(json.RawMessage(`{"__rt_tenant":""}`))
	if err != nil {
		t.Fatal(err)
	}
	if !present || session != "" {
		t.Fatalf("present=%v session=%q, want true,''", present, session)
	}
}
