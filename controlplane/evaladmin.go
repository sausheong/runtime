package controlplane

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"

	"github.com/sausheong/runtime/internal/eval"
	"github.com/sausheong/runtime/internal/identity"
	"github.com/sausheong/runtime/internal/obs"
)

// RegisterEvalAdmin mounts /admin/evals/* on mux. Admin-scoped; a nil EvalStore
// ⇒ 503. ctx is the server signal context used to launch run goroutines (a run
// must OUTLIVE the request, so run.Execute is launched with ctx, never
// r.Context()). reg supplies the agent-visibility check for run creation.
func RegisterEvalAdmin(ctx context.Context, mux *http.ServeMux, store AdminStore, es eval.EvalStore, inv eval.Invoker, judge eval.Judge, reg *Registry, m *obs.ControlMetrics) {
	// --- sets ---

	mux.HandleFunc("POST /admin/evals/sets", func(w http.ResponseWriter, r *http.Request) {
		p, ok := requireAdmin(w, r)
		if !ok {
			return
		}
		if es == nil {
			http.Error(w, "evals not configured", http.StatusServiceUnavailable)
			return
		}
		var body struct {
			Tenant string      `json:"tenant"`
			Name   string      `json:"name"`
			Cases  []eval.Case `json:"cases"`
		}
		if !decode(w, r, &body) {
			return
		}
		target, ok := evalWriteTenant(w, p, body.Tenant)
		if !ok {
			return
		}
		if err := eval.ValidateSet(body.Name, body.Cases); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := es.PutSet(r.Context(), eval.Set{Tenant: target, Name: body.Name, Cases: body.Cases}); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		slog.Info("eval set stored", "tenant", target, "set", body.Name, "cases", len(body.Cases))
		writeJSON(w, http.StatusCreated, map[string]any{"tenant": target, "name": body.Name, "cases": len(body.Cases)})
	})

	mux.HandleFunc("GET /admin/evals/sets", func(w http.ResponseWriter, r *http.Request) {
		p, ok := requireAdmin(w, r)
		if !ok {
			return
		}
		if es == nil {
			http.Error(w, "evals not configured", http.StatusServiceUnavailable)
			return
		}
		sets, err := es.ListSets(r.Context(), evalReadTenant(p, r))
		if err != nil {
			serverError(w, "list eval sets", err)
			return
		}
		type dto struct {
			Tenant string `json:"tenant"`
			Name   string `json:"name"`
			Cases  int    `json:"cases"`
		}
		out := make([]dto, 0, len(sets))
		for _, s := range sets {
			out = append(out, dto{Tenant: s.Tenant, Name: s.Name, Cases: len(s.Cases)})
		}
		writeJSON(w, http.StatusOK, out)
	})

	mux.HandleFunc("GET /admin/evals/sets/{name}", func(w http.ResponseWriter, r *http.Request) {
		p, ok := requireAdmin(w, r)
		if !ok {
			return
		}
		if es == nil {
			http.Error(w, "evals not configured", http.StatusServiceUnavailable)
			return
		}
		set, found, err := es.GetSet(r.Context(), evalReadTenant(p, r), r.PathValue("name"))
		if err != nil {
			serverError(w, "get eval set", err)
			return
		}
		if !found {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, set)
	})

	mux.HandleFunc("DELETE /admin/evals/sets/{name}", func(w http.ResponseWriter, r *http.Request) {
		p, ok := requireAdmin(w, r)
		if !ok {
			return
		}
		if es == nil {
			http.Error(w, "evals not configured", http.StatusServiceUnavailable)
			return
		}
		if _, err := es.DeleteSet(r.Context(), evalReadTenant(p, r), r.PathValue("name")); err != nil {
			serverError(w, "delete eval set", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// --- runs ---

	mux.HandleFunc("POST /admin/evals/runs", func(w http.ResponseWriter, r *http.Request) {
		p, ok := requireAdmin(w, r)
		if !ok {
			return
		}
		if es == nil {
			http.Error(w, "evals not configured", http.StatusServiceUnavailable)
			return
		}
		var body struct {
			Tenant string `json:"tenant"`
			Set    string `json:"set"`
			Agent  string `json:"agent"`
		}
		if !decode(w, r, &body) {
			return
		}
		target, ok := evalWriteTenant(w, p, body.Tenant)
		if !ok {
			return
		}
		if body.Set == "" || body.Agent == "" {
			http.Error(w, "set and agent required", http.StatusBadRequest)
			return
		}
		// The set must exist under the target tenant.
		if _, found, err := es.GetSet(r.Context(), target, body.Set); err != nil {
			serverError(w, "get eval set", err)
			return
		} else if !found {
			http.Error(w, "unknown set", http.StatusBadRequest)
			return
		}
		// The agent must be visible to the caller.
		if !evalAgentVisible(reg, p, body.Agent) {
			http.Error(w, "unknown or invisible agent", http.StatusBadRequest)
			return
		}
		runID, err := mintEvalRunID()
		if err != nil {
			serverError(w, "mint run id", err)
			return
		}
		if err := es.CreateRun(r.Context(), eval.Run{
			RunID: runID, Tenant: target, SetName: body.Set, AgentID: body.Agent, Status: eval.StatusPending,
		}); err != nil {
			serverError(w, "create eval run", err)
			return
		}
		// The run must outlive the request: launch on the server ctx, not r.Context().
		go eval.Execute(ctx, es, inv, judge, runID, m)
		slog.Info("eval run started", "tenant", target, "set", body.Set, "agent", body.Agent, "run", runID)
		writeJSON(w, http.StatusAccepted, map[string]string{"run_id": runID})
	})

	mux.HandleFunc("GET /admin/evals/runs", func(w http.ResponseWriter, r *http.Request) {
		p, ok := requireAdmin(w, r)
		if !ok {
			return
		}
		if es == nil {
			http.Error(w, "evals not configured", http.StatusServiceUnavailable)
			return
		}
		runs, err := es.ListRuns(r.Context(), evalReadTenant(p, r))
		if err != nil {
			serverError(w, "list eval runs", err)
			return
		}
		writeJSON(w, http.StatusOK, runs)
	})

	mux.HandleFunc("GET /admin/evals/runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, ok := requireAdmin(w, r)
		if !ok {
			return
		}
		if es == nil {
			http.Error(w, "evals not configured", http.StatusServiceUnavailable)
			return
		}
		run, found, err := es.GetRun(r.Context(), r.PathValue("id"))
		if err != nil {
			serverError(w, "get eval run", err)
			return
		}
		if !found || !evalRunVisible(p, run) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, run)
	})

	mux.HandleFunc("GET /admin/evals/runs/{id}/results", func(w http.ResponseWriter, r *http.Request) {
		p, ok := requireAdmin(w, r)
		if !ok {
			return
		}
		if es == nil {
			http.Error(w, "evals not configured", http.StatusServiceUnavailable)
			return
		}
		run, found, err := es.GetRun(r.Context(), r.PathValue("id"))
		if err != nil {
			serverError(w, "get eval run", err)
			return
		}
		if !found || !evalRunVisible(p, run) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		results, err := es.ListResults(r.Context(), run.RunID)
		if err != nil {
			serverError(w, "list eval results", err)
			return
		}
		writeJSON(w, http.StatusOK, results)
	})
}

// evalWriteTenant resolves the tenant a write should target. An empty body
// tenant defaults to the caller's own tenant. A non-superuser naming another
// tenant or the "*" wildcard is rejected with 403 (after writing the response).
func evalWriteTenant(w http.ResponseWriter, p identity.Principal, bodyTenant string) (string, bool) {
	target := bodyTenant
	if target == "" {
		target = p.TenantID
	}
	if !p.Superuser && (target == "*" || target != p.TenantID) {
		http.Error(w, "forbidden: cannot target another tenant", http.StatusForbidden)
		return "", false
	}
	return target, true
}

// evalReadTenant is the tenant scope for reads: a non-superuser is pinned to
// its own tenant; a superuser may narrow with ?tenant= ("" ⇒ all).
func evalReadTenant(p identity.Principal, r *http.Request) string {
	if p.Superuser {
		return r.URL.Query().Get("tenant")
	}
	return p.TenantID
}

// evalAgentVisible reports whether agentID exists and the caller may target it:
// a superuser sees every agent; a tenant admin sees only its own agents.
func evalAgentVisible(reg *Registry, p identity.Principal, agentID string) bool {
	if reg == nil {
		return false
	}
	info, ok := reg.Get(agentID)
	return ok && (p.Superuser || info.Tenant == p.TenantID)
}

// evalRunVisible enforces run ownership: a superuser sees any run; otherwise the
// run's tenant must match the caller's.
func evalRunVisible(p identity.Principal, run eval.Run) bool {
	return p.Superuser || run.Tenant == p.TenantID
}

// mintEvalRunID returns a 16-byte hex run identifier.
func mintEvalRunID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
