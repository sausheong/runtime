package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "runtime.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestEnrichValidation(t *testing.T) {
	base := "agents:\n  - {id: a1, name: A1, model: test/scripted, listen_addr: 127.0.0.1:9401}\n" +
		"gateway:\n  servers:\n"

	// enrich on a non-openapi upstream ⇒ error.
	_, err := Load(writeCfg(t, base+
		"    - {name: sbx, url: 'http://x', enrich: {tenant: X-Runtime-Tenant}}\n"))
	if err == nil {
		t.Error("enrich on url upstream must fail (openapi-only)")
	}

	// unknown claim ⇒ error.
	_, err = Load(writeCfg(t, base+
		"    - {name: o, openapi: 'http://x/s.yaml', base_url: 'http://x', enrich: {clearance: X-Runtime-Clr}}\n"))
	if err == nil {
		t.Error("unknown enrich claim must fail")
	}

	// collision with cred_header ⇒ error.
	_, err = Load(writeCfg(t, base+
		"    - {name: o, openapi: 'http://x/s.yaml', base_url: 'http://x', cred_secret: S, cred_header: X-Runtime-Tenant, enrich: {tenant: X-Runtime-Tenant}}\n"))
	if err == nil {
		t.Error("enrich header colliding with cred_header must fail")
	}

	// valid openapi enrich ⇒ ok.
	cfg, err := Load(writeCfg(t, base+
		"    - {name: o, openapi: 'http://x/s.yaml', base_url: 'http://x', enrich: {tenant: X-Runtime-Tenant, subject: X-Runtime-User}}\n"))
	if err != nil {
		t.Fatalf("valid enrich must load: %v", err)
	}
	if got := cfg.Gateway.Servers[0].Enrich["tenant"]; got != "X-Runtime-Tenant" {
		t.Fatalf("enrich not parsed: %v", cfg.Gateway.Servers[0].Enrich)
	}
}
