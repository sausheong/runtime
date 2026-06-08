# Identity M2 — Secrets Brokering Design

**Date:** 2026-06-09
**Sub-project:** B3 Identity, second milestone
**Status:** Approved design, pre-implementation
**Builds on:** Identity M1 (`docs/superpowers/specs/2026-06-08-identity-m1-design.md`)

---

## Goal

Give each tenant its own provider credentials (e.g. `OPENAI_API_KEY`),
encrypted at rest, and inject them as environment variables into that tenant's
agent subprocesses at spawn time — with **agents completely unmodified**.

This completes the "usable by others" arc started in M1: M1 lets a stranger
authenticate and gives per-agent RBAC; M2 lets that stranger bring and run their
own provider keys instead of sharing the operator's. Today every agent
subprocess inherits runtimed's entire environment (`SpawnFunc` does
`append(os.Environ(), …)`), so the operator's single `OPENAI_API_KEY` is shared
by all tenants. M2 replaces that with per-tenant brokered secrets.

## Non-goals (explicit scope boundaries)

- **No key rotation.** The master key is fixed for a deployment; rotating it
  (re-encrypting all rows) is future work, noted in Limitations.
- **No per-tenant or per-agent encryption keys.** One operator master key.
- **No read-back of secret values.** Write-only API.
- **No live reload.** Secret changes apply on the agent's next spawn/restart.
- **No per-agent secret scope.** Secrets are per-tenant; every agent in a tenant
  receives the tenant's full secret set.
- **No structured/typed provider records.** A secret is a generic
  `(name, value)` env var, provider-agnostic.
- **No external KMS/Vault.** Pure stdlib AES-GCM, on-prem, self-contained.

---

## Decisions (locked during brainstorming)

| Decision | Choice |
|---|---|
| Secret model | Generic named env var: `(tenant, name, value)` injected as env var `name` |
| Scope | Per-tenant; all of a tenant's agents get the tenant's full set |
| Encryption at rest | AES-256-GCM, random 12-byte nonce per value, stdlib only |
| Master key source | `RUNTIME_SECRETS_KEY` env, base64 of 32 bytes |
| API read-back | Write-only; list returns names + metadata, never values |
| Injection | `append(os.Environ(), RUNTIME_*…, tenantSecrets…)` — tenant secrets shadow inherited env |
| Refresh | Next-spawn-only (documented); no hot reload |
| Authorization | `admin` role, tenant-scoped (mirrors users/keys) |
| CLI | `runtimectl admin secret set/ls/rm`, `--tenant` for superusers |
| Master key unset | Feature disabled, platform runs normally (back-compat) |
| Master key malformed | runtimed fails to start (operator error ≠ deliberate off switch) |
| Decryption failure at spawn | Fail closed — spawn fails, agent does not start with partial secrets |

---

## Architecture & components

The registry/proxy depend only on a one-method `SecretBroker` interface; all
crypto and storage stay inside `internal/identity`. The spawn path is testable
with a fake broker.

