package controlplane

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/sausheong/runtime/internal/agentstore"
)

// AgentStore is the persistence the dynamic-agent API needs (satisfied by
// *agentstore.Store). Mirrors UpstreamStore.
type AgentStore interface {
	Insert(ctx context.Context, r agentstore.AgentRow) error
	List(ctx context.Context, tenant string) ([]agentstore.AgentRow, error)
	Get(ctx context.Context, id string) (agentstore.AgentRow, bool, error)
	Delete(ctx context.Context, tenant, id string) error
	SetEnabled(ctx context.Context, tenant, id string, enabled bool) error
}

// AgentManager applies a stored managed-agent row to the live registry + health
// monitors: attach/detach/enable/disable/re-attach. It is the dynamic-agent
// counterpart to gateway.Manager. Construct with NewAgentManager.
type AgentManager struct {
	reg     *Registry
	mon     *MonitorSet
	resolve func(ctx context.Context, tenant, secretName string) (string, error) // optional bearer resolver; nil ⇒ no bearer
}

// NewAgentManager binds a manager to the live registry and monitor set. resolver
// turns an optional per-tenant secret NAME (row.AuthSecret) into a bearer token
// at attach time (nil ⇒ agents carry no bearer, matching the shim today).
func NewAgentManager(reg *Registry, mon *MonitorSet, resolver func(context.Context, string, string) (string, error)) *AgentManager {
	return &AgentManager{reg: reg, mon: mon, resolve: resolver}
}

// process builds the AgentInfo + AgentProcess for a stored row, resolving the
// optional bearer. Remote/replica-0 are set by reg.AddRemote.
func (m *AgentManager) process(ctx context.Context, row agentstore.AgentRow) (AgentInfo, AgentProcess, error) {
	token := ""
	if row.AuthSecret != "" && m.resolve != nil {
		t, err := m.resolve(ctx, row.TenantID, row.AuthSecret)
		if err != nil {
			return AgentInfo{}, AgentProcess{}, fmt.Errorf("resolve agent credential: %w", err)
		}
		token = t
	}
	info := AgentInfo{ID: row.ID, Name: row.Name, Model: row.Model, Tenant: row.TenantID}
	ap := AgentProcess{AgentID: row.ID, BaseURL: row.URL, AuthToken: token, Tenant: row.TenantID}
	return info, ap, nil
}

// Attach registers row in the live registry (managed) and starts its health
// monitor. If the row is disabled, it is registered but immediately marked
// disabled (kept out of routing). Used at boot and by the register API.
func (m *AgentManager) Attach(ctx context.Context, row agentstore.AgentRow) error {
	info, ap, err := m.process(ctx, row)
	if err != nil {
		return err
	}
	m.reg.AddRemote(info, ap, true)
	if !row.Enabled {
		m.reg.SetEnabled(row.ID, false)
	}
	m.mon.Start(ap)
	return nil
}

// Detach removes the agent from the registry and stops its monitor.
func (m *AgentManager) Detach(id string) {
	m.reg.RemoveAgent(id)
	m.mon.Stop(id)
}

// SetEnabled flips the agent's routing eligibility in the live registry.
func (m *AgentManager) SetEnabled(id string, enabled bool) { m.reg.SetEnabled(id, enabled) }

// Reattach restarts the agent's health monitor (re-probe now) using its current
// dial target. No-op if the agent is unknown.
func (m *AgentManager) Reattach(id string) {
	ap, ok := m.reg.Replica(id, 0)
	if !ok {
		return
	}
	m.mon.Restart(ap)
}

// IsManaged reports whether id is a dynamically-managed agent.
func (m *AgentManager) IsManaged(id string) bool { return m.reg.IsManaged(id) }

// AgentParams is the validated request to register a managed agent.
type AgentParams struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Model      string `json:"model"`
	URL        string `json:"url"`
	AuthSecret string `json:"auth_secret"`
}

// RegisterAgentShared validates params, persists the row, and attaches it to the
// live registry + monitors. HTTP-agnostic (callers map the error to a status).
func RegisterAgentShared(ctx context.Context, store AgentStore, mgr *AgentManager, tenant string, p AgentParams) (agentstore.AgentRow, error) {
	id := strings.TrimSpace(p.ID)
	if id == "" {
		return agentstore.AgentRow{}, fmt.Errorf("id required")
	}
	if strings.ContainsAny(id, "/ ") {
		return agentstore.AgentRow{}, fmt.Errorf("id must not contain spaces or '/'")
	}
	if _, exists := mgr.reg.Get(id); exists {
		return agentstore.AgentRow{}, fmt.Errorf("an agent with id %q already exists", id)
	}
	u, err := url.Parse(strings.TrimSpace(p.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return agentstore.AgentRow{}, fmt.Errorf("url must be an absolute http(s) URL")
	}
	row := agentstore.AgentRow{
		ID: id, TenantID: tenant, Name: p.Name, Model: p.Model,
		URL: strings.TrimSpace(p.URL), AuthSecret: p.AuthSecret, Enabled: true,
	}
	if err := store.Insert(ctx, row); err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			return agentstore.AgentRow{}, fmt.Errorf("an agent with id %q already exists", id)
		}
		return agentstore.AgentRow{}, err
	}
	if err := mgr.Attach(ctx, row); err != nil {
		// roll back persistence so DB and live state stay consistent
		_ = store.Delete(ctx, tenant, id)
		return agentstore.AgentRow{}, err
	}
	return row, nil
}

