package controlplane

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sausheong/runtime/internal/agentstore"
	"github.com/sausheong/runtime/internal/config"
	"github.com/sausheong/runtime/internal/identity"
)

// fakeAgentStore implements AgentStore in-memory.
type fakeAgentStore struct {
	rows map[string]agentstore.AgentRow
}

func newFakeAgentStore() *fakeAgentStore {
	return &fakeAgentStore{rows: map[string]agentstore.AgentRow{}}
}
func (f *fakeAgentStore) Insert(_ context.Context, r agentstore.AgentRow) error {
	if _, ok := f.rows[r.ID]; ok {
		return errDuplicate
	}
	f.rows[r.ID] = r
	return nil
}
func (f *fakeAgentStore) List(_ context.Context, tenant string) ([]agentstore.AgentRow, error) {
	var out []agentstore.AgentRow
	for _, r := range f.rows {
		if tenant == "" || r.TenantID == tenant {
			out = append(out, r)
		}
	}
	return out, nil
}
func (f *fakeAgentStore) Get(_ context.Context, id string) (agentstore.AgentRow, bool, error) {
	r, ok := f.rows[id]
	return r, ok, nil
}
func (f *fakeAgentStore) Delete(_ context.Context, tenant, id string) (bool, error) {
	if r, ok := f.rows[id]; ok && r.TenantID == tenant {
		delete(f.rows, id)
		return true, nil
	}
	return false, nil
}
func (f *fakeAgentStore) SetEnabled(_ context.Context, tenant, id string, enabled bool) (bool, error) {
	if r, ok := f.rows[id]; ok && r.TenantID == tenant {
		r.Enabled = enabled
		f.rows[id] = r
		return true, nil
	}
	return false, nil
}

func TestAgentManagerAttachDoesNotOverrideFileConfiguredAgent(t *testing.T) {
	reg := NewRegistry(&config.Config{Agents: []config.AgentConfig{{
		ID: "shared", Name: "Operator agent", Model: "test/model",
		Tenant: "acme", URL: "http://10.10.0.4:8302",
	}}}, "", "")
	mgr := NewAgentManager(reg, nil, nil)

	err := mgr.Attach(context.Background(), agentstore.AgentRow{
		ID: "shared", TenantID: "acme", Name: "Managed agent",
		Model: "test/model", URL: "http://10.10.0.4:9999", Enabled: true,
	})
	if err == nil || !strings.Contains(err.Error(), "operator file") {
		t.Fatalf("Attach error = %v, want operator-file collision", err)
	}
	if !IsFileAgentConflict(err) {
		t.Fatalf("Attach error = %v, want typed file-agent conflict", err)
	}
	ap, ok := reg.Get("shared")
	if !ok || ap.BaseURL != "http://10.10.0.4:8302" {
		t.Fatalf("file-configured agent was replaced: ok=%v base=%q", ok, ap.BaseURL)
	}
	if reg.IsManaged("shared") {
		t.Fatal("file-configured agent was incorrectly marked managed")
	}
}

func TestDeregisterShadowedRowPreservesFileAgent(t *testing.T) {
	reg := NewRegistry(&config.Config{Agents: []config.AgentConfig{{
		ID: "shared", Name: "Operator agent", Model: "test/model",
		Tenant: "acme", URL: "http://10.10.0.4:8302",
	}}}, "", "")
	mgr := NewAgentManager(reg, nil, nil)
	s := newFakeAgentStore()
	s.rows["shared"] = agentstore.AgentRow{
		ID: "shared", TenantID: "acme", Name: "Stale",
		URL: "https://old.example", Enabled: false,
	}

	if err := DeregisterAgentShared(context.Background(), s, nil, mgr, "acme", "shared"); err != nil {
		t.Fatalf("DeregisterAgentShared: %v", err)
	}
	if _, ok := s.rows["shared"]; ok {
		t.Fatal("shadowed persisted row was not deleted")
	}
	ap, ok := reg.Get("shared")
	if !ok || ap.BaseURL != "http://10.10.0.4:8302" {
		t.Fatalf("file-configured agent was detached: ok=%v ap=%+v", ok, ap)
	}
}

type dupErr struct{}

func (dupErr) Error() string { return "duplicate key value" }

var errDuplicate = dupErr{}

// agentAdminMux wires a real registry + monitor set + manager behind the API,
// so handler effects are observable on the registry.
func agentAdminMux(t *testing.T) (http.Handler, *fakeAgentStore, *Registry) {
	t.Helper()
	reg := NewRegistry(&config.Config{}, "/bin/agentd", "dsn")
	ms := NewMonitorSet(context.Background(), reg, nil)
	mgr := NewAgentManager(reg, ms, nil)
	s := newFakeAgentStore()
	as := newFakeAdminStore()
	_ = as.CreateTenant(context.Background(), "acme", "Acme")
	mux := http.NewServeMux()
	RegisterAgentAdmin(mux, s, as, mgr)
	return mux, s, reg
}

