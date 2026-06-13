# C3 M2 — Registration Handshake (Design)

**Date:** 2026-06-13
**Sub-project:** C3 (Remote agents — attach instead of spawn)
**Status:** Design approved; ready for implementation plan.
**Predecessor:** C3 M1 (attach-instead-of-spawn, `2026-06-13-c3-remote-agents-design.md`).
**Closes:** the C2 M2 limitation "brokered per-tenant secrets are spawn-time only,
so per-agent-pod agents get provider creds via the static chart Secret". (Does NOT
close the per-agent `gateway:` opt-in follow-up — gateway is rejected on remote
agents at config validation, so it remains unsupported; see §3.)

---

## 1. Problem

C3 M1 made the data plane location-agnostic: `runtimed` can ATTACH to an
already-running remote `agentd` (`url:` instead of `listen_addr:`), proxying,
health-checking, and reporting status without spawning or supervising it. C2 M2
built on that — each agent runs as its own Kubernetes StatefulSet that runtimed
attaches to as a remote replica pool.

But a process runtimed did not spawn never runs `AgentProcess.buildEnv`
(`controlplane/proxy.go:66`) — the function that, for every **local** child,
injects the tenant's **brokered per-tenant secrets** (`broker.SecretsFor(tenant)`
→ `NAME=val` env) along with the `RUNTIME_*` control vars and opt-in feature env
(memory flag, gateway URL/key). So today a scheduled/remote pod gets:

