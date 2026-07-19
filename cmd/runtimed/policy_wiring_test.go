package main

import (
	"errors"
	"testing"
)

func TestBuildPolicyEngine(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	validCedar := []byte(`forbid (principal, action, resource) when { resource.server == "sandbox" };`)
	readOK := func(string) ([]byte, error) { return validCedar, nil }
	readErr := func(string) ([]byte, error) { return nil, errors.New("no such file") }
	readBad := func(string) ([]byte, error) { return []byte("this is not cedar"), nil }

	t.Run("neither var: off", func(t *testing.T) {
		e, err := buildPolicyEngine(env(nil), readOK, nil)
		if err != nil || e != nil {
			t.Fatalf("want (nil,nil), got (%v,%v)", e, err)
		}
	})
	t.Run("ENABLED=1: empty platform, engine on", func(t *testing.T) {
		e, err := buildPolicyEngine(env(map[string]string{"RUNTIME_POLICY_ENABLED": "1"}), readErr, nil)
		if err != nil || e == nil {
			t.Fatalf("want engine, got (%v,%v)", e, err)
		}
	})
	t.Run("FILE readable+parseable: engine on", func(t *testing.T) {
		e, err := buildPolicyEngine(env(map[string]string{"RUNTIME_POLICY_FILE": "p.cedar"}), readOK, nil)
		if err != nil || e == nil {
			t.Fatalf("want engine, got (%v,%v)", e, err)
		}
	})
	t.Run("FILE unreadable: boot error", func(t *testing.T) {
		if _, err := buildPolicyEngine(env(map[string]string{"RUNTIME_POLICY_FILE": "p.cedar"}), readErr, nil); err == nil {
			t.Fatal("unreadable file must be a boot error")
		}
	})
	t.Run("FILE unparseable: boot error", func(t *testing.T) {
		if _, err := buildPolicyEngine(env(map[string]string{"RUNTIME_POLICY_FILE": "p.cedar"}), readBad, nil); err == nil {
			t.Fatal("unparseable platform file must be a boot error")
		}
	})
}