func acmeAdmin(r *http.Request) *http.Request {
	return withPrincipal(r, identity.Principal{TenantID: "acme", Role: identity.RoleAdmin})
}

func TestAgentAdmin_RegisterAttachesAndPersists(t *testing.T) {
	mux, s, reg := agentAdminMux(t)
	body := `{"id":"hello","name":"Hello","model":"m","url":"https://example.com"}`
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, acmeAdmin(httptest.NewRequest("POST", "/admin/agents", strings.NewReader(body))))
	if rec.Code != 201 {
		t.Fatalf("register: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := s.rows["hello"]; !ok {
		t.Fatal("row not persisted")
	}
	if _, ok := reg.Get("hello"); !ok {
		t.Fatal("agent not attached to registry")
	}
	if !reg.IsManaged("hello") {
		t.Fatal("agent should be managed")
	}
}

func TestAgentAdmin_RegisterRejectsBadURL(t *testing.T) {
	mux, _, _ := agentAdminMux(t)
	body := `{"id":"hello","url":"not-a-url"}`
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, acmeAdmin(httptest.NewRequest("POST", "/admin/agents", strings.NewReader(body))))
	if rec.Code != 400 {
		t.Fatalf("bad url: code=%d want 400", rec.Code)
	}
}

func TestAgentAdmin_RegisterRejectsPrivateAndMetadataURLs(t *testing.T) {
	mux, _, _ := agentAdminMux(t)
	for _, target := range []string{
		"http://127.0.0.1:8310",
		"http://10.0.0.10",
		"http://169.254.169.254/latest/meta-data",
		"http://metadata.google.internal",
	} {
		body := `{"id":"hello","url":"` + target + `"}`
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, acmeAdmin(httptest.NewRequest("POST", "/admin/agents", strings.NewReader(body))))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("target %q: code=%d want 400", target, rec.Code)
		}
	}
}

func TestAgentAdmin_NonAdminForbidden(t *testing.T) {
	mux, _, _ := agentAdminMux(t)
	rec := httptest.NewRecorder()
	r := withPrincipal(httptest.NewRequest("POST", "/admin/agents", strings.NewReader(`{"id":"x","url":"http://x"}`)),
		identity.Principal{TenantID: "acme", Role: identity.RoleOperator})
	mux.ServeHTTP(rec, r)
	if rec.Code != 403 {
		t.Fatalf("operator: code=%d want 403", rec.Code)
	}
}

func TestAgentAdmin_DisableEnable(t *testing.T) {
	mux, s, reg := agentAdminMux(t)
	mux.ServeHTTP(httptest.NewRecorder(), acmeAdmin(httptest.NewRequest("POST", "/admin/agents",
		strings.NewReader(`{"id":"hello","url":"http://x"}`))))

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, acmeAdmin(httptest.NewRequest("POST", "/admin/agents/hello/disable", nil)))
	if rec.Code != 204 {
		t.Fatalf("disable: code=%d", rec.Code)
	}
	if !reg.Disabled("hello") {
		t.Fatal("registry should mark hello disabled")
	}
	if s.rows["hello"].Enabled {
		t.Fatal("store should mark hello disabled")
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, acmeAdmin(httptest.NewRequest("POST", "/admin/agents/hello/enable", nil)))
	if rec.Code != 204 || reg.Disabled("hello") {
		t.Fatalf("enable: code=%d disabled=%v", rec.Code, reg.Disabled("hello"))
	}
}

func TestAgentAdmin_Deregister(t *testing.T) {
	mux, s, reg := agentAdminMux(t)
	mux.ServeHTTP(httptest.NewRecorder(), acmeAdmin(httptest.NewRequest("POST", "/admin/agents",
		strings.NewReader(`{"id":"hello","url":"http://x"}`))))

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, acmeAdmin(httptest.NewRequest("DELETE", "/admin/agents/hello", nil)))
	if rec.Code != 204 {
		t.Fatalf("deregister: code=%d", rec.Code)
	}
	if _, ok := s.rows["hello"]; ok {
		t.Fatal("row should be deleted")
	}
	if _, ok := reg.Get("hello"); ok {
		t.Fatal("agent should be detached from registry")
	}
}

