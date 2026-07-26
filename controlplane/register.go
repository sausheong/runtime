package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/sausheong/runtime/internal/identity"
)

// RegTokenVerifier resolves a registration token to its immutable agent binding
// and bcrypt hash. *identity.Store implements it.
type RegTokenVerifier interface {
	ActiveRegTokenByID(ctx context.Context, tokenID string) (identity.RegTokenCredential, error)
}

// RegisterRequest is the agent's handshake body.
type RegisterRequest struct {
	Ordinal int `json:"ordinal"`
}

// RegisterResponse carries the env delta (KEY→VAL) the agent applies via os.Setenv.
type RegisterResponse struct {
	Env map[string]string `json:"env"`
}

// RegisterHandshake mounts POST /register. It authenticates with the agent's OWN
// per-agent registration token (NOT the identity middleware), so it is mounted
// OUTSIDE the identity chain (like /metrics). The token's binding to an agent_id
// is authoritative only together with its tenant and instance generation. The
// response is that exact agent identity's env delta for the claimed ordinal.
func RegisterHandshake(mux *http.ServeMux, tokens RegTokenVerifier, reg *Registry) {
	mux.HandleFunc("POST /register", func(w http.ResponseWriter, r *http.Request) {
		raw := extractToken(r) // existing helper in controlplane/auth.go
		id, secret, ok := identity.ParseServiceKey(raw)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		credential, err := tokens.ActiveRegTokenByID(r.Context(), id)
		if err != nil || !identity.VerifyKey(credential.Hash, secret) {
			// Uniform 401: no oracle distinguishing "no token" from "wrong secret".
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var body RegisterRequest
		// Cap the (auth-gated) body at 64 KiB; a too-large body surfaces as a decode error → 400.
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		// Tolerate an empty body (ordinal 0 default); reject only malformed JSON.
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			http.Error(w, "bad request body", http.StatusBadRequest)
			return
		}
		ap, ok := reg.Replica(credential.AgentID, body.Ordinal)
		if !ok {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if !ap.RegistrationEnabled ||
			credential.TenantID != ap.Tenant ||
			credential.AgentGeneration != ap.RegistrationGeneration {
			// Uniform 401: a removed/reassigned agent identity must not reveal
			// whether the same id currently exists in another trust domain.
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		delta, err := ap.envDelta(r.Context())
		if err != nil {
			// Broker error (e.g. undecryptable secret): deliberately treated as
			// fail-closed-unavailable (503, no partial env) rather than 500.
			slog.Error("register: envDelta failed", "agent", credential.AgentID, "tenant", ap.Tenant, "ordinal", body.Ordinal, "err", err)
			http.Error(w, "registration unavailable", http.StatusServiceUnavailable)
			return
		}
		env := make(map[string]string, len(delta))
		for _, kv := range delta {
			if i := strings.IndexByte(kv, '='); i >= 0 {
				env[kv[:i]] = kv[i+1:]
			}
		}
		// A valid one-time registration handshake is authoritative evidence
		// that this ordinal is restarting. Clear a stale "down" observation to
		// unknown so it may become routable as soon as its HTTP listener is
		// healthy, without waiting a full monitor interval.
		reg.ResetReplicaReachable(credential.AgentID, body.Ordinal)
		// Access log: identifiers only — NEVER an env value or secret name.
		slog.Info("register", "agent", credential.AgentID, "tenant", ap.Tenant, "ordinal", body.Ordinal, "token_id", id, "vars", len(env))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(RegisterResponse{Env: env})
	})
}
