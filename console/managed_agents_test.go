package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sausheong/runtime/controlplane"
	"github.com/sausheong/runtime/internal/agentstore"
	"github.com/sausheong/runtime/internal/config"
)

// fakeAgentStore3 implements controlplane.AgentStore in-memory for console tests.
type fakeAgentStore3 struct{ rows map[string]agentstore.AgentRow }

func newFakeAgentStore3() *fakeAgentStore3 {
	return &fakeAgentStore3{rows: map[string]agentstore.AgentRow{}}
}
func (f *fakeAgentStore3) Insert(_ context.Context, r agentstore.AgentRow) error {
	f.rows[r.ID] = r
	return nil
}
func (f *fakeAgentStore3) List(_ context.Context, tenant string) ([]agentstore.AgentRow, error) {
	var out []agentstore.AgentRow
	for _, r := range f.rows {
		if tenant == "" || r.TenantID == tenant {
			out = append(out, r)
		}
	}
	return out, nil
}
func (f *fakeAgentStore3) Get(_ context.Context, id string) (agentstore.AgentRow, bool, error) {
	r, ok := f.rows[id]
	return r, ok, nil
}
func (f *fakeAgentStore3) Delete(_ context.Context, tenant, id string) error {
	delete(f.rows, id)
	return nil
}
func (f *fakeAgentStore3) SetEnabled(_ context.Context, tenant, id string, enabled bool) error {
	if r, ok := f.rows[id]; ok {
		r.Enabled = enabled
		f.rows[id] = r
	}
	return nil
}

// consoleWithAgents wires a console whose onboarding deps include a real
// registry + monitor set + agent manager, so the managed-agents UI is active.
func consoleWithAgents(t *testing.T) (http.Handler, *fakeAgentStore3, *controlplane.Registry) {
	t.Helper()
	reg := controlplane.NewRegistry(&config.Config{}, "/bin/agentd", "dsn")
	ms := controlplane.NewMonitorSet(context.Background(), reg, nil)
	mgr := controlplane.NewAgentManager(reg, ms, nil)
	as := newFakeAgentStore3()
	deps := &Onboarding{
		Upstreams: &fakeUpstreamStore2{},
		Mutator:   &fakeMut2{},
		Admin:     &fakeAdmin2{},
		Secrets:   &fakeSec2{},
		Agents:    as,
		AgentMgr:  mgr,
	}
	return Handler(nil, nil, OIDCConfig{}, deps), as, reg
}

func TestManagedAgents_SectionRenders(t *testing.T) {
	h, as, _ := consoleWithAgents(t)
	as.rows["hello"] = agentstore.AgentRow{ID: "hello", TenantID: "t1", Name: "Hello", URL: "http://x", Enabled: true}
	r := adminReq("GET", "/ui/onboarding", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	body := w.Body.String()
	if !strings.Contains(body, "Managed agents") {
		t.Fatal("managed agents section missing")
	}
	if !strings.Contains(body, "/ui/onboarding/agents/hello/disable") {
		t.Fatal("expected a disable action for the enabled agent")
	}
}

func TestManagedAgents_RegisterRequiresCSRF(t *testing.T) {
	h, _, _ := consoleWithAgents(t)
	form := url.Values{"id": {"x"}, "url": {"http://x"}} // no csrf_token
	r := adminReq("POST", "/ui/onboarding/agents", form)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("register without csrf: want 403 got %d", w.Code)
	}
}

func TestManagedAgents_RegisterAttaches(t *testing.T) {
	h, as, reg := consoleWithAgents(t)
	token := issuedCSRF(t, h)
	form := url.Values{
		"csrf_token": {token}, "id": {"hello"},
		"name": {"Hello"}, "url": {"http://127.0.0.1:8310"},
	}
	r := adminReq("POST", "/ui/onboarding/agents", form)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("register: want 303 got %d (%s)", w.Code, w.Body.String())
	}
	if _, ok := as.rows["hello"]; !ok {
		t.Fatal("row not persisted")
	}
	if _, ok := reg.Get("hello"); !ok {
		t.Fatal("agent not attached to registry")
	}
}

func TestManagedAgents_RegisterRequiresAdmin(t *testing.T) {
	h, _, _ := consoleWithAgents(t)
	token := issuedCSRF(t, h)
	form := url.Values{"csrf_token": {token}, "id": {"x"}, "url": {"http://x"}}
	r := nonAdminReq("POST", "/ui/onboarding/agents", form)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-admin register: want 403 got %d", w.Code)
	}
}
