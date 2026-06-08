# Identity M2 — Secrets Brokering Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Per-tenant provider credentials, encrypted at rest with AES-256-GCM, brokered as environment variables into that tenant's agent subprocesses at spawn time, with agents unmodified.

**Architecture:** A new `internal/identity/crypto.go` (`Cipher`) and `internal/identity/secrets.go` (store methods) back a new `internal/identity/broker.go` (`Broker`) that seals on write and opens on read. The control plane sees the broker only through two tiny interfaces — `SecretBroker` (read, used by the spawn path) and `SecretAdmin` (write, used by the admin API). `Registry.Get` injects the broker into each `AgentProcess`, whose `SpawnFunc` appends decrypted tenant secrets after the `RUNTIME_*` vars (so they shadow the inherited operator env). When `RUNTIME_SECRETS_KEY` is unset, the broker is nil and behavior is byte-identical to today.

**Tech Stack:** Go 1.25, stdlib `crypto/aes`+`crypto/cipher`+`crypto/rand`, Postgres (pgx), `net/http`. The `go` CLI is ground truth (ignore LSP). Integration tests use `//go:build integration` against `postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable`.

**Spec:** `docs/superpowers/specs/2026-06-09-identity-m2-secrets-brokering-design.md`

**Conventions:**
- Git commits MUST use `git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit`.
- Commit messages end with `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`.
- Work happens on branch `feat/identity-m2-secrets` (already created; the spec is committed there as `50b26a1`).
- Run `go build ./...` and `go vet ./...` before each commit. Hermetic unit tests: `go test ./...`. Integration: `go test -tags integration ./internal/identity/ ./test/` (run those two packages **sequentially**, not via `./...`, because both drop the same identity tables).

---

## File Structure

| File | Responsibility | Action |
|---|---|---|
| `internal/identity/crypto.go` | `Cipher`: AES-256-GCM seal/open, nonce-prepended | Create |
| `internal/identity/crypto_test.go` | Cipher unit tests (hermetic) | Create |
| `internal/identity/schema.sql` | add `secrets` table | Modify |
| `internal/identity/secrets.go` | `*Store` secret methods (ciphertext only) + read models | Create |
| `internal/identity/secrets_store_test.go` | secret store tests (integration) | Create |
| `internal/identity/broker.go` | `Broker{store,cipher}`: `SecretsFor`/`SetSecret`/`ListSecretNames`/`DeleteSecret` | Create |
| `internal/identity/broker_test.go` | Broker unit tests over a fake store (hermetic) | Create |
| `controlplane/proxy.go` | `SecretBroker` iface, `AgentProcess.broker`, `buildEnv` | Modify |
| `controlplane/proxy_test.go` | buildEnv ordering + nil back-compat tests | Modify |
| `controlplane/registry.go` | `Registry.broker` + `SetBroker`, inject in `Get` | Modify |
| `controlplane/admin.go` | `SecretAdmin` iface, `/admin/secrets` routes, write-only | Modify |
| `controlplane/admin_test.go` | secret admin tests (403/503/400, no-value-leak) | Modify |
| `cmd/runtimed/main.go` | parse `RUNTIME_SECRETS_KEY`, build broker, wire | Modify |
| `cmd/runtimectl/main.go` | `admin secret set/ls/rm` | Modify |
| `test/secrets_e2e_test.go` | two-tenant spawn-isolation E2E (integration) | Create |
| `README.md`, `ROADMAP.md`, `docs/images/project-layout.mmd` | docs | Modify |

---

## Task 1: Cipher (AES-256-GCM seal/open)

**Files:**
- Create: `internal/identity/crypto.go`
- Test: `internal/identity/crypto_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/identity/crypto_test.go`:

```go
package identity

import (
	"bytes"
	"testing"
)

func key32() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i)
	}
	return k
}

func TestCipher_RoundTrip(t *testing.T) {
	c, err := NewCipher(key32())
	if err != nil {
		t.Fatal(err)
	}
	for _, pt := range [][]byte{
		[]byte("sk-abc123"),
		[]byte(""),
		[]byte("-----BEGIN KEY-----\nline2\nline3\n-----END KEY-----\n"),
		{0x00, 0x01, 0xff, 0xfe, 0x10},
	} {
		ct, err := c.Seal(pt)
		if err != nil {
			t.Fatalf("seal: %v", err)
		}
		got, err := c.Open(ct)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		if !bytes.Equal(got, pt) {
			t.Fatalf("round-trip mismatch: got %q want %q", got, pt)
		}
	}
}

func TestNewCipher_RejectsWrongKeySize(t *testing.T) {
	for _, n := range []int{0, 16, 31, 33} {
		if _, err := NewCipher(make([]byte, n)); err == nil {
			t.Fatalf("NewCipher accepted %d-byte key, want error", n)
		}
	}
}

func TestCipher_NonceMakesCiphertextUnique(t *testing.T) {
	c, _ := NewCipher(key32())
	pt := []byte("same-plaintext")
	a, _ := c.Seal(pt)
	b, _ := c.Seal(pt)
	if bytes.Equal(a, b) {
		t.Fatal("two seals of same plaintext produced identical ciphertext (nonce not random)")
	}
}

func TestCipher_OpenRejectsTampered(t *testing.T) {
	c, _ := NewCipher(key32())
	ct, _ := c.Seal([]byte("secret"))
	// Flip a byte in the ciphertext body (after the 12-byte nonce).
	ct[len(ct)-1] ^= 0xff
	if _, err := c.Open(ct); err == nil {
		t.Fatal("Open accepted tampered ciphertext")
	}
	// Truncated input shorter than a nonce must error, not panic.
	if _, err := c.Open([]byte{1, 2, 3}); err == nil {
		t.Fatal("Open accepted too-short input")
	}
}

func TestCipher_OpenRejectsWrongKey(t *testing.T) {
	c1, _ := NewCipher(key32())
	other := key32()
	other[0] ^= 0xff
	c2, _ := NewCipher(other)
	ct, _ := c1.Seal([]byte("secret"))
	if _, err := c2.Open(ct); err == nil {
		t.Fatal("Open with wrong key succeeded")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/identity/ -run TestCipher -run 'Cipher|NewCipher' 2>&1 | head`
Expected: FAIL — `undefined: NewCipher`.

- [ ] **Step 3: Write the implementation**

Create `internal/identity/crypto.go`:

```go
package identity

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

// Cipher seals and opens secret values with AES-256-GCM. Each Seal prepends a
// fresh random 12-byte nonce to the returned ciphertext; Open expects that
// layout. The 32-byte key comes from the operator (RUNTIME_SECRETS_KEY).
type Cipher struct {
	aead cipher.AEAD
}

// NewCipher builds a Cipher from a 32-byte (AES-256) key. Any other length is an
// error — a misconfigured key must be caught at startup, not at spawn time.
func NewCipher(key []byte) (*Cipher, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("identity: secrets key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Cipher{aead: aead}, nil
}

// Seal returns nonce || GCM(plaintext). The plaintext may be any bytes.
func (c *Cipher) Seal(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	// Seal appends the ciphertext to its first arg (the nonce), giving
	// nonce||ciphertext in one allocation.
	return c.aead.Seal(nonce, nonce, plaintext, nil), nil
}

// Open reverses Seal. It errors on a too-short input or any authentication
// failure (tampering / wrong key).
func (c *Cipher) Open(blob []byte) ([]byte, error) {
	ns := c.aead.NonceSize()
	if len(blob) < ns {
		return nil, errors.New("identity: ciphertext shorter than nonce")
	}
	nonce, ct := blob[:ns], blob[ns:]
	return c.aead.Open(nil, nonce, ct, nil)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/identity/ -run 'Cipher|NewCipher' -v 2>&1 | tail -20`