| Unit | Responsibility |
|---|---|
| `internal/identity/crypto.go` (new) | `Cipher` type. `NewCipher(masterKey []byte) (*Cipher, error)` validates 32-byte length. `Seal(plaintext []byte) ([]byte, error)` / `Open(ciphertext []byte) ([]byte, error)` using AES-256-GCM with a random 12-byte nonce prepended to the ciphertext. Pure stdlib (`crypto/aes`, `crypto/cipher`, `crypto/rand`). |
| `internal/identity/secrets.go` (new) | Secret persistence methods on the existing `*Store`: `PutSecret(ctx, tenant, name string, valueEnc []byte) error` (UPSERT), `ListSecretNames(ctx, tenant string) ([]SecretMeta, error)`, `DeleteSecret(ctx, tenant, name string) error`, `LoadSecrets(ctx, tenant string) ([]EncryptedSecret, error)`. The store moves **ciphertext only** — it never holds the cipher. |
| `internal/identity/schema.sql` (extend) | `secrets` table (see Data model). |
| `internal/identity/broker.go` (new) | `Broker{store, cipher *Cipher}`. Read side (control plane): `SecretsFor(ctx, tenant) (map[string]string, error)` loads encrypted rows, `Open`s each, returns name→plaintext. Write side (admin API): `SetSecret(ctx, tenant, name, plaintext)` `Seal`s then `PutSecret`s; `ListSecretNames` and `DeleteSecret` pass through. The broker is the single place crypto meets storage; it is unit-testable against a fake store interface (no Postgres). The control plane sees only the `SecretBroker` (read) and `SecretAdmin` (write) interfaces it satisfies. |
| `controlplane/registry.go` (change) | `Registry` gains an optional `broker SecretBroker` field (interface: `SecretsFor(ctx, tenant) (map[string]string, error)`); nil ⇒ no brokering. A `WithBroker` option or constructor param sets it. The spawn path reaches it. |
| `controlplane/proxy.go` (change) | `AgentProcess` gains an unexported `broker SecretBroker` (set by the registry when building the process). Extract `buildEnv(ctx) ([]string, error)` from `SpawnFunc`: it appends `RUNTIME_*` then, if `broker != nil`, the resolved tenant secrets (last ⇒ shadow inherited env). nil broker / empty map ⇒ byte-identical to today. Decryption error ⇒ returned, spawn fails closed. |
| `controlplane/admin.go` (extend) | `/admin/secrets` `POST`/`GET`/`DELETE`. Admin-role via existing `requireAdmin`; tenant via existing `effectiveTenant` (superuser may target a validated tenant; others pinned). The handlers take a `SecretAdmin` dependency — a small interface (`SetSecret(ctx, tenant, name, plaintext)`, `ListSecretNames`, `DeleteSecret`) implemented by the `Broker` (it seals before persisting). `RegisterAdmin` receives it as a separate, optional argument; when it is nil (no master key configured), the secret handlers return `503 secrets not configured`. The plaintext→ciphertext sealing lives in the broker, never in the handler or the store. |
| `cmd/runtimed/main.go` (change) | Read `RUNTIME_SECRETS_KEY`. Unset ⇒ broker nil, log "secrets brokering disabled". Set+valid ⇒ build `Cipher`→`Broker`, pass to registry + admin wiring. Set+malformed ⇒ fatal. |
| `cmd/runtimectl/main.go` (extend) | `admin secret set <name> <value>`, `admin secret ls`, `admin secret rm <name>`; `--tenant` honored for superusers; empty name/value guarded client-side too. |

### The `SecretBroker` seam

```go
// controlplane
type SecretBroker interface {
    SecretsFor(ctx context.Context, tenant string) (map[string]string, error)
}
```

`internal/identity.Broker` implements it. The registry holds it; the proxy calls
it at spawn. A fake implementation drives the proxy/registry unit tests.

---

## Data model

Extends `internal/identity/schema.sql`:

```sql
CREATE TABLE IF NOT EXISTS secrets (
    tenant_id  TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    value_enc  BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, name)
);
```

- `value_enc` is `nonce || AES-256-GCM ciphertext`. Plaintext is never stored.
- `ON DELETE CASCADE`: dropping a tenant drops its secrets (mirrors users/keys).
- PK `(tenant_id, name)` ⇒ one value per name per tenant; UPSERT on conflict.

Read models:

```go
type SecretMeta struct {           // for list — NO value
    Name      string
    CreatedAt time.Time
    UpdatedAt time.Time
}
type EncryptedSecret struct {      // broker-internal — ciphertext only
    Name     string
    ValueEnc []byte
}
```

---

## Data flow

### Write (set a secret)

