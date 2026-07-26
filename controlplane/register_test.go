package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sausheong/runtime/internal/config"
	"github.com/sausheong/runtime/internal/identity"
)

// fakeRegTokens implements RegTokenVerifier for hermetic tests.
type fakeRegTokens struct {
	credential identity.RegTokenCredential
	err        error
}

func (f fakeRegTokens) ActiveRegTokenByID(_ context.Context, id string) (identity.RegTokenCredential, error) {
	if f.err != nil {
		return identity.RegTokenCredential{}, f.err
	}
	return f.credential, nil
}

func regCredential(agentID, tenant, generation, hash string) fakeRegTokens {
	return fakeRegTokens{credential: identity.RegTokenCredential{
		AgentID: agentID, TenantID: tenant, AgentGeneration: generation, Hash: hash,
	}}
}

// regFakeBroker returns a fixed secret set (SecretBroker). Named distinctly from
// the spawn-path fakeBroker in proxy_test.go (which is keyed by tenant).
type regFakeBroker struct {
	secrets map[string]string
	err     error
}

func (f regFakeBroker) SecretsFor(_ context.Context, _ string) (map[string]string, error) {
	return f.secrets, f.err
}

func regTestRegistry(t *testing.T, broker SecretBroker) *Registry {
	t.Helper()
	cfg := &config.Config{Agents: []config.AgentConfig{
		{ID: "support", Name: "Support", Model: "test/scripted", Tenant: "acme",
			URL: "http://127.0.0.1:900{i}", Replicas: 2,
			RegistrationGeneration: "11111111-1111-4111-8111-111111111111"},
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg validate: %v", err)
	}
	reg := NewRegistry(cfg, "", "dsn://x")
	if broker != nil {
		reg.SetBroker(broker)
	}
	return reg
}

func post(t *testing.T, h http.Handler, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/register", bytes.NewReader(buf))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestRegister_Success(t *testing.T) {
	mk, _ := identity.MintServiceKey()
	broker := regFakeBroker{secrets: map[string]string{"OPENAI_API_KEY": "sk-xyz"}}
	mux := http.NewServeMux()
	reg := regTestRegistry(t, broker)
	_, generation, _ := reg.RegistrationIdentity("support")
	store := regCredential("support", "acme", generation, mk.Hash)
	reg.SetReachable("support", 1, false)
	RegisterHandshake(mux, store, reg)

	rec := post(t, mux, mk.Plaintext, RegisterRequest{Ordinal: 1})
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body)
	}
	var resp RegisterResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Env["RUNTIME_AGENT_ID"] != "support" ||
		resp.Env["RUNTIME_AGENT_TENANT"] != "acme" ||
		resp.Env["RUNTIME_AGENT_GENERATION"] != generation ||
		resp.Env["RUNTIME_DBOS_SCHEMA"] == "" ||
		resp.Env["RUNTIME_AGENT_REPLICA"] != "1" ||
		resp.Env["DBOS__VMID"] != "" || // remote pool: DBOSVMID empty (remote owns its id)
		resp.Env["OPENAI_API_KEY"] != "sk-xyz" {
		t.Fatalf("unexpected env: %+v", resp.Env)
	}
	if !reg.reachableOrUnknown("support", 1) {
		t.Fatal("successful registration left stale unreachable state in place")
	}
}

func TestRegistrationDisabledWithoutExplicitFileGeneration(t *testing.T) {
	cfg := &config.Config{Agents: []config.AgentConfig{{
		ID: "local", Name: "Local", Model: "m", Tenant: "acme",
		ListenAddr: "127.0.0.1:9000",
	}}}
	reg := NewRegistry(cfg, "", "")
	if tenant, generation, ok := reg.RegistrationIdentity("local"); ok {
		t.Fatalf("registration unexpectedly enabled: %q/%q", tenant, generation)
	}
}

func TestRegister_MissingBearer(t *testing.T) {
	mux := http.NewServeMux()
	RegisterHandshake(mux, fakeRegTokens{}, regTestRegistry(t, nil))
	rec := post(t, mux, "", RegisterRequest{Ordinal: 0})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
}

func TestRegister_WrongSecret(t *testing.T) {
	mk, _ := identity.MintServiceKey()
	other, _ := identity.MintServiceKey()
	_, generation, _ := regTestRegistry(t, nil).RegistrationIdentity("support")
	store := regCredential("support", "acme", generation, mk.Hash)
	mux := http.NewServeMux()
	RegisterHandshake(mux, store, regTestRegistry(t, nil))
	rec := post(t, mux, other.Plaintext, RegisterRequest{Ordinal: 0}) // valid format, wrong secret
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
}

func TestRegister_Revoked(t *testing.T) {
	mk, _ := identity.MintServiceKey()
	store := fakeRegTokens{err: identity.ErrNoRegToken}
	mux := http.NewServeMux()
	RegisterHandshake(mux, store, regTestRegistry(t, nil))
	rec := post(t, mux, mk.Plaintext, RegisterRequest{Ordinal: 0})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
}

func TestRegister_OrdinalOutOfRange(t *testing.T) {
	mk, _ := identity.MintServiceKey()
	reg := regTestRegistry(t, nil)
	_, generation, _ := reg.RegistrationIdentity("support")
	store := regCredential("support", "acme", generation, mk.Hash)
	mux := http.NewServeMux()
	RegisterHandshake(mux, store, reg)
	rec := post(t, mux, mk.Plaintext, RegisterRequest{Ordinal: 7}) // support has replicas:2
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", rec.Code)
	}
}

func TestRegister_UnknownAgent(t *testing.T) {
	mk, _ := identity.MintServiceKey()
	store := regCredential("ghost", "acme", "config:acme:ghost", mk.Hash)
	mux := http.NewServeMux()
	RegisterHandshake(mux, store, regTestRegistry(t, nil))
	rec := post(t, mux, mk.Plaintext, RegisterRequest{Ordinal: 0})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", rec.Code)
	}
}

func TestRegister_RejectsReassignedTenantOrGeneration(t *testing.T) {
	mk, _ := identity.MintServiceKey()
	reg := regTestRegistry(t, regFakeBroker{
		secrets: map[string]string{"NEW_TENANT_SECRET": "must-not-leak"},
	})

	for _, tc := range []struct {
		name, tenant, generation string
	}{
		{name: "old tenant", tenant: "former", generation: "config:former:support"},
		{name: "old generation", tenant: "acme", generation: "deleted-instance"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			RegisterHandshake(mux,
				regCredential("support", tc.tenant, tc.generation, mk.Hash), reg)
			rec := post(t, mux, mk.Plaintext, RegisterRequest{Ordinal: 0})
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("code=%d body=%s want 401", rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "must-not-leak") {
				t.Fatal("mismatched registration credential leaked brokered secret")
			}
		})
	}
}

func TestRegister_BrokerErrorFailsClosed(t *testing.T) {
	mk, _ := identity.MintServiceKey()
	broker := regFakeBroker{err: context.DeadlineExceeded}
	reg := regTestRegistry(t, broker)
	_, generation, _ := reg.RegistrationIdentity("support")
	store := regCredential("support", "acme", generation, mk.Hash)
	mux := http.NewServeMux()
	RegisterHandshake(mux, store, reg)
	rec := post(t, mux, mk.Plaintext, RegisterRequest{Ordinal: 0})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", rec.Code)
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("OPENAI")) {
		t.Fatalf("no partial env on broker error")
	}
}