func TestAgentAdmin_DeregisterRevokesDeletedGeneration(t *testing.T) {
	reg := NewRegistry(&config.Config{}, "/bin/agentd", "dsn")
	mgr := NewAgentManager(reg, NewMonitorSet(context.Background(), reg, nil), nil)
	agents := newFakeAgentStore()
	admin := newFakeAdminStore()
	_ = admin.CreateTenant(context.Background(), "acme", "Acme")
	mux := http.NewServeMux()
	RegisterAgentAdmin(mux, agents, admin, mgr)

	create := httptest.NewRecorder()
	mux.ServeHTTP(create, acmeAdmin(httptest.NewRequest("POST", "/admin/agents",
		strings.NewReader(`{"id":"reusable","url":"https://example.com"}`))))
	if create.Code != http.StatusCreated {
		t.Fatalf("create code=%d body=%s", create.Code, create.Body.String())
	}
	row := agents.rows["reusable"]
	if row.RegistrationGeneration == "" {
		t.Fatal("managed agent has no registration generation")
	}
	admin.regTokens["old-token"] = identity.RegTokenRow{
		TokenID: "old-token", AgentID: row.ID, TenantID: row.TenantID,
		AgentGeneration: row.RegistrationGeneration,
	}

	remove := httptest.NewRecorder()
	mux.ServeHTTP(remove, acmeAdmin(httptest.NewRequest("DELETE", "/admin/agents/reusable", nil)))
	if remove.Code != http.StatusNoContent {
		t.Fatalf("delete code=%d body=%s", remove.Code, remove.Body.String())
	}
	if !admin.regTokens["old-token"].Revoked {
		t.Fatal("deleting managed agent did not revoke its registration tokens")
	}

	recreate := httptest.NewRecorder()
	mux.ServeHTTP(recreate, acmeAdmin(httptest.NewRequest("POST", "/admin/agents",
		strings.NewReader(`{"id":"reusable","url":"https://example.com"}`))))
	if recreate.Code != http.StatusCreated {
		t.Fatalf("recreate code=%d body=%s", recreate.Code, recreate.Body.String())
	}
	if next := agents.rows["reusable"].RegistrationGeneration; next == row.RegistrationGeneration {
		t.Fatalf("delete/recreate reused generation %q", next)
	}
}

// A file-config agent (not managed) must not be deregisterable via this API.
func TestAgentAdmin_DeregisterRejectsFileAgent(t *testing.T) {
	reg := NewRegistry(&config.Config{Agents: []config.AgentConfig{
		{ID: "fileagent", Name: "F", Model: "m", URL: "http://x", Tenant: "acme"},
	}}, "/bin/agentd", "dsn")
	ms := NewMonitorSet(context.Background(), reg, nil)
	mgr := NewAgentManager(reg, ms, nil)
	s := newFakeAgentStore()
	as := newFakeAdminStore()
	_ = as.CreateTenant(context.Background(), "acme", "Acme")
	mux := http.NewServeMux()
	RegisterAgentAdmin(mux, s, as, mgr)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, acmeAdmin(httptest.NewRequest("DELETE", "/admin/agents/fileagent", nil)))
	if rec.Code != 400 {
		t.Fatalf("deregister file agent: code=%d want 400 (rejected)", rec.Code)
	}
	if _, ok := reg.Get("fileagent"); !ok {
		t.Fatal("file agent must remain registered")
	}
}

func TestAgentAdmin_RestartRejectsUnmanaged(t *testing.T) {
	mux, _, _ := agentAdminMux(t)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, acmeAdmin(httptest.NewRequest("POST", "/admin/agents/ghost/restart", nil)))
	if rec.Code != 400 {
		t.Fatalf("restart unmanaged: code=%d want 400", rec.Code)
	}
}

func TestAgentAdmin_CrossTenantMutationsDoNotTouchLiveAgent(t *testing.T) {
	for _, tc := range []struct {
		name, method, path string
	}{
		{"disable", "POST", "/admin/agents/victim/disable"},
		{"enable", "POST", "/admin/agents/victim/enable"},
		{"restart", "POST", "/admin/agents/victim/restart"},
		{"delete", "DELETE", "/admin/agents/victim"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux, s, reg := agentAdminMux(t)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, acmeAdmin(httptest.NewRequest("POST", "/admin/agents",
				strings.NewReader(`{"id":"victim","url":"https://example.com"}`))))
			if rec.Code != http.StatusCreated {
				t.Fatalf("register: code=%d body=%s", rec.Code, rec.Body.String())
			}
			row := s.rows["victim"]
			row.TenantID = "beta" // model a globally-known id owned by another tenant
			s.rows["victim"] = row

			rec = httptest.NewRecorder()
			mux.ServeHTTP(rec, acmeAdmin(httptest.NewRequest(tc.method, tc.path, nil)))
			if rec.Code != http.StatusNotFound && rec.Code != http.StatusBadRequest {
				t.Fatalf("%s cross-tenant: code=%d body=%s", tc.name, rec.Code, rec.Body.String())
			}
			if _, ok := reg.Get("victim"); !ok {
				t.Fatalf("%s cross-tenant detached live agent", tc.name)
			}
			if _, ok := s.rows["victim"]; !ok {
				t.Fatalf("%s cross-tenant deleted persistence row", tc.name)
			}
		})
	}
}
