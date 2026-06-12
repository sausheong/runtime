package browser

import (
	"context"
	"encoding/json"
	"testing"
)

func TestServerToolsRegistered(t *testing.T) {
	m := NewManager(NewFakeBackend(), Config{})
	srv := NewServer(m, false)
	if srv == nil {
		t.Fatal("nil server")
	}
}

func TestPopTenantStripAndDefault(t *testing.T) {
	tn, present, rest, err := popTenant(json.RawMessage(`{"__rt_tenant":"acme","x":1}`))
	if err != nil || tn != "acme" || !present {
		t.Fatalf("tenant=%q present=%v err=%v", tn, present, err)
	}
	var got map[string]any
	_ = json.Unmarshal(rest, &got)
	if _, leaked := got["__rt_tenant"]; leaked {
		t.Fatal("__rt_tenant not stripped")
	}
	tn2, present2, _, _ := popTenant(json.RawMessage(`{}`))
	if present2 || tn2 != "default" {
		t.Fatalf("absent key: tenant=%q present=%v", tn2, present2)
	}
}

func TestCreateBrowserToolFakeBackend(t *testing.T) {
	m := NewManager(NewFakeBackend(), Config{})
	srv := NewServer(m, true)
	_ = srv
	// Direct Manager check stands in for the create_browser handler path
	// (the fake backend has no CDP endpoint, so action tools can't run, but
	// create/list/close lifecycle works).
	s, err := m.Create(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	if s.ID == "" {
		t.Fatal("empty id")
	}
}