```
runtimectl admin secret set OPENAI_API_KEY sk-xxx [--tenant alpha]
  → POST /admin/secrets {name, value, tenant?}
    → IdentityMiddleware: authn → authz (admin role)
    → requireAdmin + effectiveTenant(p, body.Tenant)   # superuser may target; others pinned
    → if no cipher: 503 "secrets not configured" (before touching store)
    → validate name ^[A-Za-z_][A-Za-z0-9_]*$ ; value non-empty   # else 400
    → cipher.Seal(value)            # AES-256-GCM, random nonce prepended
    → Store.PutSecret(tenant, name, ciphertext)   # UPSERT (tenant_id,name)
    → 200 {name}                    # value never echoed
```

### List

```
GET /admin/secrets [?tenant=alpha]
  → admin + tenant-scoped
  → Store.ListSecretNames(tenant) → [{name, created_at, updated_at}]   # no values
```

### Read / inject (spawn time)

```
Supervisor (re)starts agent X (tenant=alpha)
  → registry builds AgentProcess (carries Tenant + broker)
  → SpawnFunc → buildEnv:
      secrets := broker.SecretsFor(ctx, "alpha")   # {} if nil broker / no rows
        → Store.LoadSecrets → encrypted rows
        → cipher.Open(each)                          # decrypt; error ⇒ fail closed
      env = append(os.Environ(),
          RUNTIME_PG_DSN, RUNTIME_LISTEN_ADDR, RUNTIME_AGENT_ID, RUNTIME_AGENT_KIND,
          "OPENAI_API_KEY=sk-xxx", …)                # tenant secrets LAST → shadow
  → agent process starts; os.Getenv("OPENAI_API_KEY") unchanged
```

**Plaintext lifetime:** exists transiently only in (1) the POST body and (2) the
child process environment at spawn. Never logged, never returned by GET, never
held in the registry.

**Fail-closed:** decryption happens once per spawn, synchronously, before
`cmd.Start()`. A failure fails the spawn (logged with tenant + secret name, never
value); the supervisor backs off and retries. An agent never starts with a
partial secret set.

---

## Error handling & edge cases

