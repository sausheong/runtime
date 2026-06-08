# Identity M3 — Secrets Key Rotation Design

**Date:** 2026-06-09
**Sub-project:** B3 Identity, third milestone
**Status:** Approved design, pre-implementation
**Builds on:** Identity M2 (`docs/superpowers/specs/2026-06-09-identity-m2-secrets-brokering-design.md`)

---

## Goal

Let an operator rotate the secrets master key without losing data: introduce a
**keyring** (multiple keys, one designated primary), make every ciphertext
self-describing so rows sealed under different keys coexist, and provide an
explicit, re-runnable re-encrypt pass that migrates the backlog onto the primary
so retired keys can be dropped. Fold in **AAD binding** of each secret's
`(tenant, name)` so a row's ciphertext is cryptographically pinned to its row
(defeats DB-level row swaps) — rotation is the one time we re-seal every row, so
this is essentially free.

This closes the gap M2 explicitly deferred ("No key rotation … rotating the
master key requires a future re-encrypt migration") and the optional AAD-binding
hardening item the M2 spec named under Limitations.

## Non-goals (explicit scope boundaries)

- **No per-tenant or per-agent keys.** One operator-managed keyring shared across
  tenants. (The self-describing-row format generalizes to per-tenant keys later,
  but that is future work.)
- **No external KMS/Vault.** Pure stdlib AES-256-GCM, on-prem, self-contained
  (unchanged from M2).
- **No automatic rotation.** No re-encrypt at startup, no lazy on-access re-seal.
  Rotation is a deliberate operator action via `runtimectl`.
- **No live reload.** Key/keyring changes take effect at runtimed restart; row
  migration is the explicit `rotate` pass. Secrets still apply to agents at next
  spawn (M2 behavior).
- **No read-back of secret values.** The write-only API model from M2 is kept;
  rotation never returns plaintext.
- **No schema change to the `secrets` table.** The key identifier lives in the
  ciphertext blob (per the chosen design), not in a SQL column.
- **No keyring file.** Keys come from environment variables, at the same trust
  level as `RUNTIME_PG_DSN` (unchanged from M2).

---

## Decisions (locked during brainstorming)

| Decision | Choice |
|---|---|
| How a row tracks its key | **Key-ID prefix in the blob** — self-describing rows, crash-safe mixed-state rotation |
| Keyring config | **Single structured env var** `RUNTIME_SECRETS_KEYS="id:b64,id:b64"` + `RUNTIME_SECRETS_PRIMARY=id`; lone `RUNTIME_SECRETS_KEY` → keyring `{v1:key}` (back-compat) |
| Rotation trigger | **Explicit CLI** `runtimectl admin secret rotate [--tenant t]` → `POST /admin/secrets/rotate`; observable, re-runnable |
| AAD binding | **Bind `(tenant, name)` now**, during the same re-encrypt pass; all new seals bind it |
| Legacy-row distinction | **Magic version byte** `0x01` at the front of new blobs; version-less blobs = legacy M2 rows |
| Bare `rotate` as superuser | Rotates **all tenants**; `--tenant` targets one; non-superuser admin rotates own tenant only |
| Rotate hits a bad row | **Continue** the pass, count it as failed, log name (never value); CLI exits non-zero |
| Config errors (bad keyring/primary/legacy mismatch) | **Fatal at startup** (operator error, mirrors M2 malformed-key) |
| Data errors at runtime (bad row, unknown key id, AAD mismatch) | **Fail closed** (spawn fails for that tenant; runtimed stays up) |

---

## Blob format

New-format ciphertext is self-describing and versioned:

```
┌──────┬───────────┬───────────────┬───────────┬──────────────────┐
│ 0x01 │ keyIDLen  │ keyID         │ nonce     │ GCM ciphertext   │
│ 1 B  │ 1 B       │ keyIDLen B    │ 12 B      │ rest             │
└──────┴───────────┴───────────────┴───────────┴──────────────────┘
```

- **`0x01`** — format version. A blob whose first byte is **not** `0x01` is a
  legacy M2 row (`nonce || ciphertext`, nil AAD) and is opened on the legacy
  path. Future format changes bump this byte.
- **`keyIDLen`** — one byte, so a key ID is 1–255 bytes of UTF-8 (operator
  strings like `v1`, `v2`, `2026q2`). `keyIDLen == 0` is a corrupt blob.
- **`keyID`** — names the keyring entry that sealed (and must open) this row.
- **`nonce`** — fresh random 12-byte GCM nonce per seal.
- **AAD** for new-format seals/opens is `tenant + "\x00" + name`. Legacy opens
  pass nil AAD. AAD is authenticated, not encrypted; a mismatch fails `Open`.

A legacy M2 blob is `nonce(12) || GCM(ciphertext)` with nil AAD — the absence of
the `0x01` prefix is what marks it. (A legacy GCM ciphertext's first byte is the
first byte of the random nonce; the format treats *any* non-`0x01` first byte as
legacy. This is safe because new blobs are always written with the `0x01`
prefix, and the only non-prefixed blobs in existence are genuine M2 rows.)

---

## Architecture & components

All crypto/keyring/rotation logic stays inside `internal/identity` behind the
existing `controlplane.SecretBroker` (read) and `controlplane.SecretAdmin`
(write) seams. The control-plane spawn path and the `secrets` table shape are
unchanged.

| Unit | Change | Responsibility |
|---|---|---|
| `internal/identity/crypto.go` | **Rewrite `Cipher` → `Keyring`** | `Keyring{keys map[string]cipher.AEAD; primaryID string; legacyID string}`. `Seal(tenant, name string, plaintext []byte) ([]byte, error)` → new-format blob under the primary key, AAD = `tenant\x00name`. `Open(tenant, name string, blob []byte) ([]byte, error)` → if `blob[0]==0x01`, parse keyID, select key (missing ⇒ error), AAD-bound open; else legacy path (legacy key, nil AAD). `PrimaryID() string`. Pure stdlib (`crypto/aes`, `crypto/cipher`, `crypto/rand`). |
| `internal/identity/keyring.go` (new) | Config parsing | `ParseKeyring(keysEnv, primaryEnv, legacyKeyEnv string) (*Keyring, error)`. Parses `id:b64,id:b64`; validates each key is 32 bytes and IDs are unique; requires `primaryEnv` ∈ keys. Lone `legacyKeyEnv` (old `RUNTIME_SECRETS_KEY`) with empty `keysEnv` → keyring `{v1:key}`, primary `v1`, legacy `v1`. With `keysEnv` set, a non-empty `legacyKeyEnv` must equal one keyring entry's bytes (names the legacy-decrypt key); else error. All-empty → returns `(nil, nil)` sentinel (feature disabled). |
| `internal/identity/broker.go` | Thread `(tenant,name)`; add `Rotate` | `SecretsFor`/`SetSecret` pass tenant+name into `Open`/`Seal`. New `Rotate(ctx, tenant string) (RotateStats, error)`: load all rows, `Open` each (old key/legacy), `Seal` under primary+AAD, `PutSecret` back; per-row failure isolated and counted. `RotateAll(ctx, tenants []string)` loops `Rotate`. |
| `internal/identity/secrets.go` | (no shape change) | `LoadSecrets`/`PutSecret`/`ListSecretNames`/`DeleteSecret` unchanged; `Rotate` reuses `LoadSecrets` + `PutSecret`. No new column — the key ID is in the blob. |
| `controlplane/admin.go` | Extend `SecretAdmin` + new route | `SecretAdmin` gains `RotateSecrets(ctx, tenant string) (identity.RotateStats, error)`. The superuser all-tenants path enumerates tenants via the existing `AdminStore.ListTenants` (already on `*identity.Store`, returns `[]identity.TenantRow`) — add `ListTenants(ctx) ([]identity.TenantRow, error)` to the `AdminStore` interface. New `POST /admin/secrets/rotate` (admin role; `effectiveTenant`; superuser with no tenant ⇒ all tenants). 403/503/400 guards mirror the existing secret handlers. |
| `cmd/runtimectl/main.go` | New verb | `admin secret rotate [--tenant t]`; prints `RotateStats`; non-zero exit if any row failed. |
| `cmd/runtimed/main.go` | Keyring construction | `buildSecretBroker` reads `RUNTIME_SECRETS_KEYS` / `RUNTIME_SECRETS_PRIMARY` / `RUNTIME_SECRETS_KEY` and calls `ParseKeyring`; nil ⇒ disabled (log), error ⇒ fatal, ok ⇒ `NewBroker(idStore, keyring)` + log key count & primary. |

### Read models / types

```go
// RotateStats reports the outcome of a re-encrypt pass (no secret values).
type RotateStats struct {
    Tenant         string `json:"tenant"`
    Total          int    `json:"total"`
    Rotated        int    `json:"rotated"`
    Failed         int    `json:"failed"`
}
```

`NewBroker` keeps its signature shape but now takes a `*Keyring` instead of a
`*Cipher` (the broker field type changes; the control-plane seams are unchanged
because they never referenced the concrete cipher type).

---

## Data model

**No change to the `secrets` table.** The key identifier and format version live
in `value_enc` (the blob). Existing M2 rows remain valid and readable via the
legacy path until a `rotate` pass migrates them. `ON DELETE CASCADE`, the
`(tenant_id, name)` PK, and the ciphertext-only store methods are all unchanged.

---

## Data flow

### Startup (keyring construction)

```
runtimed boot → buildSecretBroker(idStore) → ParseKeyring(KEYS, PRIMARY, KEY):
  KEYS=="" && KEY==""                  → (nil,nil)  → broker nil, "secrets brokering disabled"
  KEYS=="" && KEY set (legacy)         → {v1:key}, primary=v1, legacy=v1
                                          "secrets brokering enabled (legacy single-key)"
  KEYS set                             → parse id:b64 pairs; require PRIMARY ∈ keys
                                          KEY set ⇒ its bytes must equal one entry (legacy-decrypt key)
                                          "secrets brokering enabled (keyring: N keys, primary=…)"
  any parse/size/dup/primary/legacy error → FATAL (os.Exit(1))
```

The **legacy-decrypt key** opens version-less M2 blobs. By default it is the
primary; if the operator's old `RUNTIME_SECRETS_KEY` differs from a new primary,
they keep both set so old rows still open until `rotate` migrates them.

### Write (set) — unchanged surface, new seal

```
runtimectl admin secret set OPENAI_API_KEY sk-xxx [--tenant alpha]
 → POST /admin/secrets → broker.SetSecret(ctx,"alpha","OPENAI_API_KEY","sk-xxx")
   → keyring.Seal("alpha","OPENAI_API_KEY","sk-xxx")
       blob = 0x01 | len(primaryID) | primaryID | nonce | GCM(primary,nonce,pt, aad="alpha\x00OPENAI_API_KEY")
   → Store.PutSecret(alpha,OPENAI_API_KEY,blob)   # UPSERT, always primary
```

### Read / inject (spawn) — version-aware open

```
spawn agent (tenant=alpha) → broker.SecretsFor(ctx,"alpha")
 → Store.LoadSecrets(alpha) → [(name,blob),…]
 → keyring.Open("alpha",name,blob) per row:
     blob[0]==0x01 → keyID from blob; key=keyring[keyID] (missing ⇒ error)
                      aad="alpha\x00"+name; GCM_open
     else          → key=legacyKey (missing ⇒ error); aad=nil; GCM_open
   any open error ⇒ abort whole tenant resolution (FAIL-CLOSED, M2 invariant)
 → name→plaintext → injected LAST in spawn env (shadows operator env, M2)
```

Mixed-state is transparent: a tenant may hold legacy + `v1` + `v2` rows
simultaneously; each opens by its own descriptor.

### Rotate (the new pass)

```
runtimectl admin secret rotate [--tenant alpha]
 → POST /admin/secrets/rotate → effectiveTenant:
     non-superuser            → broker.Rotate(ctx, p.TenantID)
     superuser + --tenant t   → broker.Rotate(ctx, t)        (t must exist)
     superuser, no --tenant   → broker.RotateAll(ctx, ids(store.ListTenants(ctx)))
   Rotate(ctx, tenant):
     for each (name,blob) in LoadSecrets(tenant):
       pt  := keyring.Open(tenant,name,blob)   # old key / legacy
       new := keyring.Seal(tenant,name,pt)     # primary + AAD, 0x01 format
       Store.PutSecret(tenant,name,new)        # overwrite in place (atomic per row)
       ok ⇒ Rotated++ ; open/seal/store err ⇒ Failed++ (log name+tenant, never value)
     → RotateStats{Tenant,Total,Rotated,Failed}
 → CLI prints per-tenant stats; exit non-zero if any Failed>0
```

- **Crash-safe & re-runnable:** each row is overwritten by its own atomic
  `PutSecret`; a crash leaves a mix of migrated/not, and re-running finishes
  (every row is self-describing).
- **Per-row isolation:** unlike spawn, one undecryptable row does not abort the
  batch — it is counted and the pass continues, so a single corrupt row can't
  block migrating the rest.

**Secret value lifetime** is unchanged from M2: plaintext exists only
transiently in the POST body, in the `rotate` pass's memory, and in the child
env at spawn. Never logged, never returned, never stored.

---

## Error handling & edge cases

| Situation | Behavior |
|---|---|
| No keys set at all | Broker nil — feature disabled (M2 back-compat). `/admin/secrets*` incl. rotate → `503`. |
| `RUNTIME_SECRETS_KEYS` malformed (bad `id:b64`, dup ID, key ≠ 32B) | **Fatal at startup.** |
| `RUNTIME_SECRETS_PRIMARY` unset or not in keyring | **Fatal at startup** (no key can seal). |
| `RUNTIME_SECRETS_KEY` set but bytes ≠ any keyring entry | **Fatal at startup** (declared a legacy key the ring can't honor). |
| `keyIDLen == 0` or truncated header in a `0x01` blob | `Open` errors (corrupt) → fail-closed. |
| `0x01` blob names a keyID not in the keyring | `Open` errors ("unknown key id") → fail-closed; surfaces a key dropped while still in use. Logged with tenant+name+keyID, never value. |
| Legacy (version-less) blob but no legacy key configured | `Open` errors → fail-closed. |
| AAD mismatch (row swapped/renamed in DB) | GCM `Open` fails → fail-closed. **Row-swap defense working.** |
| Tampered/truncated ciphertext, wrong key | GCM auth failure → fail-closed (M2 behavior preserved). |
| `rotate` hits one undecryptable row | `Failed++` (log name only); pass continues. `RotateStats.Failed>0`; CLI exits non-zero. |
| `rotate` crashes mid-pass | Re-runnable; migrated rows on primary, rest self-describing; re-run finishes. |
| `rotate` for nonexistent / cross-tenant target | `effectiveTenant` validation → `400` (M2 behavior). |
| Concurrent `rotate` + `set` on same row | Both full-row UPSERTs; last writer wins; row stays valid. |
| Concurrent two `rotate` passes | Idempotent; worst case a row re-sealed twice. Harmless. |

**Two deliberate asymmetries:**
1. **Spawn fails the whole tenant on one bad row; `Rotate` does not.** An agent
   can't start half-credentialed (all-or-nothing); a maintenance sweep must not
   let one corrupt row block migrating the rest.
2. **Config errors are fatal; data errors are fail-closed.** A bad keyring is an
   operator mistake caught loudly at boot; a bad row at runtime degrades safely
   (that tenant's spawns fail, logged) without taking runtimed down.

**Security model:** keyring keys live in runtimed's environment at DSN trust
level (unchanged). No key bytes, no secret values are ever logged or returned.
Losing **all** keyring keys still makes ciphertext unrecoverable (documented) —
but with rotation, losing a *retired* key no longer matters once its rows are
migrated. AAD binding pins each ciphertext to its `(tenant, name)`, defeating
DB-level row swaps.

---

## Testing strategy

### Unit (hermetic, no DB)

`internal/identity/crypto_test.go` (rewrite for keyring):
- `Seal`→`Open` round-trips with matching `(tenant,name)` AAD; short, multiline
  PEM, binary values.
- New-format blob starts with `0x01`, embeds the primary key ID, decodes back.
- `Open` with wrong `(tenant,name)` (swapped tenant or name) → error (AAD).
- Multi-key keyring: a blob sealed under `v1` opens after primary moves to `v2`.
- `Open` on a blob whose keyID isn't in the keyring → "unknown key id".
- Legacy: a hand-built `nonce||ciphertext` blob (nil AAD, legacy key) opens via
  the version-less branch; a `0x01` blob never takes the legacy path.
- Tamper/truncate (flip a ciphertext byte; cut below header+nonce length) → error.
- Same plaintext sealed twice → different blobs (fresh nonce).

`internal/identity/keyring_test.go` (new — config parsing):
- `KEYS="v1:b64,v2:b64"` + `PRIMARY=v2` → 2-key ring, primary v2.
- Lone `RUNTIME_SECRETS_KEY` → `{v1:key}`, primary v1, legacy v1.
- All unset → `(nil,nil)` sentinel.
- Errors: bad base64, dup ID, non-32B key, primary not in ring, `RUNTIME_SECRETS_KEY`
  bytes ≠ any ring entry → each returns an error.

`internal/identity/broker_test.go` (extend, fake store):
- `SetSecret` persists a `0x01` primary-format blob (assert first byte + embedded
  ID, not plaintext).
- `SecretsFor` decrypts a mix of legacy + v1 + v2 rows → correct name→plaintext.
- `SecretsFor` fail-closed: one undecryptable row → nil map + error (M2 invariant).
- `Rotate`: store seeded with legacy + old-key rows → after pass every stored
  blob is `0x01` under primary; `RotateStats{Total,Rotated,Failed}` correct.
- `Rotate` isolates a bad row: 1 corrupt + 2 good → 2 rotated, 1 failed, good
  rows now primary.
- `Rotate` idempotent: run twice → second run re-seals (fresh nonce), Failed=0.

`controlplane/admin_test.go` (extend):
- `POST /admin/secrets/rotate` non-admin → 403; nil broker → 503; non-superuser
  scoped to own tenant; superuser with no tenant → all-tenants path invoked
  (assert via a fake `SecretAdmin` recording the tenants rotated).

### Integration (`//go:build integration`, Postgres at the standard DSN)

`internal/identity/secrets_store_test.go` (extend): `Rotate` over real Postgres —
seed rows including a hand-written legacy blob, rotate, reload, assert all rows
now primary-format and still decrypt to original plaintext. `t.Cleanup` drops the
`secrets` table (the M1 pollution lesson).

`test/secrets_e2e_test.go` (extend the headline E2E) — prove rotation end-to-end
across the spawn path:
1. Seal a secret under an **old** primary; spawn → env shows the value.
2. Reconfigure the broker: add a **new** primary, keep the old as non-primary;
   run `Rotate`.
3. Spawn again → env still shows the same value (served from a re-sealed row).
4. **Retire** the old key (keyring = new only); spawn again → still works
   (proves the backlog was fully migrated and the old key is safely droppable).

Step 4 is the proof rotation achieves its purpose, not merely that it runs.

### Deliberately not tested

External KMS, per-tenant keys (future), TLS termination (operator concern), live
reload (still next-spawn-only).

---

## Backward compatibility

Fully additive. A deployment with no keys set behaves exactly as M2-disabled
(broker nil, 503 API). A deployment using only the M2 `RUNTIME_SECRETS_KEY`
behaves exactly as M2: the key becomes keyring `{v1:key}` (primary + legacy v1),
existing rows read via the legacy path, new writes use `v1` new-format, and the
operator can `rotate` whenever they choose to migrate the backlog. Upgrading is a
no-flag-day operation: old rows keep working until deliberately rotated.

---

## Limitations (record in README + ROADMAP)

- **One keyring per deployment**, shared across tenants. Per-tenant keys are
  future work (the self-describing-row format already admits them).
- **Manual rotation.** The operator deploys a new primary and runs
  `runtimectl admin secret rotate`; there is no scheduler.
- **Retiring a key is operator-driven.** After `rotate` reports 0 rows on an old
  key, the operator removes it from `RUNTIME_SECRETS_KEYS`. The platform does not
  auto-prune keys (it cannot prove no row uses one without scanning).
- **Keyring keys in env**, at DSN trust level (unchanged from M2).
- **Spawn-time refresh only** — a key/secret change still needs an agent restart
  to apply (M2 limitation, unchanged).

---

## Documentation updates on completion

- README "Authentication & multi-tenancy" → extend the Secrets sub-section with
  the keyring env vars, `runtimectl admin secret rotate`, AAD binding, and the
  retire-old-key workflow.
- README env-var table → `RUNTIME_SECRETS_KEYS`, `RUNTIME_SECRETS_PRIMARY` (note
  `RUNTIME_SECRETS_KEY` is still honored as the legacy single-key / legacy-decrypt
  key).
- README CLI table → `admin secret rotate`.
- ROADMAP §B3 → mark key rotation + AAD binding done; move remaining Identity
  items (per-tenant keys, RBAC, console UI, self-service, local passwords, CSRF)
  down.
- `docs/images/project-layout.mmd` → note the keyring in the `identity/`
  description.
- Project memory `runtime-platform-project.md` → Identity M3 paragraph.
