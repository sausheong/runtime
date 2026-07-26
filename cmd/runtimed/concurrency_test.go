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

func TestLocalAgentIdentityRejectsSharedRoleAcrossAgents(t *testing.T) {
	cfg := &config.Config{Agents: []config.AgentConfig{
		{ID: "a", Tenant: "alpha"},
		{ID: "b", Tenant: "beta"},
		{ID: "remote", Tenant: "gamma", URL: "https://remote.example"},
	}}
	if _, _, err := localAgentIdentity(cfg, false, false); err == nil {
		t.Fatal("multiple local-agent tenants accepted one database role")
	}
	cfg.Agents[1].Tenant = "alpha"
	if _, _, err := localAgentIdentity(cfg, false, false); err == nil {
		t.Fatal("multiple same-tenant agents accepted one restricted database role")
	}
	tenant, agentID, err := localAgentIdentity(cfg, true, false)
	if err != nil || tenant != "*" || agentID != "*" {
		t.Fatalf("test-only shared-role result=%q/%q err=%v", tenant, agentID, err)
	}
}

func TestLocalAgentIdentitySkipsAttachOnlyRemotes(t *testing.T) {
	cfg := &config.Config{Agents: []config.AgentConfig{
		{ID: "a", Tenant: "acme", URL: "http://agent-a:8081"},
		{ID: "b", Tenant: "acme", URL: "http://agent-b:8081"},
	}}
	tenant, agentID, err := localAgentIdentity(cfg, false, false)
	if err != nil || tenant != "" || agentID != "" {
		t.Fatalf("remote-only registry should not provision an agent role: identity=%q/%q err=%v", tenant, agentID, err)
	}
	tenant, agentID, err = localAgentIdentity(cfg, true, false)
	if err != nil || tenant != "" || agentID != "" {
		t.Fatalf("test-only remote registry should not provision an agent role: identity=%q/%q err=%v", tenant, agentID, err)
	}

	cfg.Agents[1].Tenant = "other"
	tenant, agentID, err = localAgentIdentity(cfg, false, false)
	if err != nil || tenant != "" || agentID != "" {
		t.Fatalf("multi-tenant remotes should not affect local role provisioning: identity=%q/%q err=%v", tenant, agentID, err)
	}
}

func TestLocalAgentIdentityIgnoresAttachOnlyAgents(t *testing.T) {
	cfg := &config.Config{Agents: []config.AgentConfig{
		{ID: "local", Tenant: "alpha"},
		{ID: "remote-a", Tenant: "beta", URL: "https://remote-a.example"},
		{ID: "remote-b", Tenant: "gamma", URL: "https://remote-b.example"},
	}}
	tenant, agentID, err := localAgentIdentity(cfg, false, false)
	if err != nil || tenant != "alpha" || agentID != "local" {
		t.Fatalf("identity=%q/%q err=%v, want alpha/local", tenant, agentID, err)
	}
}

func TestLocalAgentIdentityCanProvisionOneRegistrationManagedRemote(t *testing.T) {
	cfg := &config.Config{Agents: []config.AgentConfig{
		{ID: "registered", Tenant: "alpha", URL: "https://registered.example"},
	}}
	tenant, agentID, err := localAgentIdentity(cfg, false, true)
	if err != nil || tenant != "alpha" || agentID != "registered" {
		t.Fatalf("identity=%q/%q err=%v, want alpha/registered", tenant, agentID, err)
	}
}

func TestPublishedGCPRemoteProfileDoesNotProvisionAgentRole(t *testing.T) {
	for _, key := range []string{
		"NUTRITION_GO_TOKEN",
		"NUTRITION_OPENAI_TOKEN",
		"HELLO_CLAUDE_TOKEN",
		"FOOD_LABEL_TOKEN",
	} {
		t.Setenv(key, "test-token")
	}
	for _, key := range []string{
		"NUTRITION_GO_GENERATION",
		"NUTRITION_OPENAI_GENERATION",
		"HELLO_CLAUDE_GENERATION",
		"FOOD_LABEL_GENERATION",
	} {
		t.Setenv(key, "11111111-1111-4111-8111-111111111111")
	}
	cfg, err := config.Load("../../deploy/gcp/control-plane/runtime.remote.yaml")
	if err != nil {
		t.Fatalf("load published GCP profile: %v", err)
	}
	tenant, agentID, err := localAgentIdentity(cfg, false, false)
	if err != nil || tenant != "" || agentID != "" {
		t.Fatalf("published remote-only profile attempted restricted-role provisioning: identity=%q/%q err=%v", tenant, agentID, err)
	}
}
