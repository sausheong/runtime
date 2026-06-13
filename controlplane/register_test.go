package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sausheong/runtime/internal/config"
	"github.com/sausheong/runtime/internal/identity"
)

// fakeRegTokens implements RegTokenVerifier for hermetic tests.
type fakeRegTokens struct {
	agentID, hash string
	err           error
}

func (f fakeRegTokens) ActiveRegTokenByID(_ context.Context, id string) (string, string, error) {
	if f.err != nil {
		return "", "", f.err
	}
	return f.agentID, f.hash, nil
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
			URL: "http://127.0.0.1:900{i}", Replicas: 2},
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
	store := fakeRegTokens{agentID: "support", hash: mk.Hash}
	broker := regFakeBroker{secrets: map[string]string{"OPENAI_API_KEY": "sk-xyz"}}
	mux := http.NewServeMux()
	RegisterHandshake(mux, store, regTestRegistry(t, broker))

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
		resp.Env["RUNTIME_AGENT_REPLICA"] != "1" ||
		resp.Env["DBOS__VMID"] != "" || // remote pool: DBOSVMID empty (remote owns its id)
		resp.Env["OPENAI_API_KEY"] != "sk-xyz" {
		t.Fatalf("unexpected env: %+v", resp.Env)
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
	store := fakeRegTokens{agentID: "support", hash: mk.Hash}
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
	store := fakeRegTokens{agentID: "support", hash: mk.Hash}
	mux := http.NewServeMux()
	RegisterHandshake(mux, store, regTestRegistry(t, nil))
	rec := post(t, mux, mk.Plaintext, RegisterRequest{Ordinal: 7}) // support has replicas:2
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", rec.Code)
	}
}

func TestRegister_UnknownAgent(t *testing.T) {
	mk, _ := identity.MintServiceKey()
	store := fakeRegTokens{agentID: "ghost", hash: mk.Hash}
	mux := http.NewServeMux()
	RegisterHandshake(mux, store, regTestRegistry(t, nil))
	rec := post(t, mux, mk.Plaintext, RegisterRequest{Ordinal: 0})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", rec.Code)
	}
}

func TestRegister_BrokerErrorFailsClosed(t *testing.T) {
	mk, _ := identity.MintServiceKey()
	store := fakeRegTokens{agentID: "support", hash: mk.Hash}
	broker := regFakeBroker{err: context.DeadlineExceeded}
	mux := http.NewServeMux()
	RegisterHandshake(mux, store, regTestRegistry(t, broker))
	rec := post(t, mux, mk.Plaintext, RegisterRequest{Ordinal: 0})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", rec.Code)
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("OPENAI")) {
		t.Fatalf("no partial env on broker error")
	}
}