| Situation | Behavior |
|---|---|
| `RUNTIME_SECRETS_KEY` unset | Broker nil. `SpawnFunc` skips brokering (today's passthrough). `/admin/secrets` (all verbs) → `503 {"error":"secrets not configured"}`. Startup log: "secrets brokering disabled: RUNTIME_SECRETS_KEY not set". |
| `RUNTIME_SECRETS_KEY` malformed (not base64 / ≠32 bytes) | runtimed **fails to start**, clear fatal error. |
| `cipher.Open` fails at spawn (corrupt row / wrong key) | Fail closed: `SecretsFor` returns error, spawn reports it, supervisor backs off + retries. Agent does not start. Logged with tenant + name only. |
| Empty `name` or empty `value` on set | `400`. To remove a secret use `DELETE`, not set-empty. |
| `name` not a valid env-var identifier | `400` (`^[A-Za-z_][A-Za-z0-9_]*$`) — prevents `=`/newline smuggling into child env. |
| Set/list/delete for nonexistent tenant | `effectiveTenant` validates: non-superuser pinned to own tenant; superuser targeting missing tenant → `400`/`404` (reuses the M1 validation that prevents the FK-500 bootstrap bug class). |
| Tenant deleted | `ON DELETE CASCADE` removes its secrets. |
| Value with odd bytes (multiline PEM, binary) | Stored encrypted, injected as-is. Only the **name** is identifier-validated; the value is opaque. |
| Concurrent set of same `(tenant,name)` | UPSERT (`ON CONFLICT … DO UPDATE`), last writer wins, `updated_at` bumped. No partial state. |
| Non-admin hits `/admin/secrets` | `403` via `requireAdmin`. Cross-tenant naming blocked unless superuser. |

**Security model:** the master key lives in runtimed's environment, the same
trust level as the Postgres DSN — operator-managed. TLS termination in front of
runtimed (protecting the POST body in transit) is the operator's responsibility,
as it already is for service keys and tokens. Losing the master key makes
existing ciphertext unrecoverable (documented). No secret value is ever passed to
`slog` or returned in a response body; the access log records status/subject/
tenant, not bodies.

---

## Testing strategy

### Unit (hermetic, no DB)

`internal/identity/crypto_test.go`:
- `Seal`→`Open` round-trips arbitrary bytes (short, multiline PEM, binary).
- `NewCipher` rejects keys ≠ 32 bytes.
- Same plaintext ⇒ different ciphertext across calls (random nonce).
- `Open` fails on tampered/truncated ciphertext and on a wrong key.

`internal/identity/broker_test.go`:
- `Broker.SecretsFor` over a fake store returns name→plaintext.
- `Broker.SetSecret` seals then persists (assert the fake store received
  ciphertext, not plaintext).
- A bad row surfaces the `Open` error (fail-closed), not a silent drop.
- Empty source ⇒ empty map; nil-safe.

`controlplane/proxy_test.go` (extend):
- `buildEnv` with a fake `SecretBroker` puts `name=value` **after** the
  `RUNTIME_*` vars (assert ordering ⇒ shadowing).
- nil broker ⇒ env byte-identical to today (back-compat regression guard).

`controlplane/admin_test.go` (extend or via integration):
- `POST /admin/secrets` non-admin → 403; no cipher → 503; empty name/value →
  400; invalid env-name → 400.
- `GET` returns names+meta and the value bytes are **absent** from the response.

### Integration (`//go:build integration`, Postgres at the standard DSN)

`internal/identity/secrets_store_test.go`:
- `PutSecret`/`ListSecretNames`/`DeleteSecret`/`LoadSecrets` against real
  Postgres; UPSERT bumps `updated_at`; `ON DELETE CASCADE` removes secrets when
  the tenant is dropped; list never selects `value_enc`.
- Self-cleans tables + `dbos` schema; `t.Cleanup` drops the `secrets` table so it
  doesn't pollute sibling open-mode tests (the M1 test-pollution lesson).

`test/secrets_e2e_test.go` — **headline end-to-end:**
- Control plane with a real `Cipher`+`Broker`; tenant `alpha` with secret
  `OPENAI_API_KEY=sk-test`, tenant `beta` with its own value, plus a tenant with
  no secrets. Agents spawn via the generalized `command:` path running a tiny
  stub that writes its environment to a file.
- Assert: alpha's process saw `sk-test`; beta's saw *its* value (isolation); the
  no-secret tenant fell back to the inherited operator env. Proves the whole
  chain: API set → encrypted row → spawn-time decrypt → correct per-tenant env.

### Deliberately not tested

Key rotation (out of scope), live reload (not built), TLS termination (operator
concern).

---

## Backward compatibility

Fully additive. A deployment that sets no `RUNTIME_SECRETS_KEY` behaves exactly
as today: the broker is nil, subprocesses inherit the operator env, the secrets
API returns 503. Open mode and existing single-tenant deployments are untouched.
Symmetric with how absent OIDC ⇒ open mode in M1.

---

## Limitations (record in README + ROADMAP)

- **No key rotation in M2.** Rotating `RUNTIME_SECRETS_KEY` requires a future
  re-encrypt migration (decrypt-with-old, encrypt-with-new across all rows).
- **One master key per deployment**, shared across tenants. Per-tenant keys are
  future work.
- **Spawn-time refresh only** — a secret change needs an agent restart to apply.
  A `runtimectl agent restart` is a candidate follow-up (also serves A1/A3 debt).
- **POST body carries plaintext** — relies on operator TLS in front of runtimed,
  as service keys/tokens already do.

---

## Documentation updates on completion

- README "Authentication & multi-tenancy" → add a Secrets sub-section (env var,
  `runtimectl admin secret …`, write-only, restart-to-apply, `RUNTIME_SECRETS_KEY`).
- README env-var table → `RUNTIME_SECRETS_KEY`.
- README CLI table → `admin secret set/ls/rm`.
- ROADMAP §B3 → mark secrets brokering done; move remaining Identity items down.
- `docs/images/project-layout.mmd` → note secrets in the `identity/` description.
- Project memory `runtime-platform-project.md` → Identity M2 paragraph.
