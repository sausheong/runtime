# Identity M1 — Multi-tenant access control (design)

**Date:** 2026-06-08
**Status:** Approved design — ready for implementation planning
**Sub-project:** Identity (ROADMAP §B3), first milestone
**Supersedes:** M3's static bearer tokens (`tokens:` in `runtime.yaml`)

## Goal

Make the platform safely usable by multiple teams. Replace M3's single shared
token with **multi-tenant access control**: each team (tenant) sees and operates
only its own agents. Humans authenticate via an **external OIDC provider**;
machines authenticate via **platform-issued service keys**. All authentication
and authorization is enforced at the **control-plane edge** — agents are
unmodified and remain loopback-trusting.

This is milestone 1 of the Identity sub-project. Later milestones add secrets
brokering, fine-grained RBAC, an admin console UI, and (optionally) local
password accounts.

## Context: what exists today (M3)

- Auth is a flat list of static bearer tokens in `runtime.yaml`
  (`tokens: [{token, label}]`), matched by a plain map lookup in
  `controlplane/auth.go`'s `AuthMiddleware`. A matched token stashes an opaque
  *label* in request context, used only for log attribution.
- No users, no roles, no tenancy: any valid token reaches every agent and every
  endpoint. The label grants and restricts nothing.
- The console (`/ui`) login drops the raw token into a `runtime_token` cookie.
- Postgres + a cold-start DDL path (`store.ApplyDDLLocked`, advisory-locked)
  already exist; new identity tables are added the same way.
- The control-plane edge (`runtimed`) is already the single trust boundary:
  routing, supervision, auth, and the console all operate there; agents
  (`agentd`) trust loopback and never authenticate.

ROADMAP §B3 scopes Identity as "agent identity, secrets brokering, OAuth, RBAC,
per-user/multi-tenant" and notes it absorbs A7 (constant-time token compare +
hashing-at-rest). M1 delivers the tenancy + authN + per-agent authZ slice and
absorbs A7; the rest is deferred (see Scope boundary).

## Design decisions (settled during brainstorming)

1. **Audience:** multi-tenant — teams/orgs each own a set of agents and
   credentials, isolated from each other.
2. **Identity source:** human login delegates to an **external OIDC provider**;
   the platform does not store passwords. Machine identity is
   **platform-issued service keys**.
3. **Enforcement point:** **control-plane edge only.** Agents stay
   loopback-trusting and completely unmodified — identity is never propagated to
   agents in M1.
4. **First-milestone scope:** tenants, OIDC users, service keys, roles
   (`admin`/`operator`/`viewer`) scoped per-agent, edge enforcement,
   tenant-filtered listings, hashing-at-rest + constant-time compare.
5. **Administration:** hybrid — agent→tenant mapping in `runtime.yaml` (agents
   are static); tenants, users, role bindings, and service keys live in Postgres
   and are mutated at runtime via a `runtimectl admin` API. No dependency on
   dynamic agent deploy (A3).
6. **Code structure:** a self-contained `internal/identity` package behind an
   identity-backed edge middleware (Approach A), matching the project's
   package-per-responsibility pattern.

## Architecture

The trust boundary is unchanged: `runtimed` is the only place that
authenticates and authorizes. The request path becomes:

```
client → [runtimed edge: Authenticate → Principal in ctx
          → Authorize(agent, action) → tenant filter] → agentd (loopback, unchanged)
```

A new package **`internal/identity`** owns three concepts:

1. **`Principal`** — the resolved caller: `{TenantID, Subject, Role}`.
   `Subject` is an OIDC `sub` claim (humans) or a service-key id (machines).
   `Role` is one of `admin | operator | viewer`.
2. **`Authenticator`** — `Authenticate(r *http.Request) (Principal, error)`.
   Tries two mechanisms behind one interface (service key, then OIDC). This
   interface is the extension seam for later authn methods (local accounts,
   etc.).
