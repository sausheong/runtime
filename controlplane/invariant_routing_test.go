package controlplane

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/sausheong/runtime/internal/config"
	"github.com/sausheong/runtime/internal/identity"
	"github.com/sausheong/runtime/internal/store"
)

func TestAPIReauthorizesExactSelectedAgentSnapshot(t *testing.T) {
	var replacementHits atomic.Int32
	replacement := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		replacementHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer replacement.Close()

	reg := NewRegistry(&config.Config{Agents: []config.AgentConfig{{
		ID: "shared", Name: "Shared", Model: "m", Tenant: "alpha",
		ListenAddr: "127.0.0.1:1",
	}}}, "", "")

	// This is the state after middleware authorized alpha but before the API
	// selected its forwarding target: the same ID now belongs to beta.
	reg.AddRemote(
		AgentInfo{ID: "shared", Name: "Replacement", Model: "m", Tenant: "beta"},
		AgentProcess{
			AgentID: "shared", Tenant: "beta", BaseURL: replacement.URL,
			RegistrationGeneration: "replacement-generation",
		},
		true,
	)

	req := httptest.NewRequest(http.MethodPost, "/agents/shared/sessions", nil)
	req = req.WithContext(WithPrincipal(req.Context(), identity.Principal{
		Subject: "alice", TenantID: "alpha", Role: identity.RoleOperator,
	}))
	rec := httptest.NewRecorder()
	NewAPI(reg, nil, store.NewMemStore(), false).ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 for stale tenant authorization", rec.Code)
	}
	if replacementHits.Load() != 0 {
		t.Fatal("stale authorization reached the replacement tenant")
	}
}

func TestPickReplicaRejectsStaleAndLegacyAgentGeneration(t *testing.T) {
	reg := NewRegistry(&config.Config{Agents: []config.AgentConfig{{
		ID: "shared", Name: "Shared", Model: "m", Tenant: "alpha",
		ListenAddr: "127.0.0.1:1",
	}}}, "", "")
	current, ok := reg.Get("shared")
	if !ok {
		t.Fatal("missing configured agent")
	}
	st := store.NewMemStore()

	for _, tc := range []struct {
		name       string
		generation string
	}{
		{name: "stale replacement", generation: "old-generation-value"},
		{name: "legacy unbound", generation: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sid, err := st.CreateSessionForIdentity(
				context.Background(), "alpha", "shared", tc.generation, 0)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodGet, "/sessions/"+sid, nil)
			if _, err := pickReplica(req, reg, st, "shared"); err == nil {
				t.Fatalf("generation %q routed to current %q", tc.generation, current.RegistrationGeneration)
			}
		})
	}

	sid, err := st.CreateSessionForIdentity(
		context.Background(), "alpha", "shared", current.RegistrationGeneration, 0)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/sessions/"+sid, nil)
	if got, err := pickReplica(req, reg, st, "shared"); err != nil {
		t.Fatalf("exact generation rejected: %v", err)
	} else if got.RegistrationGeneration != current.RegistrationGeneration {
		t.Fatalf("selected generation=%q, want %q", got.RegistrationGeneration, current.RegistrationGeneration)
	}
}