- core identity (DSN, agent id, tenant, kind, replica, DBOS VMID) from its
  StatefulSet env (C2 M2's `$HOSTNAME` sh-wrapper), and
- provider credentials from the **static chart Secret** — NOT from the
  identity secrets broker.

That means the AES-256-GCM per-tenant secrets brokering (Identity M2/M3) — the
platform's whole story for getting provider creds into agents securely — does not
reach K8s-scheduled or remote agents. C3 M2 closes that hole.

## 2. Approach

A **pull handshake** in the reverse direction of M1's data plane: the agent calls
back to the control plane, authenticates, and receives the exact environment a
local child would have been spawned with (minus runtimed's own process
environment). The control plane is the single source of truth for an agent's
environment; the agent is a thin fetch-then-run prelude.

### 2.1 Alternatives considered

- **A. Pull handshake reusing `buildEnv` — CHOSEN.** Agent boots with only a
  registration token + control-plane URL, POSTs `/register`, receives the env
  delta, `os.Setenv`s it, then runs the unchanged `os.Getenv` startup path.
  Reuses the existing identity token primitive, the existing agentd env path, and
  the existing env-assembly logic. Smallest blast radius; single source of truth.
- **B. Push provisioning (control-plane → agent at attach).** Rejected: requires
  the agent to expose a config-ingest endpoint and idle waiting; inverts the
  natural "child asks parent for its environment" flow; races the health monitor.
- **C. Sidecar/init-container writes a secrets file.** Rejected: K8s-only (breaks
  the plain remote-agent C3 case), and it's option A with extra packaging — the
  fetch logic still has to authenticate and live somewhere.

## 3. Architecture & trust model

A remote/scheduled `agentd` boots knowing only two things from its pod env:

- `RUNTIME_REGISTRATION_URL` — the control-plane `/register` endpoint.
- `RUNTIME_REGISTRATION_TOKEN` — a per-agent bearer (identity-backed, §4).

Before its normal startup, agentd performs the handshake: `POST {url}` with the
bearer and its `$HOSTNAME`-derived ordinal. The control plane:

1. verifies the token (the token IS the agent's identity — it binds to one
   `agent_id`, whose tenant comes from config),
2. validates the claimed ordinal against that agent's configured replica count,
3. computes the per-replica **env delta** (the entries `buildEnv` adds on top of
   the inherited env — never runtimed's own `os.Environ()`), and
4. returns the delta as JSON `{KEY: VAL}`.

agentd `os.Setenv`s every returned pair into its own process environment, then
runs the **exact existing** `os.Getenv` startup path. `Serve`, `agentkind`,
DBOS, and every downstream consumer are byte-for-byte unchanged.

**Trust boundary.** The registration token is a **bearer over operator-terminated
TLS** — ingress/mesh terminates HTTPS, the same trust model M1 used for the
runtimed→agent bearer. Because the token is per-agent and identity-backed, a
leaked token can fetch ONLY its own tenant's secrets, and ONLY for ordinals K8s
will actually create (§5 fail-closed bounds check). **mTLS is deferred** to a
later C3 item.

**Relationship to M1.** M1 made runtimed→agent location-agnostic and
authenticated (the `RUNTIME_AGENT_AUTH_TOKEN` shared bearer). M2 adds the reverse
hop — agent→control-plane config delivery — so a process runtimed didn't spawn
still gets its full brokered environment. The two tokens are distinct credentials
in distinct directions and are never conflated.

**Per-agent-pod gateway is NOT delivered by the handshake.** A remote agent
rejects the `gateway:` field at config validation (`config.Validate` forbids
spawn-time-only fields on a `url:` agent), so `GatewayOn` is false and the env
delta carries only the empty `RUNTIME_GATEWAY_URL`/`_KEY` shadow — which agentd
skips. Per-agent-pod gateway therefore remains unsupported and stays future work;
this handshake does not change that.

## 4. Token model & lifecycle

A new `registration_tokens` table, minted with the **existing**
`identity.MintServiceKey` primitive (bcrypt `id.secret`; plaintext shown once):

```sql
CREATE TABLE IF NOT EXISTS registration_tokens (
  token_id    TEXT PRIMARY KEY,           -- public id half (from MintServiceKey)
  agent_id    TEXT NOT NULL,
  hash        TEXT NOT NULL,              -- bcrypt hash of the secret half
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  revoked_at  TIMESTAMPTZ                 -- NULL ⇒ active
);
```

The token binds to one `agent_id`; the agent's `tenant` is derived from config
(the Registry already knows agent→tenant), never persisted redundantly here.

**Verification** mirrors service-key auth:

1. split the presented `id.secret` via `identity.ParseServiceKey`;
2. look up the row by `token_id`;
3. reject if no row, or `revoked_at IS NOT NULL`;
4. `identity.VerifyKey(hash, secret)` (constant-time bcrypt);
5. on success derive `agent_id` **from the row** — never from a client claim.

Failures are uniform (`401`, no body) so the response cannot distinguish "no such
token" from "wrong secret".

**Two distinct auth surfaces.** `POST /register` authenticates with its OWN
registration token (above) and is available regardless of whether identity
enforcement (OIDC/service keys) is enabled — a remote/scheduled pod must be able
to register even in open mode. **Minting** tokens (`runtimectl register …`),
however, is an admin action gated by the existing identity admin guard. Therefore
in fully-open mode (no identity configured at all) there is no admin path to mint
a registration token — handshake mode requires identity to be configured enough
to authorize an admin, exactly as `/admin/secrets` does. This is consistent: the
handshake's whole purpose is delivering brokered secrets, which already require a
configured keyring + admin.

**Management** via `runtimectl` (admin-scoped, behind the existing identity admin
guard; the same guard `/admin/secrets` uses):

- `runtimectl register mint --agent <id>` → prints the one-time plaintext token.
- `runtimectl register list` → `token_id`, `agent_id`, created, revoked status
  (never the secret).
- `runtimectl register revoke --token-id <id>` → sets `revoked_at`.

**Rotation** = mint a new token, update the pod Secret, revoke the old. Because
agentd re-fetches on every restart (§5), a revoked token takes effect at the next
pod restart — no long-lived in-memory grant.

## 5. Control-plane endpoint & data flow

New handler `controlplane/register.go`, wired into runtimed's mux beside the
existing routes.

```go
type RegisterRequest struct {
    Ordinal int `json:"ordinal"`   // from the pod's $HOSTNAME suffix
}
type RegisterResponse struct {
    Env map[string]string `json:"env"`   // the envDelta, KEY→VAL
}
```

**Flow:**

1. Extract `Authorization: Bearer <id.secret>`; missing/malformed → `401`.
2. `ParseServiceKey` → `token_id`, `secret`; look up row; revoked/absent → `401`;
   constant-time `VerifyKey` → mismatch → `401` (uniform failure, no oracle).
3. Derive `agent_id` from the row. Decode body → `ordinal`.
4. `Registry.Replica(agent_id, ordinal)` → the per-ordinal `AgentProcess`
   (already broker-stamped via `withBroker`). This call already returns `false`
   for an unknown agent OR `ordinal ∉ [0, replicaCount)`, so the bounds check is
   the existing return value — `false` → `403`, empty body. No new Registry API.
5. `ap.envDelta(ctx)` → if the broker errors (e.g. an undecryptable secret),
   **fail closed**: `503`, NO partial env (mirrors local spawn's fail-closed
   `SecretsFor`).
6. Serialize the delta into `RegisterResponse.Env`; `200`.

**Logging discipline.** The access log records `agent_id`, `tenant`, `ordinal`,
`token_id`, and status — NEVER an env value or a secret name. (Established
no-secrets-in-logs convention.)

**Testability.** The handler depends only on a small interface (token
verification + `Registry.Replica`), so it is unit-testable without Postgres using
a fake token store and a hand-built `Registry`. The token store has a real
pgstore implementation plus an in-memory store for tests.

## 6. The `buildEnv` split (server-side env safety)

`AgentProcess.buildEnv` today is `append(os.Environ(), <delta>...)`. For a local
child, inheriting runtimed's `os.Environ()` is correct (same trust domain). But
the registration response crosses the network — returning `buildEnv` verbatim
would ship runtimed's entire environment, including `RUNTIME_SECRETS_KEYS` (the
master keyring), `RUNTIME_ADMIN_BOOTSTRAP`, and OIDC client secrets. Catastrophic.

Extract the delta so the inherited-env leak is **structurally impossible**:

```go
// envDelta returns ONLY the entries buildEnv adds on top of the inherited
// process environment: the RUNTIME_* control vars, the opt-in feature vars,
// and (if a broker is set) the tenant's decrypted secrets. It never includes
// os.Environ(), so it is safe to serialize across the network to a remote agent.
func (a AgentProcess) envDelta(ctx context.Context) ([]string, error) { ... }

func (a AgentProcess) buildEnv(ctx context.Context) ([]string, error) {
    delta, err := a.envDelta(ctx)
    if err != nil {
        return nil, err
    }
    return append(os.Environ(), delta...), nil
}
```

`SpawnFunc` is unchanged (still calls `buildEnv` → identical local behavior,
guarded by existing spawn tests). The registration endpoint calls `envDelta` and
returns ONLY that.

**Empty-value shadowing entries kept.** `buildEnv` emits `RUNTIME_AGENT_MEMORY=`,
`RUNTIME_GATEWAY_URL=`, `RUNTIME_GATEWAY_KEY=` (empty) to defeat *inherited*
operator vars; these stay in the delta. On the agent side `os.Setenv(k, "")` over
the pod's own clean env is a harmless no-op, and agentd already treats empty as
unset. Keeping them makes the delta a faithful, complete description of "what this
agent should see."

**Not a full bind-address bootstrap.** The delta is config, not the listen
address: for a remote agent the control plane has no `Addr` (`AgentProcess.Addr`
is empty for a remote/scheduled `AgentProcess`), so `RUNTIME_LISTEN_ADDR` (and,
analogously, the ordinal) come back empty in the delta. agentd's fetch **skips
empty delta values**, so the StatefulSet's static `RUNTIME_LISTEN_ADDR` and the
`$HOSTNAME`-derived ordinal fallback survive. The handshake therefore bootstraps
DSN + identity + tenant + feature env + brokered secrets — the bind address and
ordinal remain pod/infra-provided.

## 7. agentd fetch path (client side)

New `cmd/agentd/register.go`, one function called at the very top of `main()`
before any `mustEnv`:

```go
// fetchRegistration, when RUNTIME_REGISTRATION_URL and _TOKEN are both set,
// POSTs to the control-plane and os.Setenv's every returned pair into this
// process's environment. A no-op when either var is unset (local spawns are
// byte-for-byte unchanged). Fails hard (log.Fatal) on any error — a pod that
// cannot fetch its config must not start with a partial environment.
func fetchRegistration() { ... }
```

`main()` becomes `fetchRegistration()` then the **unchanged** existing body
(`mustEnv("RUNTIME_PG_DSN")`, …), which now succeeds because the handshake
populated the env.

**Design properties:**

- **Fail hard, not degrade.** Unlike runtimed→agent (degrade-don't-fail), a pod
  that can't fetch its own DSN/secrets cannot function — any handshake error is
  `log.Fatal`. K8s restarts the pod (CrashLoopBackOff), the correct backpressure.
- **Ordinal from `$HOSTNAME`.** Reuses the C2 M2 derivation (`${HOSTNAME##*-}`,
  default 0). The StatefulSet sh-wrapper still exports
  `RUNTIME_AGENT_REPLICA`/`DBOS__VMID` as a pre-handshake fallback, but in
  handshake mode the fetched delta authoritatively overwrites them with the
  control-plane's validated values.
- **No new dependencies.** Plain `net/http` + `encoding/json`; bearer in the
  `Authorization` header.
- **Re-fetch on every restart** → fresh secrets/keys automatically; a rotated key
  or revoked token takes effect at next restart.

## 8. Chart integration (perAgentPods mode)

The agent StatefulSet gains two env entries:

- `RUNTIME_REGISTRATION_URL` — the control-plane Service DNS + `/register`.
- `RUNTIME_REGISTRATION_TOKEN` — from the pod's Secret (a new per-agent key,
  distinct from the M1 shared `agentAuthToken`).

When handshake mode is active, the generated `runtime.yaml` and the static chart
Secret no longer need to carry brokered secrets (or the DSN) to the pods — they
arrive through the handshake. The shared `agentAuthToken` (M1, runtimed→agent)
stays. Monolith mode renders **byte-for-byte unchanged** (no `/register` wiring;
local spawns never set the registration vars).

## 9. Testing

**Hermetic unit:**

- `envDelta` excludes `os.Environ()` — set a sentinel var, assert it is absent
  from the delta; assert the `RUNTIME_*` + opt-in + secret entries ARE present.
- `/register` handler: valid token → `200` + delta; revoked → `401`; wrong secret
  → `401`; unknown agent / out-of-range ordinal → `403`; broker error → `503`;
  and a no-secrets-in-log assertion.
- token store CRUD (mint/list/revoke, bcrypt verify) against the in-memory store.
- `ordinalFromHostname` parsing (`pod-3` → 3; no suffix → 0).

**Integration (`//go:build integration`, Postgres):** `TestRegistrationHandshake`
— mint a real token; stand up a control plane with a broker holding a tenant
secret; run a standalone agentd in handshake mode (only URL+TOKEN in its env);
assert it fetches DSN+id+tenant+secret, boots, and serves a conformant session.
Plus: a revoked token makes agentd fail to start; a token for agent A cannot read
agent B's (different tenant's) secret.

