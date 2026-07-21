package controlplane

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/sausheong/runtime/internal/eval"
	"github.com/sausheong/runtime/internal/identity"
	"github.com/sausheong/runtime/internal/obs"
	"github.com/sausheong/runtime/internal/store"
)

// RegisterEvalAdmin mounts /admin/evals/* on mux. Admin-scoped; a nil EvalStore
// ⇒ 503. ctx is the server signal context used to launch run goroutines (a run
// must OUTLIVE the request, so run.Execute is launched with ctx, never
// r.Context()). reg supplies the agent-visibility check for run creation.
func RegisterEvalAdmin(ctx context.Context, mux *http.ServeMux, adminStore AdminStore, es eval.EvalStore, ps eval.PolicyStoreAPI, ctlStore store.Store, inv eval.Invoker, judge eval.Judge, reg *Registry, m *obs.ControlMetrics) {
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

	// --- online policies ---
	// Per-agent online-sampling policy CRUD. A nil policy store ⇒ 503 (online
	// sampling not configured). RBAC mirrors the sets/runs routes.

	mux.HandleFunc("POST /admin/evals/policy", func(w http.ResponseWriter, r *http.Request) {
		p, ok := requireAdmin(w, r)
		if !ok {
			return
		}
		if ps == nil {
			http.Error(w, "eval policies not configured", http.StatusServiceUnavailable)
			return
		}
		var body struct {
			Tenant     string           `json:"tenant"`
			Agent      string           `json:"agent"`
			SampleRate int              `json:"sample_rate"`
			Criteria   []eval.Criterion `json:"criteria"`
		}
		if !decode(w, r, &body) {
			return
		}
		target, ok := evalWriteTenant(w, p, body.Tenant)
		if !ok {
			return
		}
		pol := eval.Policy{Tenant: target, AgentID: body.Agent, SampleRate: body.SampleRate, Criteria: body.Criteria}
		if err := ps.PutPolicy(r.Context(), pol); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		slog.Info("eval policy stored", "tenant", target, "agent", body.Agent, "sample_rate", body.SampleRate, "criteria", len(body.Criteria))
		writeJSON(w, http.StatusCreated, map[string]any{"tenant": target, "agent": body.Agent, "sample_rate": body.SampleRate, "criteria": len(body.Criteria)})
	})

	mux.HandleFunc("GET /admin/evals/policy", func(w http.ResponseWriter, r *http.Request) {
		p, ok := requireAdmin(w, r)
		if !ok {
			return
		}
		if ps == nil {
			http.Error(w, "eval policies not configured", http.StatusServiceUnavailable)
			return
		}
		policies, err := ps.ListPolicies(r.Context(), evalReadTenant(p, r))
		if err != nil {
			serverError(w, "list eval policies", err)
			return
		}
		writeJSON(w, http.StatusOK, policies)
	})

	mux.HandleFunc("GET /admin/evals/policy/{agent}", func(w http.ResponseWriter, r *http.Request) {
		p, ok := requireAdmin(w, r)
		if !ok {
			return
		}
		if ps == nil {
			http.Error(w, "eval policies not configured", http.StatusServiceUnavailable)
			return
		}
		pol, found, err := ps.GetPolicy(r.Context(), evalReadTenant(p, r), r.PathValue("agent"))
		if err != nil {
			serverError(w, "get eval policy", err)
			return
		}
		if !found {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, pol)
	})

	mux.HandleFunc("DELETE /admin/evals/policy/{agent}", func(w http.ResponseWriter, r *http.Request) {
		p, ok := requireAdmin(w, r)
		if !ok {
			return
		}
		if ps == nil {
			http.Error(w, "eval policies not configured", http.StatusServiceUnavailable)
			return
		}
		if _, err := ps.DeletePolicy(r.Context(), evalReadTenant(p, r), r.PathValue("agent")); err != nil {
			serverError(w, "delete eval policy", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// --- online results ---
	// Read-only view of online-eval outcomes. M2 keeps results tenant/session
	// scoped: online_eval_results carries a tenant (not an agent_id) column, so
	// there is no session→agent join here. ?session= reads one session's results
	// (filtered to the caller's tenant so a cross-tenant caller sees empty);
	// otherwise the caller's whole tenant is listed (newest first, capped).
	mux.HandleFunc("GET /admin/evals/online-results", func(w http.ResponseWriter, r *http.Request) {
		p, ok := requireAdmin(w, r)
		if !ok {
			return
		}
		if ctlStore == nil {
			http.Error(w, "eval results not configured", http.StatusServiceUnavailable)
			return
		}
		readTenant := evalReadTenant(p, r)
		if session := r.URL.Query().Get("session"); session != "" {
			rows, err := ctlStore.ListOnlineResults(r.Context(), session)
			if err != nil {
				serverError(w, "list online results", err)
				return
			}
			out := make([]store.OnlineResult, 0, len(rows))
			for _, row := range rows {
				// Superuser scoping to "" ⇒ see all; otherwise pin to caller tenant.
				if readTenant == "" || row.Tenant == readTenant {
					out = append(out, row)
				}
			}
			writeJSON(w, http.StatusOK, out)
			return
		}
		// No session: a superuser with no ?tenant= would read "" which the store
		// treats as an empty tenant, not "all" — pin an empty read tenant to the
		// caller's own tenant so a plain superuser still sees its results.
		listTenant := readTenant
		if listTenant == "" {
			listTenant = p.TenantID
		}
		rows, err := ctlStore.ListOnlineResultsByTenant(r.Context(), listTenant, 200)
		if err != nil {
			serverError(w, "list online results by tenant", err)
			return
		}
		writeJSON(w, http.StatusOK, rows)
	})

	// --- failure breakdown (M3) ---
	// Per-agent terminal-session failure-category counts. ?agent=<id> is
	// required and must be visible to the caller (evalAgentVisible); optional
	// ?since=<dur> (a Go duration, e.g. "24h") bounds the window. Reads the
	// category agentd wrote via ctlStore (native agents share its Postgres).
	mux.HandleFunc("GET /admin/evals/failures", func(w http.ResponseWriter, r *http.Request) {
		p, ok := requireAdmin(w, r)
		if !ok {
			return
		}
		if ctlStore == nil {
			http.Error(w, "eval results not configured", http.StatusServiceUnavailable)
			return
		}
		agent := r.URL.Query().Get("agent")
		if agent == "" {
			http.Error(w, "agent required", http.StatusBadRequest)
			return
		}
		if !evalAgentVisible(reg, p, agent) {
			http.Error(w, "unknown or invisible agent", http.StatusBadRequest)
			return
		}
		var since time.Time
		if s := strings.TrimSpace(r.URL.Query().Get("since")); s != "" {
			d, err := time.ParseDuration(s)
			if err != nil {
				http.Error(w, "bad since duration: "+err.Error(), http.StatusBadRequest)
				return
			}
			since = time.Now().Add(-d)
		}
		breakdown, err := ctlStore.FailureBreakdownByAgent(r.Context(), agent, since)
		if err != nil {
			serverError(w, "failure breakdown", err)
			return
		}
		if breakdown == nil {
			breakdown = map[string]int{}
		}
		writeJSON(w, http.StatusOK, breakdown)
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