3. **`Authorizer`** — `Authorize(p Principal, agentID, action) error`. A pure
   function over the tenant/role model.

`controlplane.AuthMiddleware` (the flat map lookup) is replaced by an
identity-backed middleware that stashes the `Principal` in context; the router
and the `/agents` + `/sessions` handlers consult it for tenant filtering and
action checks.

## Data model

### Agent → tenant (in `runtime.yaml`)

Agents are static, so their tenant assignment stays in config:

```yaml
agents:
  - id: support
    tenant: alpha          # NEW; an agent belongs to exactly one tenant
    name: Support Agent
    model: openai/gpt-5.4
    listen_addr: 127.0.0.1:8101
```

- A `tenant:` referencing a tenant absent from the DB is a **startup validation
  error** (fail fast, like the existing id/listen_addr uniqueness checks).
- An **absent** `tenant:` maps to a reserved `"default"` tenant, so existing
  configs keep working on upgrade.
- At startup `runtimed` resolves the config into an in-memory
  `agentID → tenantID` map used by the `Authorizer` (no per-request DB hit for
  the agent's tenant).

### Identity tables (Postgres, via `ApplyDDLLocked`)

| Table | Columns (essentials) | Purpose |
|---|---|---|
| `tenants` | `id` TEXT PK (e.g. `alpha`), `name`, `created_at` | The isolation unit. |
| `users` | `tenant_id` FK, `subject` (OIDC `sub`), `role`, `created_at`; PK `(tenant_id, subject)` | Maps an authenticated OIDC identity → tenant + role. A user belongs to exactly one tenant in M1. |
| `service_keys` | `id` TEXT PK (e.g. `svk-…`), `tenant_id` FK, `key_hash`, `role`, `label`, `created_at`, `revoked_at` (nullable) | Machine credentials. Only the hash is stored; plaintext is shown once at creation. |

- **Roles** are a fixed enum enforced by a CHECK constraint:
  `admin | operator | viewer`. No custom roles or per-action grants in M1.
- `service_keys.revoked_at` makes revocation **instant** (a query filter), not a
  restart.
- OIDC users are *provisioned* in runtime (a row binds `subject` → tenant +
  role). The IdP proves *who* the caller is; runtime decides *what* they can do
  — authorization stays on-prem even with external authentication.

**YAGNI for M1:** no cross-tenant membership — a user/key belongs to exactly one
tenant. Multi-tenant-per-identity adds a join table and "which tenant am I
acting as?" ambiguity not yet worth it.

## AuthN & AuthZ mechanics

### Authentication — one `Bearer` header, two mechanisms (tried in order)

The caller sends `Authorization: Bearer <credential>` (or the `runtime_token`
cookie for the console / EventSource, unchanged). The `Authenticator`
distinguishes by shape:

1. **Service key** — credentials with the `svk-` prefix. Parse `svk-<id>`, look
   up the row; if `revoked_at IS NULL`, **constant-time-compare** the presented
   secret against `key_hash`. Match → `Principal{tenant, subject: "svk-<id>",
   role}`. Fast path: one indexed lookup + one compare, no external dependency.
2. **OIDC token** — anything else is treated as a JWT. Verify the signature
   against the issuer's JWKS (cached; refreshed on `kid` miss), check
   `iss`/`aud`/`exp`. Extract `sub`, look up the `users` row →
   `Principal{tenant, subject, role}`.

Both reduce to the same `Principal`; everything downstream is
mechanism-agnostic.

### Authorization — `Authorize(principal, agentID, action)`

Two checks, in order:

1. **Tenant isolation:** the agent's tenant (from the in-memory map) must equal
   `principal.TenantID`. Mismatch → **404** (a tenant must not learn another
   tenant's agents even exist).
2. **Role / action matrix:**

| Action (HTTP) | viewer | operator | admin |
|---|:--:|:--:|:--:|
| `GET /agents`, `GET /sessions`, `GET /sessions/{id}`, stream | ✅ | ✅ | ✅ |
| `POST /sessions` (invoke) | ❌ | ✅ | ✅ |
| `runtimectl admin …` (manage identity) | ❌ | ❌ | ✅ |

The action is derived from method+path at the edge (e.g. `POST .../sessions` →
`invoke`). `admin` manages identity **only within its own tenant** (a
tenant-admin, not a platform superuser).

### Tenant-filtered listing

`GET /agents` returns only agents whose tenant matches the principal, so the
fleet view is scoped (not just per-agent calls). Same for any cross-agent
session view.

### Bootstrapping (chicken-and-egg)

Admin actions require an admin, so the first admin is created via a `runtimed`
env var **`RUNTIME_ADMIN_BOOTSTRAP`** — a one-time platform-superuser service
key (read from env, never stored) that can call the admin API to create the
first tenant + admin, then is removed from config. This is the documented
break-glass path and the only superuser-level credential.

### Open mode (backward-compat)

When **no identity of any kind is configured** — no OIDC issuer, no service
keys, no users, AND no legacy `tokens:` — the edge runs in **open mode** exactly
like M3 today: every request passes, with a logged startup warning. Auth turns
on the moment any identity is configured, including the legacy `tokens:` compat
path (a config with only `tokens:` is **enforced**, not open — each token maps to
a `default`-tenant superuser per Migration below). This keeps local development
friction-free and makes the upgrade backward-compatible.

## Admin API, CLI & console

### Admin API (new control-plane endpoints; `admin`-only; tenant-scoped)

| Method & path | Action |
|---|---|
| `POST /admin/tenants` | Create a tenant (platform-superuser via bootstrap key). |
| `GET /admin/tenants` | List tenants (superuser: all; tenant-admin: own only). |
| `POST /admin/users` | Provision `{subject, role}` in the caller's tenant. |
| `DELETE /admin/users/{subject}` | De-provision a user. |
| `GET /admin/users` | List users in the caller's tenant. |
| `POST /admin/keys` | Mint a service key `{label, role}`; response includes the **plaintext once**. |
| `DELETE /admin/keys/{id}` | Revoke (sets `revoked_at`) — instant. |
| `GET /admin/keys` | List keys in the tenant (id, label, role, created/revoked — never the secret). |

All `/admin/*` routes pass through the identity middleware; handlers additionally
assert `role == admin` and scope every query to `principal.TenantID` (a
tenant-admin cannot touch another tenant). Tenant *creation* is the one
superuser-only operation in M1.

### CLI (`runtimectl admin …`)

Thin wrappers over the API, matching existing CLI style (`RUNTIME_TOKEN` carries
the caller's credential):

```bash
runtimectl admin tenant create alpha --name "Team Alpha"
runtimectl admin user add alice@corp --tenant alpha --role operator   # subject = OIDC sub/email claim
runtimectl admin key create --tenant alpha --role operator --label ci
#   → svk-7f3a…  (shown once — store it now)
runtimectl admin key revoke svk-7f3a…
runtimectl admin user ls --tenant alpha
```

Existing commands gain tenant-awareness for free: `runtimectl agents` /
`sessions` already hit the now-filtered endpoints, so they show only the
caller's tenant.

### Console (`/ui`)

- **Login changes** from "paste a token" to OIDC: `/ui/login` redirects to the
  IdP; the callback verifies the authorization code and sets the `runtime_token`
  cookie to the **validated ID token** itself. Every subsequent `/ui` request is
  verified by the same `Authenticator` — **no server-side session store**.
  Expiry equals the token's `exp` (the user re-logs in). When in open mode or a
  service-key-only deployment, the paste-token login is retained as a fallback.
- **Views become tenant-scoped automatically** (same filtered endpoints): a
  logged-in user sees only their tenant's agents/sessions.
- The console **stays read-only** — no admin management UI in M1 (deferred to
  the "M2 console UX" item). Admin is CLI/API only for now.

## Error handling

- **401 Unauthorized** — no / malformed / expired / bad-signature credential.
  Generic body (never leak *why*).
- **403 Forbidden** — authenticated but the action isn't permitted for the role
  (e.g. viewer doing `POST /sessions`), OR a validly-signed OIDC token with no
  provisioned `users` row (authenticated, not provisioned).
- **404 Not Found** — agent exists but in another tenant (existence hidden), and
  genuinely-missing agents — indistinguishable on purpose.
- `/healthz` stays auth-exempt; `/ui/login` and `/ui/static/*` stay exempt.
- **JWKS unreachable** → `503` for OIDC requests (fail closed), logged.
  Service-key auth still works (no external dependency), so a key-using tenant
  is unaffected by IdP downtime.

## Testing

Hermetic-first, matching the project's `go test ./...` discipline:

- **`internal/identity` unit tests:** the `Authorizer` matrix (every role ×
  action × same/other tenant); service-key hashing + constant-time compare;
  OIDC claim→principal mapping using a **test-signed JWT against an in-test
  JWKS** (no network).
- **`controlplane` middleware tests:** 401/403/404 paths, open-mode
  passthrough, tenant-filtered `/agents`.
- **Store tests** for the identity tables (revocation flips access; CHECK
  rejects bad roles) — these need Postgres, so `//go:build integration` like the
  existing store tests, self-cleaning their tables.
- **One integration test** proving the headline: two tenants, each with an agent
  + an operator key; tenant A's key can invoke A and gets 404 on B; a viewer key
  gets 403 on invoke.

## Migration / backward-compat

- Absent `tenant:` on an agent → reserved `default` tenant.
- No OIDC + no keys + no users → **open mode** (M3 behavior). An existing
  deployment upgrades with zero config change and identical behavior until the
  operator opts in.
- M3's flat `tokens:` in `runtime.yaml` is **kept working** as a deprecated path
  mapping each token to a superuser-equivalent in `default` (so nobody is locked
  out on upgrade), with a deprecation warning. Removed in a later milestone once
  service keys are adopted.

## Scope boundary — what M1 is NOT

- ❌ **Secrets brokering** (per-tenant provider keys injected into agents) — next
  milestone.
- ❌ **Fine-grained / custom RBAC** beyond the three fixed roles.
- ❌ **Cross-tenant users**, user self-service, or an admin console UI.
- ❌ **Identity propagated to agents** (enforcement is edge-only; agents stay
  unmodified).
- ❌ **Local password accounts** (OIDC + service keys only; the `Authenticator`
  interface leaves room to add them later).

## New / changed files (Approach A)

- `internal/identity/principal.go` — `Principal`, `Role`, action constants.
- `internal/identity/authenticator.go` — the `Authenticator` interface +
  the combined service-key-then-OIDC implementation.
- `internal/identity/oidc.go` — JWKS-verifying OIDC verifier (claims → subject).
- `internal/identity/servicekey.go` — key minting, hashing, constant-time check.
- `internal/identity/authorizer.go` — `Authorize(principal, agentID, action)`.
- `internal/identity/store.go` — Postgres CRUD for tenants/users/service_keys.
- `controlplane/identity_middleware.go` — replaces `auth.go`'s map path;
  produces a `Principal` in context and enforces authz + tenant filtering.
- `controlplane/admin.go` — the `/admin/*` endpoints.
- `internal/config/config.go` — add the `tenant:` field + startup validation;
  keep `tokens:` as a deprecated compat path.
- `cmd/runtimectl` — `admin` subcommands.
- `console/` — OIDC login + callback; cookie carries the validated token.
- New dependency: an OIDC/JWT verification library (e.g.
  `github.com/coreos/go-oidc` + `golang.org/x/oauth2`).

## How this maps to the ROADMAP

Completes the tenancy + authN + per-agent authZ slice of §B3 and absorbs A7
(constant-time compare + hashing-at-rest). Subsequent Identity milestones:
secrets brokering, fine-grained RBAC, admin console UI, optional local accounts.
