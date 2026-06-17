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
func (f *fakeAgentStore) Delete(_ context.Context, tenant, id string) error {
	if r, ok := f.rows[id]; ok && r.TenantID == tenant {
		delete(f.rows, id)
	}
	return nil
}
func (f *fakeAgentStore) SetEnabled(_ context.Context, tenant, id string, enabled bool) error {
	if r, ok := f.rows[id]; ok && r.TenantID == tenant {
		r.Enabled = enabled
		f.rows[id] = r
	}
	return nil
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
	body := `{"id":"hello","name":"Hello","model":"m","url":"http://127.0.0.1:8310"}`
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