Expected: PASS (5 tests).

- [ ] **Step 5: Build, vet, commit**

```bash
go build ./... && go vet ./internal/identity/
git add internal/identity/crypto.go internal/identity/crypto_test.go
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "feat(identity): AES-256-GCM Cipher for secret values

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 2: secrets table + store methods

**Files:**
- Modify: `internal/identity/schema.sql`
- Create: `internal/identity/secrets.go`
- Test: `internal/identity/secrets_store_test.go`

- [ ] **Step 1: Add the table to the schema**

Append to `internal/identity/schema.sql` (after the `service_keys` block, before or after the existing index — order is fine since it is its own statement):

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

- [ ] **Step 2: Write the failing integration test**

Create `internal/identity/secrets_store_test.go`:

```go
//go:build integration

package identity

import (
	"context"
	"testing"
)

// freshStore (in store_test.go) drops+recreates the identity tables, which now
// includes `secrets` via the embedded schema. The CASCADE drop of `tenants`
// removes `secrets` too, so no extra cleanup is needed here.

func TestSecretsStore_CRUD(t *testing.T) {
	ctx := context.Background()
	s, db := freshStore(t)
	defer db.Close()

	if err := s.CreateTenant(ctx, "alpha", "A"); err != nil {
		t.Fatal(err)
	}

	// Put two secrets (ciphertext is opaque to the store — use marker bytes).
	if err := s.PutSecret(ctx, "alpha", "OPENAI_API_KEY", []byte("ENC1")); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSecret(ctx, "alpha", "OPENAI_BASE_URL", []byte("ENC2")); err != nil {
		t.Fatal(err)
	}

	// List returns names + metadata, never the value.
	metas, err := s.ListSecretNames(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 2 {
		t.Fatalf("want 2 secrets, got %d", len(metas))
	}
	if metas[0].Name == "" || metas[0].UpdatedAt.IsZero() {
		t.Fatalf("meta missing name/updated_at: %+v", metas[0])
	}

	// LoadSecrets returns ciphertext for the broker.
	enc, err := s.LoadSecrets(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, e := range enc {
		got[e.Name] = string(e.ValueEnc)
	}
	if got["OPENAI_API_KEY"] != "ENC1" || got["OPENAI_BASE_URL"] != "ENC2" {
		t.Fatalf("LoadSecrets mismatch: %+v", got)
	}

	// UPSERT overwrites value and bumps updated_at.
	before := metas[0].UpdatedAt
	if err := s.PutSecret(ctx, "alpha", "OPENAI_API_KEY", []byte("ENC1b")); err != nil {
		t.Fatal(err)
	}
	enc2, _ := s.LoadSecrets(ctx, "alpha")
	for _, e := range enc2 {
		if e.Name == "OPENAI_API_KEY" && string(e.ValueEnc) != "ENC1b" {
			t.Fatalf("UPSERT did not overwrite: %q", e.ValueEnc)
		}
	}
	metas2, _ := s.ListSecretNames(ctx, "alpha")
	for _, m := range metas2 {
		if m.Name == "OPENAI_API_KEY" && !m.UpdatedAt.After(before) {
			t.Fatalf("updated_at not bumped on UPSERT (before=%v after=%v)", before, m.UpdatedAt)
		}
	}

	// Delete removes one.
	if err := s.DeleteSecret(ctx, "alpha", "OPENAI_BASE_URL"); err != nil {
		t.Fatal(err)
	}
	metas3, _ := s.ListSecretNames(ctx, "alpha")
	if len(metas3) != 1 {
		t.Fatalf("after delete want 1, got %d", len(metas3))
	}
}

func TestSecretsStore_TenantCascade(t *testing.T) {
	ctx := context.Background()
	s, db := freshStore(t)
	defer db.Close()

	if err := s.CreateTenant(ctx, "alpha", "A"); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSecret(ctx, "alpha", "K", []byte("X")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM tenants WHERE id='alpha'`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(1) FROM secrets WHERE tenant_id='alpha'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("secrets not cascade-deleted with tenant: %d remain", n)
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test -tags integration ./internal/identity/ -run TestSecretsStore 2>&1 | head`
Expected: FAIL — `s.PutSecret undefined` (or build error).

- [ ] **Step 4: Write the store methods**

Create `internal/identity/secrets.go`:

```go
package identity

import (
	"context"
	"time"
)

// SecretMeta is the list read model: name + timestamps, never the value.
type SecretMeta struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// EncryptedSecret is the broker-facing read model: name + ciphertext only.
type EncryptedSecret struct {
	Name     string
	ValueEnc []byte
}

// PutSecret inserts or overwrites a tenant's secret. valueEnc is opaque
// ciphertext (the store never sees plaintext). UPSERT bumps updated_at.
func (s *Store) PutSecret(ctx context.Context, tenantID, name string, valueEnc []byte) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO secrets (tenant_id, name, value_enc) VALUES ($1,$2,$3)
		 ON CONFLICT (tenant_id, name)
		 DO UPDATE SET value_enc=EXCLUDED.value_enc, updated_at=now()`,
		tenantID, name, valueEnc)
	return err
}

// ListSecretNames returns names + timestamps for a tenant. value_enc is never
// selected, so a value cannot leak through a listing.
func (s *Store) ListSecretNames(ctx context.Context, tenantID string) ([]SecretMeta, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, created_at, updated_at FROM secrets WHERE tenant_id=$1 ORDER BY name`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SecretMeta
	for rows.Next() {
		var m SecretMeta
		if err := rows.Scan(&m.Name, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// DeleteSecret removes one secret. No-op if absent.
func (s *Store) DeleteSecret(ctx context.Context, tenantID, name string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM secrets WHERE tenant_id=$1 AND name=$2`, tenantID, name)
	return err
}

// LoadSecrets returns all of a tenant's encrypted secrets for the broker to
// decrypt at spawn time.
func (s *Store) LoadSecrets(ctx context.Context, tenantID string) ([]EncryptedSecret, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, value_enc FROM secrets WHERE tenant_id=$1 ORDER BY name`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EncryptedSecret
	for rows.Next() {
		var e EncryptedSecret
		if err := rows.Scan(&e.Name, &e.ValueEnc); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test -tags integration ./internal/identity/ -run TestSecretsStore -v 2>&1 | tail -20`
Expected: PASS (2 tests). If postgres is unreachable the test SKIPs — start Postgres.app first.

- [ ] **Step 6: Build, vet, commit**

```bash
go build ./... && go vet ./internal/identity/
git add internal/identity/schema.sql internal/identity/secrets.go internal/identity/secrets_store_test.go
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "feat(identity): secrets table + store CRUD (ciphertext only)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 3: Broker (crypto meets storage)

**Files:**
- Create: `internal/identity/broker.go`
- Test: `internal/identity/broker_test.go`

- [ ] **Step 1: Write the failing unit tests**

Create `internal/identity/broker_test.go`:

```go
package identity

import (
	"context"
	"errors"
	"testing"
)

// fakeSecretStore implements secretStore in-memory for hermetic broker tests.
type fakeSecretStore struct {
	put     map[string][]byte // name -> ciphertext (single tenant for the test)
	loaded  []EncryptedSecret
	loadErr error // error to return from LoadSecrets
}

func (f *fakeSecretStore) PutSecret(_ context.Context, _, name string, enc []byte) error {
	if f.put == nil {
		f.put = map[string][]byte{}
	}
	f.put[name] = enc
	return nil
}
func (f *fakeSecretStore) ListSecretNames(_ context.Context, _ string) ([]SecretMeta, error) {
	return nil, nil
}
func (f *fakeSecretStore) DeleteSecret(_ context.Context, _, _ string) error { return nil }
func (f *fakeSecretStore) LoadSecrets(_ context.Context, _ string) ([]EncryptedSecret, error) {
	return f.loaded, f.loadErr
}

func newTestBroker(t *testing.T, fs *fakeSecretStore) *Broker {
	t.Helper()
	c, err := NewCipher(key32())
	if err != nil {
		t.Fatal(err)
	}
	return NewBroker(fs, c)
}

func TestBroker_SetThenSecretsFor(t *testing.T) {
	fs := &fakeSecretStore{}
	b := newTestBroker(t, fs)
	ctx := context.Background()

	if err := b.SetSecret(ctx, "alpha", "OPENAI_API_KEY", "sk-xyz"); err != nil {
		t.Fatal(err)
	}
	// The store received ciphertext, NOT the plaintext.
	if string(fs.put["OPENAI_API_KEY"]) == "sk-xyz" {
		t.Fatal("SetSecret stored plaintext, expected ciphertext")
	}

	// Feed that ciphertext back through LoadSecrets and decrypt.
	fs.loaded = []EncryptedSecret{{Name: "OPENAI_API_KEY", ValueEnc: fs.put["OPENAI_API_KEY"]}}
	got, err := b.SecretsFor(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if got["OPENAI_API_KEY"] != "sk-xyz" {
		t.Fatalf("SecretsFor decrypt mismatch: %q", got["OPENAI_API_KEY"])
	}
}

func TestBroker_SecretsForEmpty(t *testing.T) {
	b := newTestBroker(t, &fakeSecretStore{})
	got, err := b.SecretsFor(context.Background(), "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("want empty map, got %v", got)
	}
}

func TestBroker_SecretsForFailsClosedOnBadCiphertext(t *testing.T) {
	fs := &fakeSecretStore{loaded: []EncryptedSecret{{Name: "K", ValueEnc: []byte("not-valid-ciphertext")}}}
	b := newTestBroker(t, fs)
	if _, err := b.SecretsFor(context.Background(), "alpha"); err == nil {
		t.Fatal("SecretsFor must error (fail closed) on undecryptable row")
	}
}

func TestBroker_SecretsForPropagatesLoadError(t *testing.T) {
	fs := &fakeSecretStore{loadErr: errors.New("db down")}
	b := newTestBroker(t, fs)
	if _, err := b.SecretsFor(context.Background(), "alpha"); err == nil {
		t.Fatal("SecretsFor must propagate load error")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/identity/ -run TestBroker 2>&1 | head`
Expected: FAIL — `undefined: NewBroker` / `Broker`.

- [ ] **Step 3: Write the broker**

Create `internal/identity/broker.go`:

```go
package identity

import (
	"context"
	"fmt"
)

// secretStore is the slice of *Store the Broker needs. Declared as an interface
// so the broker is unit-testable without Postgres.
type secretStore interface {
	PutSecret(ctx context.Context, tenantID, name string, valueEnc []byte) error
	ListSecretNames(ctx context.Context, tenantID string) ([]SecretMeta, error)
	DeleteSecret(ctx context.Context, tenantID, name string) error
	LoadSecrets(ctx context.Context, tenantID string) ([]EncryptedSecret, error)
}

// Broker is the single place where the Cipher meets storage. It seals on write
// and opens on read; the control plane sees it only through the SecretBroker
// (read) and SecretAdmin (write) interfaces it satisfies.
type Broker struct {
	store  secretStore
	cipher *Cipher
}

// NewBroker pairs a store with a cipher.
func NewBroker(store secretStore, cipher *Cipher) *Broker {
	return &Broker{store: store, cipher: cipher}
}

// SecretsFor decrypts all of a tenant's secrets into name->plaintext. It fails
// closed: any decryption error aborts the whole resolution rather than dropping
// a secret, so an agent never starts with a partial set.
func (b *Broker) SecretsFor(ctx context.Context, tenant string) (map[string]string, error) {
	enc, err := b.store.LoadSecrets(ctx, tenant)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(enc))
	for _, e := range enc {
		pt, err := b.cipher.Open(e.ValueEnc)
		if err != nil {
			return nil, fmt.Errorf("identity: decrypt secret %q for tenant %q: %w", e.Name, tenant, err)
		}
		out[e.Name] = string(pt)
	}
	return out, nil
}

// SetSecret seals the plaintext and persists it (UPSERT).
func (b *Broker) SetSecret(ctx context.Context, tenant, name, plaintext string) error {
	enc, err := b.cipher.Seal([]byte(plaintext))
	if err != nil {
		return err
	}
	return b.store.PutSecret(ctx, tenant, name, enc)
}

// ListSecretNames passes through to the store (names + metadata, no values).
func (b *Broker) ListSecretNames(ctx context.Context, tenant string) ([]SecretMeta, error) {
	return b.store.ListSecretNames(ctx, tenant)
}

// DeleteSecret passes through to the store.
func (b *Broker) DeleteSecret(ctx context.Context, tenant, name string) error {
	return b.store.DeleteSecret(ctx, tenant, name)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/identity/ -run TestBroker -v 2>&1 | tail -20`
Expected: PASS (4 tests).

- [ ] **Step 5: Build, vet, commit**

```bash
go build ./... && go vet ./internal/identity/
git add internal/identity/broker.go internal/identity/broker_test.go
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "feat(identity): Broker seals on write, opens on read (fail-closed)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 4: spawn-path injection (buildEnv + SecretBroker)

**Files:**
- Modify: `controlplane/proxy.go`
- Test: `controlplane/proxy_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `controlplane/proxy_test.go` (add `"context"` is already imported; add `"slices"` to imports):

```go
// fakeBroker implements SecretBroker for spawn-path tests.
type fakeBroker struct {
	secrets map[string]map[string]string // tenant -> name -> value
	err     error
}

func (f fakeBroker) SecretsFor(_ context.Context, tenant string) (map[string]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.secrets[tenant], nil
}

func TestBuildEnv_TenantSecretsShadowAfterRuntimeVars(t *testing.T) {
	ap := AgentProcess{
		AgentID: "a1", Addr: "127.0.0.1:9", PGDSN: "dsn", Kind: "", Tenant: "alpha",
		broker: fakeBroker{secrets: map[string]map[string]string{
			"alpha": {"OPENAI_API_KEY": "sk-alpha"},
		}},
	}
	env, err := ap.buildEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The RUNTIME_* vars must appear, and the tenant secret must come AFTER them
	// (so exec resolves the last value — shadowing the inherited env).
	idxRuntime := lastIndexWithPrefix(env, "RUNTIME_AGENT_ID=")
	idxSecret := lastIndexWithPrefix(env, "OPENAI_API_KEY=")
	if idxRuntime < 0 || idxSecret < 0 {
		t.Fatalf("missing vars: runtime=%d secret=%d env=%v", idxRuntime, idxSecret, env)
	}
	if idxSecret < idxRuntime {
		t.Fatalf("tenant secret must come after RUNTIME_* vars: secret@%d runtime@%d", idxSecret, idxRuntime)
	}
	if !slices.Contains(env, "OPENAI_API_KEY=sk-alpha") {
		t.Fatalf("tenant secret value missing: %v", env)
	}
}

func TestBuildEnv_NilBrokerMatchesLegacy(t *testing.T) {
	ap := AgentProcess{AgentID: "a1", Addr: "127.0.0.1:9", PGDSN: "dsn", Tenant: "alpha"}
	env, err := ap.buildEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// With no broker, env is exactly os.Environ()+RUNTIME_* — no extra vars.
	// Assert none of our injected names other than RUNTIME_* leaked in.
	if lastIndexWithPrefix(env, "OPENAI_API_KEY=") >= 0 {
		t.Fatal("nil broker must not inject secrets")
	}
	if lastIndexWithPrefix(env, "RUNTIME_AGENT_ID=a1") < 0 {
		t.Fatal("RUNTIME_AGENT_ID still expected with nil broker")
	}
}

func TestBuildEnv_BrokerErrorFailsClosed(t *testing.T) {
	ap := AgentProcess{AgentID: "a1", Tenant: "alpha", broker: fakeBroker{err: errBrokerTest}}
	if _, err := ap.buildEnv(context.Background()); err == nil {
		t.Fatal("buildEnv must return broker error (fail closed)")
	}
}

var errBrokerTest = errorsNew("broker boom")

// errorsNew is a tiny helper so the test file needn't import errors twice.
func errorsNew(s string) error { return &strErr{s} }

type strErr struct{ s string }

func (e *strErr) Error() string { return e.s }

// lastIndexWithPrefix returns the index of the last env entry with the prefix.
func lastIndexWithPrefix(env []string, prefix string) int {
	idx := -1
	for i, e := range env {
		if len(e) >= len(prefix) && e[:len(prefix)] == prefix {
			idx = i
		}
	}
	return idx
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./controlplane/ -run TestBuildEnv 2>&1 | head`
Expected: FAIL — `ap.buildEnv undefined` and `AgentProcess` has no `broker` field.

- [ ] **Step 3: Add the SecretBroker interface, broker field, and buildEnv**

In `controlplane/proxy.go`, add the interface and field, and refactor `SpawnFunc` to use `buildEnv`. Replace the import block and the top of the file through `SpawnFunc` with:

```go
package controlplane

import (
	"context"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
)

// SecretBroker resolves a tenant's secrets to name->plaintext at spawn time.
// *identity.Broker implements it. A nil broker means no brokering (back-compat).
type SecretBroker interface {
	SecretsFor(ctx context.Context, tenant string) (map[string]string, error)
}

// AgentProcess describes a supervised agent subprocess.
type AgentProcess struct {
	AgentID string
	Addr    string // host:port the subprocess listens on, e.g. "127.0.0.1:8081"
	BinPath string // path to the agentd binary
	PGDSN   string
	Kind    string   // optional agent kind; "" ⇒ testagent. Passed via RUNTIME_AGENT_KIND.
	Command []string // when non-empty, exec this instead of BinPath (foreign-process agents)
	WorkDir string   // optional working directory for Command
	Tenant  string   // tenant that owns this agent (from runtime.yaml; "default" if unset)

	broker SecretBroker // optional; injected by the Registry. nil ⇒ no secret brokering.
}

// buildEnv assembles the child environment: the inherited operator env, then the
// RUNTIME_* control vars, then (if a broker is set) the tenant's decrypted
// secrets LAST so they shadow any inherited var of the same name. A broker error
// fails closed — the caller must not start the process.
func (a AgentProcess) buildEnv(ctx context.Context) ([]string, error) {
	env := append(os.Environ(),
		"RUNTIME_PG_DSN="+a.PGDSN,
		"RUNTIME_LISTEN_ADDR="+a.Addr,
		"RUNTIME_AGENT_ID="+a.AgentID,
		"RUNTIME_AGENT_KIND="+a.Kind,
	)
	if a.broker != nil {
		secrets, err := a.broker.SecretsFor(ctx, a.Tenant)
		if err != nil {
			return nil, err
		}
		for name, val := range secrets {
			env = append(env, name+"="+val)
		}
	}
	return env, nil
}

// SpawnFunc returns a Supervisor-compatible spawn closure that launches agentd
// (or, when Command is set, an arbitrary command) with the brokered env and
// reports its exit on the returned channel.
func (a AgentProcess) SpawnFunc() func(ctx context.Context) <-chan error {
	return func(ctx context.Context) <-chan error {
		ch := make(chan error, 1)
		env, err := a.buildEnv(ctx)
		if err != nil {
			ch <- err
			return ch
		}
		var cmd *exec.Cmd
		if len(a.Command) > 0 {
			cmd = exec.CommandContext(ctx, a.Command[0], a.Command[1:]...)
			if a.WorkDir != "" {
				cmd.Dir = a.WorkDir
			}
		} else {
			cmd = exec.CommandContext(ctx, a.BinPath)
		}
		cmd.Env = env
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			ch <- err
			return ch
		}
		go func() { ch <- cmd.Wait() }()
		return ch
	}
}
```

(Leave `reverseProxy` below unchanged.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./controlplane/ -run 'TestBuildEnv|TestSpawnFuncCommand|TestReverseProxy' -v 2>&1 | tail -25`
Expected: PASS — the three new buildEnv tests plus the existing spawn/proxy tests still green.

- [ ] **Step 5: Build, vet, commit**

```bash
go build ./... && go vet ./controlplane/
git add controlplane/proxy.go controlplane/proxy_test.go
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "feat(controlplane): buildEnv brokers tenant secrets into spawn env

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 5: Registry injects the broker

**Files:**
- Modify: `controlplane/registry.go`
- Test: `controlplane/registry_test.go`

- [ ] **Step 1: Write the failing test**

Append to `controlplane/registry_test.go` (ensure imports include `"context"` and `"github.com/sausheong/runtime/internal/config"` — check the existing file header and add what's missing):

```go
func TestRegistry_GetInjectsBroker(t *testing.T) {
	cfg := &config.Config{Agents: []config.AgentConfig{
		{ID: "a1", ListenAddr: "127.0.0.1:9001", Tenant: "alpha"},
	}}
	reg := NewRegistry(cfg, "./agentd", "dsn")

	// Before SetBroker: the AgentProcess has no broker.
	ap, ok := reg.Get("a1")
	if !ok {
		t.Fatal("agent a1 missing")
	}
	if ap.broker != nil {
		t.Fatal("broker should be nil before SetBroker")
	}

	br := fakeBroker{secrets: map[string]map[string]string{"alpha": {"K": "v"}}}
	reg.SetBroker(br)
	ap2, _ := reg.Get("a1")
	if ap2.broker == nil {
		t.Fatal("Get must inject the registry broker into the AgentProcess")
	}
	env, err := ap2.buildEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if lastIndexWithPrefix(env, "K=v") < 0 {
		t.Fatalf("brokered secret not in env: %v", env)
	}
}
```

(If `config.AgentConfig` uses different field names, match the existing struct — check `internal/config`. The fields used here are `ID`, `ListenAddr`, `Tenant`, which the registry already reads in `NewRegistry`.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./controlplane/ -run TestRegistry_GetInjectsBroker 2>&1 | head`
Expected: FAIL — `reg.SetBroker undefined`.

- [ ] **Step 3: Add broker field + SetBroker, inject in Get**

In `controlplane/registry.go`, modify the `Registry` struct and `Get`:

```go
// Registry holds the agents the control plane hosts, built from config.
// Read-only after construction except for the optional secret broker.
type Registry struct {
	order  []string
	agents map[string]AgentProcess
	infos  map[string]AgentInfo
	broker SecretBroker // optional; injected into each AgentProcess on Get.
}
```

Add a setter (place it after `NewRegistry`):

```go
// SetBroker installs the secret broker injected into every AgentProcess returned
// by Get. Call once at startup, before agents are spawned. nil ⇒ no brokering.
func (r *Registry) SetBroker(b SecretBroker) { r.broker = b }
```

Modify `Get` to inject the broker into the returned copy:

```go
// Get returns the AgentProcess for id, with the registry's secret broker
// attached so its SpawnFunc brokers secrets.
func (r *Registry) Get(id string) (AgentProcess, bool) {
	ap, ok := r.agents[id]
	if ok {
		ap.broker = r.broker
	}
	return ap, ok
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./controlplane/ -run 'TestRegistry' -v 2>&1 | tail -20`
Expected: PASS — new injection test plus existing registry tests.

- [ ] **Step 5: Build, vet, commit**

```bash
go build ./... && go vet ./controlplane/
git add controlplane/registry.go controlplane/registry_test.go
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "feat(controlplane): Registry injects secret broker into AgentProcess

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 6: admin API — /admin/secrets (write-only)

**Files:**
- Modify: `controlplane/admin.go`
- Test: `controlplane/admin_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `controlplane/admin_test.go`:

```go
// fakeSecretAdmin implements SecretAdmin in-memory.
type fakeSecretAdmin struct {
	set   map[string]map[string]string // tenant -> name -> plaintext
	names map[string][]identity.SecretMeta
}

func newFakeSecretAdmin() *fakeSecretAdmin {
	return &fakeSecretAdmin{set: map[string]map[string]string{}, names: map[string][]identity.SecretMeta{}}
}
func (f *fakeSecretAdmin) SetSecret(_ context.Context, tenant, name, plaintext string) error {
	if f.set[tenant] == nil {
		f.set[tenant] = map[string]string{}
	}
	f.set[tenant][name] = plaintext
	f.names[tenant] = append(f.names[tenant], identity.SecretMeta{Name: name})
	return nil
}
func (f *fakeSecretAdmin) ListSecretNames(_ context.Context, tenant string) ([]identity.SecretMeta, error) {
	return f.names[tenant], nil
}
func (f *fakeSecretAdmin) DeleteSecret(_ context.Context, tenant, name string) error {
	delete(f.set[tenant], name)
	return nil
}

// adminMuxWithSecrets wires both the store and the secret admin.
func adminMuxWithSecrets(s AdminStore, sa SecretAdmin) http.Handler {
	mux := http.NewServeMux()
	RegisterAdmin(mux, s)
	RegisterSecretAdmin(mux, s, sa)
	return mux
}

func TestSecretAdmin_SetAndListNoValueLeak(t *testing.T) {
	s := newFakeAdminStore()
	s.CreateTenant(context.Background(), "alpha", "A")
	sa := newFakeSecretAdmin()
	mux := adminMuxWithSecrets(s, sa)

	// Set a secret as a tenant admin.
	body := `{"name":"OPENAI_API_KEY","value":"sk-secret"}`
	r := withPrincipal(httptest.NewRequest("POST", "/admin/secrets", strings.NewReader(body)),
		identity.Principal{TenantID: "alpha", Role: identity.RoleAdmin})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != 200 && rec.Code != 201 {
		t.Fatalf("set secret: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if sa.set["alpha"]["OPENAI_API_KEY"] != "sk-secret" {
		t.Fatalf("secret not stored: %+v", sa.set)
	}

	// List must return the name but NEVER the value.
	lr := withPrincipal(httptest.NewRequest("GET", "/admin/secrets", nil),
		identity.Principal{TenantID: "alpha", Role: identity.RoleAdmin})
	lrec := httptest.NewRecorder()
	mux.ServeHTTP(lrec, lr)
	if lrec.Code != 200 {
		t.Fatalf("list: code=%d", lrec.Code)
	}
	if strings.Contains(lrec.Body.String(), "sk-secret") {
		t.Fatalf("LIST LEAKED THE VALUE: %s", lrec.Body.String())
	}
	if !strings.Contains(lrec.Body.String(), "OPENAI_API_KEY") {
		t.Fatalf("list missing name: %s", lrec.Body.String())
	}
}

func TestSecretAdmin_NonAdminForbidden(t *testing.T) {
	mux := adminMuxWithSecrets(newFakeAdminStore(), newFakeSecretAdmin())
	r := withPrincipal(httptest.NewRequest("POST", "/admin/secrets", strings.NewReader(`{"name":"K","value":"v"}`)),
		identity.Principal{TenantID: "alpha", Role: identity.RoleOperator})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != 403 {
		t.Fatalf("operator set secret: code=%d want 403", rec.Code)
	}
}

func TestSecretAdmin_DisabledIs503(t *testing.T) {
	s := newFakeAdminStore()
	s.CreateTenant(context.Background(), "alpha", "A")
	mux := http.NewServeMux()
	RegisterAdmin(mux, s)
	RegisterSecretAdmin(mux, s, nil) // nil broker ⇒ feature disabled
	r := withPrincipal(httptest.NewRequest("POST", "/admin/secrets", strings.NewReader(`{"name":"K","value":"v"}`)),
		identity.Principal{TenantID: "alpha", Role: identity.RoleAdmin})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != 503 {
		t.Fatalf("no broker: code=%d want 503", rec.Code)
	}
}

func TestSecretAdmin_BadNameOrValue400(t *testing.T) {
	s := newFakeAdminStore()
	s.CreateTenant(context.Background(), "alpha", "A")
	mux := adminMuxWithSecrets(s, newFakeSecretAdmin())
	cases := []string{
		`{"name":"","value":"v"}`,         // empty name
		`{"name":"OPENAI","value":""}`,    // empty value
		`{"name":"bad name","value":"v"}`, // space → invalid env identifier
		`{"name":"1BAD","value":"v"}`,     // leading digit
		`{"name":"A=B","value":"v"}`,      // '=' in name
	}
	for _, body := range cases {
		r := withPrincipal(httptest.NewRequest("POST", "/admin/secrets", strings.NewReader(body)),
			identity.Principal{TenantID: "alpha", Role: identity.RoleAdmin})
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, r)
		if rec.Code != 400 {
			t.Fatalf("body %s: code=%d want 400", body, rec.Code)
		}
	}
}

func TestSecretAdmin_Delete(t *testing.T) {
	s := newFakeAdminStore()
	s.CreateTenant(context.Background(), "alpha", "A")
	sa := newFakeSecretAdmin()
	mux := adminMuxWithSecrets(s, sa)
	sa.SetSecret(context.Background(), "alpha", "K", "v")
	r := withPrincipal(httptest.NewRequest("DELETE", "/admin/secrets/K", nil),
		identity.Principal{TenantID: "alpha", Role: identity.RoleAdmin})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != 204 {
		t.Fatalf("delete: code=%d want 204", rec.Code)
	}
	if _, ok := sa.set["alpha"]["K"]; ok {
		t.Fatal("secret not deleted")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./controlplane/ -run TestSecretAdmin 2>&1 | head`
Expected: FAIL — `undefined: SecretAdmin` / `RegisterSecretAdmin`.

- [ ] **Step 3: Add SecretAdmin interface, name validation, and routes**

In `controlplane/admin.go`, add `"regexp"` to imports, then add the interface, the validator, and the registration function:

```go
// SecretAdmin is the write surface for tenant secrets, implemented by
// *identity.Broker (it seals before persisting). A nil SecretAdmin means the
// feature is disabled (no master key) and handlers return 503.
type SecretAdmin interface {
	SetSecret(ctx context.Context, tenant, name, plaintext string) error
	ListSecretNames(ctx context.Context, tenant string) ([]identity.SecretMeta, error)
	DeleteSecret(ctx context.Context, tenant, name string) error
}

// envNameRe restricts secret names to valid env-var identifiers so an injected
// var can't smuggle '=' or newlines into the child environment.
var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// RegisterSecretAdmin mounts /admin/secrets on mux. store is reused only for
// effectiveTenant's tenant validation. When sa is nil the handlers return 503.
func RegisterSecretAdmin(mux *http.ServeMux, store AdminStore, sa SecretAdmin) {
	mux.HandleFunc("POST /admin/secrets", func(w http.ResponseWriter, r *http.Request) {
		p, ok := requireAdmin(w, r)
		if !ok {
			return
		}
		if sa == nil {
			http.Error(w, "secrets not configured", http.StatusServiceUnavailable)
			return
		}
		var body struct{ Name, Value, Tenant string }
		if !decode(w, r, &body) {
			return
		}
		if body.Value == "" || !envNameRe.MatchString(body.Name) {
			http.Error(w, "valid name (env identifier) and non-empty value required", http.StatusBadRequest)
			return
		}
		tenant, ok := effectiveTenant(w, r, store, p, body.Tenant)
		if !ok {
			return
		}
		if err := sa.SetSecret(r.Context(), tenant, body.Name, body.Value); err != nil {
			serverError(w, "set secret", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"name": body.Name})
	})

	mux.HandleFunc("GET /admin/secrets", func(w http.ResponseWriter, r *http.Request) {
		p, ok := requireAdmin(w, r)
		if !ok {
			return
		}
		if sa == nil {
			http.Error(w, "secrets not configured", http.StatusServiceUnavailable)
			return
		}
		metas, err := sa.ListSecretNames(r.Context(), p.TenantID)
		if err != nil {
			serverError(w, "list secrets", err)
			return
		}
		writeJSON(w, http.StatusOK, metas)
	})

	mux.HandleFunc("DELETE /admin/secrets/{name}", func(w http.ResponseWriter, r *http.Request) {
		p, ok := requireAdmin(w, r)
		if !ok {
			return
		}
		if sa == nil {
			http.Error(w, "secrets not configured", http.StatusServiceUnavailable)
			return
		}
		if err := sa.DeleteSecret(r.Context(), p.TenantID, r.PathValue("name")); err != nil {
			serverError(w, "delete secret", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
```

Note on the GET/DELETE 503 ordering: the spec says all verbs return 503 when disabled. The handlers check `requireAdmin` first (so an unauthenticated caller still gets 401/403, consistent with the rest of `/admin`), then the 503 gate. This matches the POST test which uses an admin principal.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./controlplane/ -run 'TestSecretAdmin|TestAdmin' -v 2>&1 | tail -30`
Expected: PASS — all new secret-admin tests plus the existing admin tests.

- [ ] **Step 5: Build, vet, commit**

```bash
go build ./... && go vet ./controlplane/
git add controlplane/admin.go controlplane/admin_test.go
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "feat(controlplane): /admin/secrets write-only API (403/503/400 guards)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 7: runtimed wiring (RUNTIME_SECRETS_KEY)

**Files:**
- Modify: `cmd/runtimed/main.go`

This task has no new unit test (it is process wiring covered end-to-end by Task 9). Verify by build + a manual smoke check.

- [ ] **Step 1: Add a helper to build the broker from the env**

In `cmd/runtimed/main.go`, add `"encoding/base64"` to imports. Add this function near `envOr` at the bottom:

```go
// buildSecretBroker constructs a secret broker from RUNTIME_SECRETS_KEY (base64
// of 32 bytes) over the identity store. Returns:
//   - (nil, nil)        when the key is unset → feature disabled (back-compat)
//   - fatal (os.Exit)   when the key is set but malformed (operator error)
//   - (*Broker, ...)    when the key is valid
func buildSecretBroker(idStore *identity.Store) *identity.Broker {
	raw := os.Getenv("RUNTIME_SECRETS_KEY")
	if raw == "" {
		slog.Info("secrets brokering disabled: RUNTIME_SECRETS_KEY not set")
		return nil
	}
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		slog.Error("RUNTIME_SECRETS_KEY is not valid base64", "err", err)
		os.Exit(1)
	}
	cipher, err := identity.NewCipher(key)
	if err != nil {
		slog.Error("RUNTIME_SECRETS_KEY invalid", "err", err)
		os.Exit(1)
	}
	slog.Info("secrets brokering enabled")
	return identity.NewBroker(idStore, cipher)
}
```

- [ ] **Step 2: Build the broker and wire it into the registry + admin**

The broker needs the identity store, which today is created only inside the `else` (identity-on) branch. Secrets brokering must work whenever a master key is set — including before any tenant exists. Restructure so the store is always available when a key is present.

Replace the identity block. After `idStore, err := identity.NewStore(ctx, identityDB)` and its error check (lines ~66-70), the store exists regardless of mode. Add the broker right after it and wire it into the registry:

```go
	// Secret broker (Identity M2): built whenever RUNTIME_SECRETS_KEY is set,
	// independent of whether identity enforcement is on. Injected into the
	// registry so each agent's SpawnFunc brokers its tenant's secrets.
	secretBroker := buildSecretBroker(idStore)
	if secretBroker != nil {
		reg.SetBroker(secretBroker)
	}
```

Then thread the broker into `buildRoot` so the admin API can mount `/admin/secrets`. Change the two `buildRoot(...)` call sites and the function signature.

In the open-mode branch:

```go
		handler = accessLog(buildRoot(reg, nil, console.OIDCConfig{}, secretBroker)) // no /admin store in open mode; secrets API still gated by nil store→no mount
```

In the identity-on branch:

```go
		root := buildRoot(reg, idStore, consoleOIDC, secretBroker)
```

Update `buildRoot`:

```go
// buildRoot assembles the root mux: console at /ui, control-plane API at /, and
// (when adminS is non-nil) the admin API at /admin. The secret admin is mounted
// alongside the admin API; a nil secretBroker makes /admin/secrets return 503.
func buildRoot(reg *controlplane.Registry, adminS controlplane.AdminStore, consoleOIDC console.OIDCConfig, secretBroker controlplane.SecretAdmin) http.Handler {
	apiMux := controlplane.NewAPI(reg)
	if adminS != nil {
		controlplane.RegisterAdmin(apiMux, adminS)
		controlplane.RegisterSecretAdmin(apiMux, adminS, secretBroker)
	}
	consoleH := console.Handler(reg, consoleOIDC)
	root := http.NewServeMux()
	root.Handle("/ui", consoleH)
	root.Handle("/ui/", consoleH)
	root.Handle("/", apiMux)
	return root
}
```

Note: `secretBroker` is a `*identity.Broker`, which satisfies both `controlplane.SecretBroker` (passed to `reg.SetBroker`) and `controlplane.SecretAdmin` (passed to `buildRoot`). When it is nil, `RegisterSecretAdmin` still mounts the routes but they return 503 — but in **open mode** `adminS` is nil so the whole `/admin` surface (including secrets) is unmounted, which is correct (no admin without identity). Secret brokering into spawns still works in open mode because `reg.SetBroker` is independent of the admin mount.

- [ ] **Step 3: Build and vet**

Run: `go build ./... && go vet ./cmd/runtimed/`
Expected: no output (success).

- [ ] **Step 4: Smoke-check the three startup modes**

Run (no key → disabled):
```bash
RUNTIME_CONFIG=runtime.yaml RUNTIME_SECRETS_KEY= go run ./cmd/runtimed 2>&1 | grep -m1 "secrets brokering" &
sleep 2; kill %1 2>/dev/null
```
Expected: a line `secrets brokering disabled: RUNTIME_SECRETS_KEY not set`.

Run (malformed key → fatal):
```bash
RUNTIME_SECRETS_KEY="not-base64!!" go run ./cmd/runtimed 2>&1 | grep -m1 -i "secrets_key\|not valid base64"
```
Expected: an error line about base64 and a non-zero exit. (`echo $?` is not meaningful through the pipe; the grep match is the assertion.)

Run (valid key → enabled):
```bash
RUNTIME_SECRETS_KEY="$(head -c32 /dev/urandom | base64)" go run ./cmd/runtimed 2>&1 | grep -m1 "secrets brokering enabled" &
sleep 3; kill %1 2>/dev/null
```
Expected: a line `secrets brokering enabled`.

- [ ] **Step 5: Commit**

```bash
git add cmd/runtimed/main.go
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "feat(runtimed): wire secret broker from RUNTIME_SECRETS_KEY

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 8: runtimectl admin secret commands

**Files:**
- Modify: `cmd/runtimectl/main.go`

- [ ] **Step 1: Add the `secret` verb to runAdmin**

In `cmd/runtimectl/main.go`, inside `runAdmin`'s `switch args[0]+" "+args[1]` block, add cases before `default:`:

```go
	case "secret set":
		// admin secret set <name> <value> [--tenant t]
		if len(args) < 4 {
			adminUsage()
		}
		name, value := args[2], args[3]
		tenant := flagValue(args[4:], "--tenant", "")
		mustAdminPost(base, "/admin/secrets", map[string]string{"name": name, "value": value, "tenant": tenant})
		fmt.Printf("secret %s set\n", name)
	case "secret ls":
		fmt.Print(string(mustAdminGet(base, "/admin/secrets")))
	case "secret rm":
		// admin secret rm <name>
		if len(args) < 3 {
			adminUsage()
		}
		mustAdminDelete(base, "/admin/secrets/"+args[2])
		fmt.Printf("secret %s removed\n", args[2])
```

- [ ] **Step 2: Update the admin usage string**

Replace the `adminUsage` body string to include the secret subcommands:

```go
func adminUsage() {
	fmt.Fprintln(os.Stderr, "usage: runtimectl admin <tenant create <id> [--name n]|user add <subject> --role r [--tenant t]|user ls|key create --role r [--label l] [--tenant t]|key ls|key revoke <id>|secret set <name> <value> [--tenant t]|secret ls|secret rm <name>>")
	os.Exit(2)
}
```

- [ ] **Step 3: Build and vet**

Run: `go build ./... && go vet ./cmd/runtimectl/`
Expected: no output (success).

- [ ] **Step 4: Smoke-check the command dispatch (no server needed)**

Run: `go run ./cmd/runtimectl admin secret 2>&1 | head -1`
Expected: the usage line (too few args → `adminUsage`).

- [ ] **Step 5: Commit**

```bash
git add cmd/runtimectl/main.go
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "feat(runtimectl): admin secret set/ls/rm

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 9: end-to-end two-tenant spawn isolation

**Files:**
- Create: `test/secrets_e2e_test.go`

This is the headline test: it proves the whole chain — broker over a real cipher + Postgres store → `Registry.SetBroker` → `AgentProcess.SpawnFunc` → a real child process that writes its environment to a file → correct per-tenant value, isolation, and no-secret fallback.

- [ ] **Step 1: Write the failing E2E test**

Create `test/secrets_e2e_test.go`:

```go
//go:build integration

package test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/sausheong/runtime/controlplane"
	"github.com/sausheong/runtime/internal/config"
	"github.com/sausheong/runtime/internal/identity"
)

// spawnAndWaitEnv runs an agent's SpawnFunc with a Command that dumps its env to
// outPath, waits for exit, and returns the file contents.
func spawnAndWaitEnv(t *testing.T, ap controlplane.AgentProcess, outPath string) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wait := ap.SpawnFunc()(ctx)
	select {
	case err := <-wait:
		if err != nil {
			t.Fatalf("spawn exited with error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("spawn did not exit in time")
	}
	b, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read env dump: %v", err)
	}
	return string(b)
}

func TestSecretsE2E_PerTenantInjection(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Skipf("postgres not reachable: %v", err)
	}
	for _, q := range []string{
		`DROP TABLE IF EXISTS secrets CASCADE`,
		`DROP TABLE IF EXISTS service_keys CASCADE`,
		`DROP TABLE IF EXISTS identity_users CASCADE`,
		`DROP TABLE IF EXISTS tenants CASCADE`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		cdb, err := sql.Open("pgx", dsn)
		if err != nil {
			return
		}
		defer cdb.Close()
		for _, q := range []string{
			`DROP TABLE IF EXISTS secrets CASCADE`,
			`DROP TABLE IF EXISTS service_keys CASCADE`,
			`DROP TABLE IF EXISTS identity_users CASCADE`,
			`DROP TABLE IF EXISTS tenants CASCADE`,
		} {
			_, _ = cdb.Exec(q)
		}
	})

	st, err := identity.NewStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	cipher, err := identity.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	broker := identity.NewBroker(st, cipher)

	// Two tenants with their own OPENAI_API_KEY; a third with none.
	for _, tn := range []string{"alpha", "beta", "gamma"} {
		if err := st.CreateTenant(ctx, tn, tn); err != nil {
			t.Fatal(err)
		}
	}
	if err := broker.SetSecret(ctx, "alpha", "OPENAI_API_KEY", "sk-alpha"); err != nil {
		t.Fatal(err)
	}
	if err := broker.SetSecret(ctx, "beta", "OPENAI_API_KEY", "sk-beta"); err != nil {
		t.Fatal(err)
	}

	// Build a registry with three agents (one per tenant) via the generalized
	// command path, each dumping env to its own file.
	dir := t.TempDir()
	outFile := func(tn string) string { return filepath.Join(dir, tn+".env") }
	mkAgent := func(tn string) config.AgentConfig {
		out := outFile(tn)
		return config.AgentConfig{
			ID:         "agent-" + tn,
			ListenAddr: "127.0.0.1:0",
			Tenant:     tn,
			Command:    []string{"sh", "-c", "env > " + out},
		}
	}
	cfg := &config.Config{Agents: []config.AgentConfig{mkAgent("alpha"), mkAgent("beta"), mkAgent("gamma")}}
	reg := controlplane.NewRegistry(cfg, "./agentd", dsn)
	reg.SetBroker(broker)

	// Ensure the inherited env carries a sentinel the no-secret tenant falls back to.
	t.Setenv("OPENAI_API_KEY", "sk-operator-fallback")

	apAlpha, _ := reg.Get("agent-alpha")
	apBeta, _ := reg.Get("agent-beta")
	apGamma, _ := reg.Get("agent-gamma")

	envAlpha := spawnAndWaitEnv(t, apAlpha, outFile("alpha"))
	envBeta := spawnAndWaitEnv(t, apBeta, outFile("beta"))
	envGamma := spawnAndWaitEnv(t, apGamma, outFile("gamma"))

	if !strings.Contains(envAlpha, "OPENAI_API_KEY=sk-alpha") {
		t.Fatalf("alpha did not get its secret:\n%s", envAlpha)
	}
	if !strings.Contains(envBeta, "OPENAI_API_KEY=sk-beta") {
		t.Fatalf("beta did not get its secret:\n%s", envBeta)
	}
	// Isolation: alpha must not see beta's value and vice versa.
	if strings.Contains(envAlpha, "sk-beta") || strings.Contains(envBeta, "sk-alpha") {
		t.Fatal("cross-tenant secret leak")
	}
	// Fallback: gamma has no secret → inherits the operator env value.
	if !strings.Contains(envGamma, "OPENAI_API_KEY=sk-operator-fallback") {
		t.Fatalf("gamma did not fall back to operator env:\n%s", envGamma)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails (then passes once everything is wired)**

Run: `go test -tags integration ./test/ -run TestSecretsE2E -v 2>&1 | tail -30`
Expected: With Tasks 1-6 complete, PASS. If postgres is down it SKIPs.

Note on env-dump shadowing: `sh -c "env"` prints the process environment. Because `buildEnv` appends the tenant secret AFTER the inherited `OPENAI_API_KEY`, the child's environment has the tenant value as the effective one. `env` may print only the last assignment per name (exec dedups), so the assertion checks for the tenant value's presence; the cross-leak check guards the negative.

- [ ] **Step 3: Run the full hermetic + both integration packages**

```bash
go build ./... && go vet ./...
go test ./...                                   # hermetic, all green
go test -tags integration ./internal/identity/  # sequential — drops identity tables
go test -tags integration ./test/               # sequential — drops identity tables
```
Expected: all PASS (integration SKIPs if Postgres is unreachable).

- [ ] **Step 4: Commit**

```bash
git add test/secrets_e2e_test.go
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "test(secrets): two-tenant spawn-isolation E2E (set→encrypt→spawn→env)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 10: documentation

**Files:**
- Modify: `README.md`
- Modify: `ROADMAP.md`
- Modify: `docs/images/project-layout.mmd` (+ regenerate `.png`)

- [ ] **Step 1: README — add a Secrets sub-section**

In `README.md`, under the "Authentication & multi-tenancy" section, add a "Per-tenant secrets" subsection. Find the section (search for `## Authentication` or `#authentication--multi-tenancy`) and add after the service-keys content:

```markdown
### Per-tenant secrets (provider credentials)

Each tenant can store its own provider credentials (e.g. `OPENAI_API_KEY`),
encrypted at rest and injected as environment variables into that tenant's agent
subprocesses at spawn time — agents read `os.Getenv` unchanged.

Enable the feature by setting a 32-byte master key (base64):

```bash
export RUNTIME_SECRETS_KEY="$(head -c32 /dev/urandom | base64)"
```

When `RUNTIME_SECRETS_KEY` is unset the feature is disabled and agents inherit
the operator's environment (the prior behavior).

Manage secrets with `runtimectl` (admin role, scoped to your tenant):

```bash
runtimectl admin secret set OPENAI_API_KEY sk-xxxxxxxx   # set/overwrite
runtimectl admin secret ls                               # names + timestamps (never values)
runtimectl admin secret rm OPENAI_API_KEY                # delete
```

Secrets are **write-only**: the API never returns a stored value. Values are
encrypted with AES-256-GCM under the operator master key. A secret change takes
effect on the agent's **next restart** (resolution happens at spawn).

> **Security:** the master key lives in runtimed's environment (operator-managed,
> like the Postgres DSN). Losing it makes existing ciphertext unrecoverable.
> The POST body carries the plaintext once — terminate TLS in front of runtimed.
> No key rotation in this milestone.
```

- [ ] **Step 2: README — env-var and CLI tables**

Add `RUNTIME_SECRETS_KEY` to the environment-variable table (search for `RUNTIME_ADMIN_BOOTSTRAP` to find it):

```markdown
| `RUNTIME_SECRETS_KEY` | base64 of 32 bytes; enables per-tenant secrets brokering (unset ⇒ disabled) |
```

Add the secret commands to the CLI table (search for `admin key revoke` or the admin command rows):

```markdown
| `runtimectl admin secret set <name> <value> [--tenant t]` | Set/overwrite a tenant secret |
| `runtimectl admin secret ls` | List secret names + timestamps (never values) |
| `runtimectl admin secret rm <name>` | Delete a tenant secret |
```

- [ ] **Step 3: ROADMAP — mark secrets brokering done**

In `ROADMAP.md`, update the `## Current state` block and §B3. In §B3's "Remaining Identity work" list (search for `**Remaining Identity`), remove "secrets brokering (per-tenant provider keys → agents)" from the remaining list and add a done-note paragraph after the M1 description:

```markdown
   **Second milestone DONE (merged to `master`, 2026-06-09):** per-tenant
   secrets brokering. Tenants store provider credentials (generic named env
   vars) encrypted at rest with AES-256-GCM under an operator master key
   (`RUNTIME_SECRETS_KEY`); a `Broker` in `internal/identity` decrypts them at
   spawn time and the registry injects them into the tenant's agent
   subprocesses' environment (shadowing the inherited operator env). Write-only
   `/admin/secrets` API + `runtimectl admin secret set/ls/rm`. Disabled (and
   fully backward-compatible) when no master key is set. Agents stay unmodified.
   Spec/plan: `docs/superpowers/{specs,plans}/2026-06-09-identity-m2-secrets-brokering*`.
```

Also update the top-of-file `**Current state:**` line to mention the M2 milestone, and trim "secrets brokering" from any remaining-work bullet.

- [ ] **Step 4: project-layout diagram**

In `docs/images/project-layout.mmd`, update the `ident` node description to mention secrets:

```
    ident["identity/<br/><i>multi-tenant authn/authz +<br/>secrets brokering:<br/>Principal, Authorizer, Authenticator,<br/>OIDC, Cipher, Broker, store</i>"]
```

Regenerate the PNG:

```bash
cd docs/images && mmdc -i project-layout.mmd -o project-layout.png -t neutral -b white -s 3 && cd ../..
```

If `mmdc` is unavailable, note it in the commit and skip the PNG (the `.mmd` source is the source of truth).

- [ ] **Step 5: Commit docs**

```bash
git add README.md ROADMAP.md docs/images/project-layout.mmd docs/images/project-layout.png
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "docs(identity): document secrets brokering (README, ROADMAP, layout)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Final verification (before finishing-a-development-branch)

- [ ] `go build ./...` — clean
- [ ] `go vet ./...` — clean
- [ ] `go test ./...` — all hermetic tests pass
- [ ] `go test -tags integration ./internal/identity/` — passes (or SKIPs without PG)
- [ ] `go test -tags integration ./test/` — passes (or SKIPs without PG)
- [ ] Spot-check: `RUNTIME_SECRETS_KEY` unset ⇒ startup logs "secrets brokering disabled"; set+valid ⇒ "secrets brokering enabled"; malformed ⇒ fatal.

Then use **superpowers:finishing-a-development-branch** to merge `feat/identity-m2-secrets` to `master`.