**Chart gate:** `test.sh` permutations — handshake env present in the StatefulSet;
registration Secret key wired; monolith regression (no `/register` vars).

**Mandatory gates (C2 precedent):** the final holistic review + a live kind proof
remain required — each prior K8s milestone's review/proof caught an independent
install-only bug invisible to per-task render checks.

## 10. Scope boundary (explicit non-goals for M2)

- **mTLS** mutual auth (CA, cert issuance/rotation, agentd TLS server) — deferred;
  bearer over operator-TLS covers M2.
- **Automatic token rotation/expiry** — manual mint+revoke only.
- **Changing the M1 runtimed→agent bearer** (`RUNTIME_AGENT_AUTH_TOKEN`).
- **The Go-contract follow-up-messages reconciliation** (a C1 item).

## 11. Files

| File | Change |
|------|--------|
| `internal/identity/regtoken.go` (or fold into `servicekey.go`) | mint/parse/verify reuse; thin registration-token helpers if any |
| `internal/store/*` | `registration_tokens` DDL + pgstore CRUD; in-memory store for tests |
| `controlplane/proxy.go` | extract `envDelta`; `buildEnv` = `os.Environ()` + delta |
| `controlplane/register.go` (new) | `/register` handler, request/response types, token verify + Registry.Replica + envDelta |
| `controlplane/registry.go` | no change — `Registry.Replica(id, i)` already returns `false` (broker attached) for unknown id or out-of-range ordinal |
| `cmd/runtimed/main.go` | construct token store + broker; mount `/register`; access-log fields |
| `cmd/runtimectl/main.go` | `register mint|list|revoke` subcommands |
| `cmd/agentd/register.go` (new) | `fetchRegistration`, `ordinalFromHostname` |
| `cmd/agentd/main.go` | call `fetchRegistration()` first; body unchanged |
| `deploy/charts/runtime/templates/agent-statefulset.yaml` | registration URL+token env (perAgentPods) |
| `deploy/charts/runtime/templates/secret.yaml`, `values.yaml` | per-agent registration token key |
| `deploy/charts/runtime/test.sh`, `README.md` | handshake permutations + docs |
| `ROADMAP.md` | C3 M2 DONE entry |

## 12. Conventions

- `go` CLI is ground truth (ignore IDE/LSP `replace ../harness` diagnostics).
- Integration tests: `//go:build integration`, `package test`, Postgres.app at
  `postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable`, self-clean
  DB + `dbos` schema; scripted model (no LLM key).
- gofmt-clean before commit; commit trailer
  `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`.
- No secrets/secret-names in logs or spans.
