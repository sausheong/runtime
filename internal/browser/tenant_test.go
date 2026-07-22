package browser

import (
	"encoding/json"
	"testing"
)

func TestPopReserved(t *testing.T) {
	raw := json.RawMessage(`{"__rt_tenant":"acme","__rt_session":"s1","browser_id":"x"}`)
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
	if m["browser_id"] != "x" {
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

func TestPopReservedEmptyTenantMapsToDefault(t *testing.T) {
	for _, tc := range []struct {
		raw         string
		wantPresent bool
	}{
		{`{"__rt_tenant":""}`, true}, // gateway open mode: key injected, empty
		{`{}`, false},
		{``, false},
		{`null`, false},
	} {
		tenant, present, _, _, err := popReserved(json.RawMessage(tc.raw))
		if err != nil || tenant != "default" {
			t.Fatalf("popReserved(%q) = %q, %v; want default", tc.raw, tenant, err)
		}
		if present != tc.wantPresent {
			t.Fatalf("popReserved(%q) present = %v, want %v", tc.raw, present, tc.wantPresent)
		}
	}
}

func TestPopReservedBadJSON(t *testing.T) {
	if _, _, _, _, err := popReserved(json.RawMessage(`not json`)); err == nil {
		t.Fatal("want error")
	}
}
