package main

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/sausheong/runtime/internal/config"
)

func TestLimitHTTPConcurrencyBoundsRequests(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	h := limitHTTPConcurrency(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		once.Do(func() { close(started) })
		<-release
	}), 1, 1, nil)
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/agents/a/sessions", nil))
		close(done)
	}()
	<-started
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/agents/a/sessions", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503", rec.Code)
	}
	close(release)
	<-done
}

func TestLimitHTTPConcurrencyBoundsStreamsSeparately(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	h := limitHTTPConcurrency(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(started)
		<-release
	}), 2, 1, nil)
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/agents/a/sessions/s/stream", nil))
		close(done)
	}()
	<-started
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/agents/a/sessions/other/stream", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503", rec.Code)
	}
	close(release)
	<-done
}

func TestLocalAgentTenantRejectsSharedRoleAcrossTenants(t *testing.T) {
	cfg := &config.Config{Agents: []config.AgentConfig{
		{ID: "a", Tenant: "alpha"},
		{ID: "b", Tenant: "beta"},
		{ID: "remote", Tenant: "gamma", URL: "https://remote.example"},
	}}
	if _, err := localAgentTenant(cfg, false); err == nil {
		t.Fatal("multiple local-agent tenants accepted one database role")
	}
	cfg.Agents[1].Tenant = "alpha"
	cfg.Agents[2].Tenant = "alpha"
	if _, err := localAgentTenant(cfg, false); err == nil {
		t.Fatal("multiple same-tenant agents accepted one restricted database role")
	}
	tenant, err := localAgentTenant(cfg, true)
	if err != nil || tenant != "*" {
		t.Fatalf("test-only shared-role result=%q err=%v", tenant, err)
	}
}

func TestLocalAgentTenantUsesRemoteOnlyTenant(t *testing.T) {
	cfg := &config.Config{Agents: []config.AgentConfig{
		{ID: "a", Tenant: "acme", URL: "http://agent-a:8081"},
		{ID: "b", Tenant: "acme", URL: "http://agent-b:8081"},
	}}
	if _, err := localAgentTenant(cfg, false); err == nil {
		t.Fatal("multiple remote agents accepted one restricted database role")
	}
	tenant, err := localAgentTenant(cfg, true)
	if err != nil || tenant != "*" {
		t.Fatalf("test-only remote shared-role result=%q err=%v", tenant, err)
	}

	cfg.Agents[1].Tenant = "other"
	if _, err := localAgentTenant(cfg, false); err == nil {
		t.Fatal("remote agents from multiple tenants accepted one database role")
	}
}