// DeregisterAgentShared removes a managed agent (scoped to tenant) from the DB
// and the live registry. Rejects file-config agents (not managed). Idempotent.
func DeregisterAgentShared(ctx context.Context, store AgentStore, mgr *AgentManager, tenant, id string) error {
	if _, ok := mgr.reg.Get(id); ok && !mgr.IsManaged(id) {
		return fmt.Errorf("agent %q is file-configured, not dynamically managed; edit runtime config to remove it", id)
	}
	if err := store.Delete(ctx, tenant, id); err != nil {
		return err
	}
	mgr.Detach(id)
	return nil
}

// RegisterAgentAdmin mounts the /admin/agents/* routes. Every handler requires an
// admin Principal and scopes to that principal's tenant. Managed-agent ops
// (enable/disable/restart/delete) act only on dynamically-registered agents.
func RegisterAgentAdmin(mux *http.ServeMux, s AgentStore, adminStore AdminStore, mgr *AgentManager) {
	mux.HandleFunc("POST /admin/agents", func(w http.ResponseWriter, r *http.Request) {
		p, ok := requireAdmin(w, r)
		if !ok {
			return
		}
		var body AgentParams
		var raw struct {
			AgentParams
			Tenant string `json:"tenant"`
		}
		if !decode(w, r, &raw) {
			return
		}
		body = raw.AgentParams
		tenant, ok := effectiveTenant(w, r, adminStore, p, raw.Tenant)
		if !ok {
			return
		}
		row, err := RegisterAgentShared(r.Context(), s, mgr, tenant, body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"id": row.ID})
	})

	mux.HandleFunc("DELETE /admin/agents/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, ok := requireAdmin(w, r)
		if !ok {
			return
		}
		tenant, ok := effectiveTenant(w, r, adminStore, p, r.URL.Query().Get("tenant"))
		if !ok {
			return
		}
		if err := DeregisterAgentShared(r.Context(), s, mgr, tenant, r.PathValue("id")); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	setEnabled := func(enabled bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			p, ok := requireAdmin(w, r)
			if !ok {
				return
			}
			id := r.PathValue("id")
			tenant, ok := effectiveTenant(w, r, adminStore, p, r.URL.Query().Get("tenant"))
			if !ok {
				return
			}
			if !mgr.IsManaged(id) {
				http.Error(w, "agent is not dynamically managed", http.StatusBadRequest)
				return
			}
			if err := s.SetEnabled(r.Context(), tenant, id, enabled); err != nil {
				serverError(w, "set enabled", err)
				return
			}
			mgr.SetEnabled(id, enabled)
			w.WriteHeader(http.StatusNoContent)
		}
	}
	mux.HandleFunc("POST /admin/agents/{id}/enable", setEnabled(true))
	mux.HandleFunc("POST /admin/agents/{id}/disable", setEnabled(false))

	mux.HandleFunc("POST /admin/agents/{id}/restart", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := requireAdmin(w, r); !ok {
			return
		}
		id := r.PathValue("id")
		if !mgr.IsManaged(id) {
			http.Error(w, "agent is not dynamically managed", http.StatusBadRequest)
			return
		}
		mgr.Reattach(id)
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("GET /admin/agents", func(w http.ResponseWriter, r *http.Request) {
		p, ok := requireAdmin(w, r)
		if !ok {
			return
		}
		tenant := p.TenantID
		if p.Superuser {
			tenant = r.URL.Query().Get("tenant") // "" ⇒ all tenants
		}
		rows, err := s.List(r.Context(), tenant)
		if err != nil {
			serverError(w, "list agents", err)
			return
		}
		type out struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Model   string `json:"model"`
			URL     string `json:"url"`
			Enabled bool   `json:"enabled"`
		}
		res := make([]out, 0, len(rows))
		for _, row := range rows {
			res = append(res, out{ID: row.ID, Name: row.Name, Model: row.Model, URL: row.URL, Enabled: row.Enabled})
		}
		writeJSON(w, http.StatusOK, res)
	})
}
