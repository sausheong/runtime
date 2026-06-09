# Identity M3 — Secrets Key Rotation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add operator-driven secrets master-key rotation — a multi-key keyring with self-describing versioned ciphertext blobs, AAD-bound to `(tenant, name)`, plus an explicit re-runnable re-encrypt pass — while keeping every M2 deployment working unchanged.

**Architecture:** Two crypto layers inside `internal/identity`: `Cipher` (single-key AES-256-GCM with AAD, nonce-prepended) and a new `Keyring` (owns the `0x01|keyIDLen|keyID|nonce|ct` blob format, key selection by embedded ID, primary-key sealing, and legacy-row fallback). The `Broker` switches from one `Cipher` to one `Keyring`, threads `(tenant, name)` into every seal/open, and gains `Rotate`. A new `POST /admin/secrets/rotate` endpoint + `runtimectl admin secret rotate` drive it. runtimed builds the keyring from `RUNTIME_SECRETS_KEYS`/`RUNTIME_SECRETS_PRIMARY` (with the lone `RUNTIME_SECRETS_KEY` as the back-compat single key).

**Tech Stack:** Go 1.25.1, stdlib `crypto/aes`+`crypto/cipher`, Postgres (integration tests only), module `github.com/sausheong/runtime` with `replace ../harness`.

**Spec:** `docs/superpowers/specs/2026-06-09-identity-m3-secrets-key-rotation-design.md`

---

## Conventions (read before starting)

- **`go` CLI is ground truth.** The IDE/LSP is broken by the `replace ../harness` cross-module setup — ignore its diagnostics; trust `go build ./...`, `go vet ./...`, `go test ./...`.
- **Hermetic tests** run via `go test ./...`. **Integration tests** are `//go:build integration` and need local Postgres.app at `postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable`. Run the integration packages **individually**, not via `./...`:
  - `go test -tags integration ./internal/identity/`
  - `go test -tags integration ./test/`
- **Commits** must use the project identity and trailer:
  ```bash
  git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' \
    commit -m "<message>

  Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
  ```
- **Branch:** all work lands on `feat/identity-m3-key-rotation` (already created; the spec commit is its first commit).
- **Never log or return a secret value.** Errors carry tenant + name + key id only.

---

## File Structure

| File | Change | Responsibility after this milestone |
|---|---|---|
| `internal/identity/crypto.go` | modify | `Cipher`: single-key AES-256-GCM, `Seal(pt, aad)` / `Open(blob, aad)`, nonce-prepended. |
| `internal/identity/crypto_test.go` | modify | Cipher round-trip incl. AAD match/mismatch, key-size, nonce, tamper, wrong-key. |
| `internal/identity/keyring.go` | **create** | `Keyring` (blob format `0x01\|idLen\|id\|nonce\|ct`, key selection, legacy fallback) + `ParseKeyring` env parsing. |
| `internal/identity/keyring_test.go` | **create** | Keyring seal/open, multi-key, legacy (incl. `0x01`-nonce collision), unknown-id; `ParseKeyring` cases. |
| `internal/identity/broker.go` | modify | `Broker` holds a `*Keyring`; threads `(tenant,name)`; adds `Rotate` + `RotateStats`. |
| `internal/identity/broker_test.go` | modify | Fake-store tests for keyring-backed broker + `Rotate`. |
| `internal/identity/secrets_store_test.go` | modify (integration) | `Rotate` over real Postgres incl. a hand-written legacy row. |
| `controlplane/admin.go` | modify | `SecretAdmin.RotateSecrets`; `AdminStore.ListTenants`; `POST /admin/secrets/rotate`. |
| `controlplane/admin_test.go` | modify | rotate-endpoint guards + superuser all-tenants fan-out. |
| `cmd/runtimectl/main.go` | modify | `admin secret rotate [--tenant t]`. |
| `cmd/runtimed/main.go` | modify | `buildSecretBroker` builds a `Keyring` via `ParseKeyring`. |
| `test/secrets_e2e_test.go` | modify (integration) | End-to-end rotation: old → add primary → rotate → retire old key. |
| `README.md`, `ROADMAP.md`, `docs/images/project-layout.mmd` (+`.png`), memory | docs | Document keyring, rotate CLI, AAD, retire workflow; mark M3 done. |

**Green-between-tasks ordering note:** Task 1 changes `Cipher.Seal/Open` signatures (only `broker.go` + tests call them — both updated in T1). Tasks 2–3 are purely additive (`keyring.go`). Task 4 is the atomic switch of `Broker`/`NewBroker` to `*Keyring`, which forces updating its three call sites (broker tests, `runtimed`, the E2E test construction) in the same commit. Everything after T4 is additive.

---

### Task 1: Cipher gains AAD

**Files:**
- Modify: `internal/identity/crypto.go`
- Modify: `internal/identity/crypto_test.go`
- Modify: `internal/identity/broker.go:33-59` (only caller; pass `nil` AAD for now — behavior preserved)
- Modify: `internal/identity/broker_test.go:79` (`b.cipher.Seal` call)

- [ ] **Step 1: Rewrite the Cipher tests to use the AAD-aware signature**

Replace the body of `internal/identity/crypto_test.go` with (keeps `key32()`, updates calls, adds AAD coverage):

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
		ct, err := c.Seal(pt, nil)
		if err != nil {
			t.Fatalf("seal: %v", err)
		}
		got, err := c.Open(ct, nil)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		if !bytes.Equal(got, pt) {
			t.Fatalf("round-trip mismatch: got %q want %q", got, pt)
		}
	}
}

func TestCipher_AADMustMatch(t *testing.T) {
	c, _ := NewCipher(key32())
	ct, err := c.Seal([]byte("secret"), []byte("alpha\x00OPENAI_API_KEY"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Open(ct, []byte("alpha\x00OPENAI_API_KEY")); err != nil {
		t.Fatalf("open with matching AAD failed: %v", err)
	}
	if _, err := c.Open(ct, []byte("beta\x00OPENAI_API_KEY")); err == nil {
		t.Fatal("open with mismatched AAD succeeded, want error")
	}
	if _, err := c.Open(ct, nil); err == nil {
		t.Fatal("open with nil AAD over AAD-sealed blob succeeded, want error")
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
	a, _ := c.Seal(pt, nil)
	b, _ := c.Seal(pt, nil)
	if bytes.Equal(a, b) {
		t.Fatal("two seals of same plaintext produced identical ciphertext (nonce not random)")
	}
}

func TestCipher_OpenRejectsTampered(t *testing.T) {
	c, _ := NewCipher(key32())
	ct, _ := c.Seal([]byte("secret"), nil)
	ct[len(ct)-1] ^= 0xff
	if _, err := c.Open(ct, nil); err == nil {
		t.Fatal("Open accepted tampered ciphertext")
	}
	if _, err := c.Open([]byte{1, 2, 3}, nil); err == nil {
		t.Fatal("Open accepted too-short input")
	}
}

func TestCipher_OpenRejectsWrongKey(t *testing.T) {
	c1, _ := NewCipher(key32())
	other := key32()
	other[0] ^= 0xff
	c2, _ := NewCipher(other)
	ct, _ := c1.Seal([]byte("secret"), nil)
	if _, err := c2.Open(ct, nil); err == nil {
		t.Fatal("Open with wrong key succeeded")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail to compile**

Run: `go test ./internal/identity/ -run TestCipher`
Expected: FAIL — `too many arguments in call to c.Seal` (signature not yet changed).

- [ ] **Step 3: Change `Cipher.Seal`/`Open` to take AAD**

In `internal/identity/crypto.go`, replace the `Seal` and `Open` methods (lines 36-54) with:

```go
// Seal returns nonce || GCM(plaintext) binding aad as additional authenticated
// data (aad may be nil). The plaintext may be any bytes.
func (c *Cipher) Seal(plaintext, aad []byte) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return c.aead.Seal(nonce, nonce, plaintext, aad), nil
}

// Open reverses Seal. aad must equal what Seal bound (nil if none). It errors on
// a too-short input or any authentication failure (tampering / wrong key / wrong
// aad).
func (c *Cipher) Open(blob, aad []byte) ([]byte, error) {
	ns := c.aead.NonceSize()
	if len(blob) < ns {
		return nil, errors.New("identity: ciphertext shorter than nonce")
	}
	nonce, ct := blob[:ns], blob[ns:]
	return c.aead.Open(nil, nonce, ct, aad)
}
```

Update the `Cipher` doc comment (lines 12-14) to:

```go
// Cipher seals and opens values with AES-256-GCM under a single key. Each Seal
// prepends a fresh random 12-byte nonce and binds the caller's AAD; Open expects
// that layout and the same AAD. The Keyring composes Ciphers into a versioned,
// multi-key blob format; callers outside this package use the Broker.
```

- [ ] **Step 4: Keep the sole caller compiling (pass nil AAD for now)**

In `internal/identity/broker.go`, update the two cipher calls so the package builds (AAD threading lands in Task 4):
- In `SecretsFor`, change `b.cipher.Open(e.ValueEnc)` → `b.cipher.Open(e.ValueEnc, nil)`.
- In `SetSecret`, change `b.cipher.Seal([]byte(plaintext))` → `b.cipher.Seal([]byte(plaintext), nil)`.

In `internal/identity/broker_test.go`, update line 79: `b.cipher.Seal([]byte("sk-good"))` → `b.cipher.Seal([]byte("sk-good"), nil)`.

- [ ] **Step 5: Run the full identity unit suite**

Run: `go test ./internal/identity/`
Expected: PASS (all existing broker tests still green; new AAD test green).

- [ ] **Step 6: Build everything**

Run: `go build ./...`
Expected: clean (no other callers of `Seal`/`Open`).

- [ ] **Step 7: Commit**

```bash
git add internal/identity/crypto.go internal/identity/crypto_test.go internal/identity/broker.go internal/identity/broker_test.go
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "feat(identity): Cipher binds AAD on seal/open

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: Keyring type + versioned blob format

**Files:**
- Create: `internal/identity/keyring.go`
- Create: `internal/identity/keyring_test.go`

The blob format is `0x01 | keyIDLen(1) | keyID | nonce(12) | GCM-ct`, AAD = `tenant\x00name`. A blob whose first byte is not `0x01` is a legacy M2 row (`nonce||ct`, nil AAD). **Open is robust to a legacy nonce whose first byte is `0x01`:** it tries the v1 parse and, on any failure, falls back to legacy decrypt — GCM authentication makes a wrong-path decrypt impossible to succeed, so the fallback only ever rescues a genuine legacy row.

- [ ] **Step 1: Write the failing keyring tests**

Create `internal/identity/keyring_test.go`:

```go
package identity

import (
	"bytes"
	"testing"
)

func mkCipher(t *testing.T, seed byte) *Cipher {
	t.Helper()
	k := make([]byte, 32)
	for i := range k {
		k[i] = seed + byte(i)
	}
	c, err := NewCipher(k)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestKeyring_SealOpenRoundTrip(t *testing.T) {
	kr, err := NewKeyring(map[string]*Cipher{"v1": mkCipher(t, 1)}, "v1", "v1")
	if err != nil {
		t.Fatal(err)
	}
	blob, err := kr.Seal("alpha", "OPENAI_API_KEY", []byte("sk-xyz"))
	if err != nil {
		t.Fatal(err)
	}
	if blob[0] != 0x01 {
		t.Fatalf("new blob must start with 0x01, got 0x%02x", blob[0])
	}
	idLen := int(blob[1])
	if string(blob[2:2+idLen]) != "v1" {
		t.Fatalf("embedded key id = %q, want v1", blob[2:2+idLen])
	}
	got, err := kr.Open("alpha", "OPENAI_API_KEY", blob)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "sk-xyz" {
		t.Fatalf("round-trip mismatch: %q", got)
	}
}

func TestKeyring_OpenWrongTenantOrNameFails(t *testing.T) {
	kr, _ := NewKeyring(map[string]*Cipher{"v1": mkCipher(t, 1)}, "v1", "v1")
	blob, _ := kr.Seal("alpha", "K", []byte("v"))
	if _, err := kr.Open("beta", "K", blob); err == nil {
		t.Fatal("open with wrong tenant succeeded (AAD not bound)")
	}
	if _, err := kr.Open("alpha", "OTHER", blob); err == nil {
		t.Fatal("open with wrong name succeeded (AAD not bound)")
	}
}

func TestKeyring_MixedKeyVersions(t *testing.T) {
	cOld, cNew := mkCipher(t, 1), mkCipher(t, 9)
	krOld, _ := NewKeyring(map[string]*Cipher{"v1": cOld}, "v1", "v1")
	blobV1, _ := krOld.Seal("alpha", "K", []byte("old"))

	// Primary moves to v2; v1 still present for decrypt.
	krBoth, _ := NewKeyring(map[string]*Cipher{"v1": cOld, "v2": cNew}, "v2", "v1")
	got, err := krBoth.Open("alpha", "K", blobV1)
	if err != nil {
		t.Fatalf("v1 blob must still open under a v2-primary ring: %v", err)
	}
	if string(got) != "old" {
		t.Fatalf("mixed-version mismatch: %q", got)
	}
	blobV2, _ := krBoth.Seal("alpha", "K", []byte("new"))
	if int(blobV2[1]) != 2 || string(blobV2[2:2+2]) != "v2" {
		t.Fatalf("new seal must use primary v2, got id %q", blobV2[2:2+int(blobV2[1])])
	}
}

func TestKeyring_UnknownKeyID(t *testing.T) {
	cOld := mkCipher(t, 1)
	krOld, _ := NewKeyring(map[string]*Cipher{"v1": cOld}, "v1", "v1")
	blob, _ := krOld.Seal("alpha", "K", []byte("x"))
	// A ring without v1 (and no legacy) cannot open the v1 blob.
	krNew, _ := NewKeyring(map[string]*Cipher{"v2": mkCipher(t, 9)}, "v2", "")
	if _, err := krNew.Open("alpha", "K", blob); err == nil {
		t.Fatal("open with unknown key id succeeded, want error")
	}
}

func TestKeyring_LegacyVersionlessBlob(t *testing.T) {
	cLegacy := mkCipher(t, 1)
	kr, _ := NewKeyring(map[string]*Cipher{"v1": cLegacy}, "v1", "v1")
	// A legacy M2 blob is exactly Cipher.Seal(pt, nil): nonce||ct, no 0x01 prefix.
	legacy, _ := cLegacy.Seal([]byte("sk-legacy"), nil)
	got, err := kr.Open("alpha", "K", legacy)
	if err != nil {
		t.Fatalf("legacy blob must open via the version-less path: %v", err)
	}
	if string(got) != "sk-legacy" {
		t.Fatalf("legacy decrypt mismatch: %q", got)
	}
}

func TestKeyring_LegacyBlobWithLeading01Byte(t *testing.T) {
	cLegacy := mkCipher(t, 1)
	kr, _ := NewKeyring(map[string]*Cipher{"v1": cLegacy}, "v1", "v1")
	// Find a legacy blob whose random nonce happens to start with 0x01, which
	// would otherwise be misdetected as new-format. Open must fall back.
	var legacy []byte
	for i := 0; i < 100000; i++ {
		b, _ := cLegacy.Seal([]byte("sk-collide"), nil)
		if b[0] == 0x01 {
			legacy = b
			break
		}
	}
	if legacy == nil {
		t.Skip("no 0x01-leading nonce in 100k tries (astronomically unlikely)")
	}
	got, err := kr.Open("alpha", "K", legacy)
	if err != nil {
		t.Fatalf("legacy blob with 0x01 nonce must open via fallback: %v", err)
	}
	if string(got) != "sk-collide" {
		t.Fatalf("fallback decrypt mismatch: %q", got)
	}
}

func TestKeyring_NoLegacyKeyRejectsVersionlessBlob(t *testing.T) {
	cLegacy := mkCipher(t, 1)
	legacy, _ := cLegacy.Seal([]byte("x"), nil)
	// Ring with no legacy id cannot open a version-less blob.
	kr, _ := NewKeyring(map[string]*Cipher{"v2": mkCipher(t, 9)}, "v2", "")
	if _, err := kr.Open("alpha", "K", legacy); err == nil {
		t.Fatal("version-less blob opened without a legacy key, want error")
	}
}

func TestNewKeyring_Validation(t *testing.T) {
	c := mkCipher(t, 1)
	if _, err := NewKeyring(map[string]*Cipher{}, "v1", ""); err == nil {
		t.Fatal("empty ring accepted")
	}
	if _, err := NewKeyring(map[string]*Cipher{"v1": c}, "v2", ""); err == nil {
		t.Fatal("primary not in ring accepted")
	}
	if _, err := NewKeyring(map[string]*Cipher{"v1": c}, "v1", "v9"); err == nil {
		t.Fatal("legacy id not in ring accepted")
	}
	kr, err := NewKeyring(map[string]*Cipher{"v1": c}, "v1", "v1")
	if err != nil {
		t.Fatal(err)
	}
	if kr.PrimaryID() != "v1" || kr.NumKeys() != 1 {
		t.Fatalf("accessor mismatch: primary=%q n=%d", kr.PrimaryID(), kr.NumKeys())
	}
	_ = bytes.Equal // keep import if unused above
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/identity/ -run TestKeyring`
Expected: FAIL — `undefined: NewKeyring`.

- [ ] **Step 3: Implement the Keyring core**

Create `internal/identity/keyring.go`:

```go
package identity

import (
	"errors"
	"fmt"
)

// formatV1 marks a new-format secret blob: 0x01 | keyIDLen(1) | keyID | nonce | ct.
// A blob whose first byte is not formatV1 is a legacy M2 row (nonce || ct, nil AAD).
const formatV1 byte = 0x01

// Keyring composes single-key Ciphers into a versioned, multi-key blob format.
// New blobs are sealed under the primary key and carry the key's id; opens select
// the key by the id embedded in the blob and bind (tenant, name) as AAD. A blob
// with no formatV1 prefix is opened with the optional legacy key and nil AAD.
type Keyring struct {
	ciphers   map[string]*Cipher
	primaryID string
	legacyID  string // "" when no legacy (version-less) decrypt key is configured
}

// NewKeyring validates that ciphers is non-empty, primaryID is present, and
// legacyID (if non-empty) is present.
func NewKeyring(ciphers map[string]*Cipher, primaryID, legacyID string) (*Keyring, error) {
	if len(ciphers) == 0 {
		return nil, errors.New("identity: keyring needs at least one key")
	}
	if _, ok := ciphers[primaryID]; !ok {
		return nil, fmt.Errorf("identity: primary key id %q not in keyring", primaryID)
	}
	if legacyID != "" {
		if _, ok := ciphers[legacyID]; !ok {
			return nil, fmt.Errorf("identity: legacy key id %q not in keyring", legacyID)
		}
	}
	return &Keyring{ciphers: ciphers, primaryID: primaryID, legacyID: legacyID}, nil
}

// PrimaryID is the id new blobs are sealed under.
func (k *Keyring) PrimaryID() string { return k.primaryID }

// NumKeys is the number of keys in the ring (for startup logging).
func (k *Keyring) NumKeys() int { return len(k.ciphers) }

func aadFor(tenant, name string) []byte {
	return []byte(tenant + "\x00" + name)
}

// Seal produces a new-format blob under the primary key, binding (tenant, name).
func (k *Keyring) Seal(tenant, name string, plaintext []byte) ([]byte, error) {
	c := k.ciphers[k.primaryID]
	body, err := c.Seal(plaintext, aadFor(tenant, name))
	if err != nil {
		return nil, err
	}
	id := []byte(k.primaryID)
	out := make([]byte, 0, 2+len(id)+len(body))
	out = append(out, formatV1, byte(len(id)))
	out = append(out, id...)
	out = append(out, body...)
	return out, nil
}

// Open reverses Seal. New-format blobs select the key by embedded id and bind
// (tenant, name) as AAD; on any new-format failure it falls back to the legacy
// path (this rescues a genuine legacy blob whose random nonce starts with the
// formatV1 byte — GCM auth makes a wrong-path success impossible). Version-less
// blobs go straight to the legacy key with nil AAD.
func (k *Keyring) Open(tenant, name string, blob []byte) ([]byte, error) {
	if len(blob) > 0 && blob[0] == formatV1 {
		pt, err := k.openV1(tenant, name, blob)
		if err == nil {
			return pt, nil
		}
		if k.legacyID != "" {
			if lpt, lerr := k.openLegacy(blob); lerr == nil {
				return lpt, nil
			}
		}
		return nil, err
	}
	return k.openLegacy(blob)
}

func (k *Keyring) openV1(tenant, name string, blob []byte) ([]byte, error) {
	if len(blob) < 2 {
		return nil, errors.New("identity: truncated v1 secret header")
	}
	idLen := int(blob[1])
	if idLen == 0 || len(blob) < 2+idLen {
		return nil, errors.New("identity: corrupt v1 secret key id")
	}
	id := string(blob[2 : 2+idLen])
	c, ok := k.ciphers[id]
	if !ok {
		return nil, fmt.Errorf("identity: unknown key id %q", id)
	}
	return c.Open(blob[2+idLen:], aadFor(tenant, name))
}

func (k *Keyring) openLegacy(blob []byte) ([]byte, error) {
	if k.legacyID == "" {
		return nil, errors.New("identity: version-less secret but no legacy key configured")
	}
	return k.ciphers[k.legacyID].Open(blob, nil)
}
```

- [ ] **Step 4: Run the keyring tests**

Run: `go test ./internal/identity/ -run TestKeyring`
Expected: PASS (all keyring + NewKeyring cases green).

- [ ] **Step 5: Build**

Run: `go build ./...`
Expected: clean.

- [ ] **Step 6: Commit**

```bash
git add internal/identity/keyring.go internal/identity/keyring_test.go
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "feat(identity): Keyring with versioned, AAD-bound, multi-key blob format

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: Keyring config parsing (`ParseKeyring`)

**Files:**
- Modify: `internal/identity/keyring.go` (add `ParseKeyring`)
- Modify: `internal/identity/keyring_test.go` (add parse tests)

`ParseKeyring(keysEnv, primaryEnv, legacyKeyEnv)`:
- All empty → `(nil, nil)` (feature disabled).
- `keysEnv` empty, `legacyKeyEnv` set → ring `{v1: key}`, primary `v1`, legacy `v1` (M2 back-compat).
- `keysEnv` set (`"id:b64,id:b64"`) → parse pairs (32-byte keys, unique ids); require `primaryEnv` ∈ ring; if `legacyKeyEnv` set, its bytes must equal exactly one ring entry → that id becomes legacy.

- [ ] **Step 1: Write the failing parse tests**

Append to `internal/identity/keyring_test.go`:

```go
import "encoding/base64" // add to the existing import block

func b64key(seed byte) string {
	k := make([]byte, 32)
	for i := range k {
		k[i] = seed + byte(i)
	}
	return base64.StdEncoding.EncodeToString(k)
}

func TestParseKeyring_AllEmptyDisabled(t *testing.T) {
	kr, err := ParseKeyring("", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if kr != nil {
		t.Fatal("all-empty config must yield nil keyring (feature disabled)")
	}
}

func TestParseKeyring_LegacySingleKey(t *testing.T) {
	kr, err := ParseKeyring("", "", b64key(1))
	if err != nil {
		t.Fatal(err)
	}
	if kr.PrimaryID() != "v1" || kr.NumKeys() != 1 || kr.legacyID != "v1" {
		t.Fatalf("legacy single-key mismatch: primary=%q n=%d legacy=%q", kr.PrimaryID(), kr.NumKeys(), kr.legacyID)
	}
}

func TestParseKeyring_MultiKeyPrimary(t *testing.T) {
	kr, err := ParseKeyring("v1:"+b64key(1)+",v2:"+b64key(9), "v2", "")
	if err != nil {
		t.Fatal(err)
	}
	if kr.PrimaryID() != "v2" || kr.NumKeys() != 2 || kr.legacyID != "" {
		t.Fatalf("multi-key mismatch: primary=%q n=%d legacy=%q", kr.PrimaryID(), kr.NumKeys(), kr.legacyID)
	}
}

func TestParseKeyring_LegacyKeyNamesRingEntry(t *testing.T) {
	// RUNTIME_SECRETS_KEY equals the v1 entry's bytes ⇒ v1 is the legacy id.
	kr, err := ParseKeyring("v1:"+b64key(1)+",v2:"+b64key(9), "v2", b64key(1))
	if err != nil {
		t.Fatal(err)
	}
	if kr.legacyID != "v1" {
		t.Fatalf("legacy id = %q, want v1", kr.legacyID)
	}
}

func TestParseKeyring_Errors(t *testing.T) {
	cases := []struct {
		name, keys, primary, legacy string
	}{
		{"bad base64", "v1:!!!notb64", "v1", ""},
		{"short key", "v1:" + base64.StdEncoding.EncodeToString(make([]byte, 16)), "v1", ""},
		{"dup id", "v1:" + b64key(1) + ",v1:" + b64key(9), "v1", ""},
		{"no colon", "v1" + b64key(1), "v1", ""},
		{"primary missing", "v1:" + b64key(1), "", ""},
		{"primary not in ring", "v1:" + b64key(1), "v9", ""},
		{"legacy not in ring", "v1:" + b64key(1), "v1", b64key(7)},
	}
	for _, c := range cases {
		if _, err := ParseKeyring(c.keys, c.primary, c.legacy); err == nil {
			t.Errorf("%s: expected error, got nil", c.name)
		}
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/identity/ -run TestParseKeyring`
Expected: FAIL — `undefined: ParseKeyring`.

- [ ] **Step 3: Implement `ParseKeyring`**

Append to `internal/identity/keyring.go` (and add `"bytes"`, `"encoding/base64"`, `"strings"` to its import block):

```go
// ParseKeyring builds a Keyring from operator env values:
//
//   keysEnv      RUNTIME_SECRETS_KEYS    "id:base64key,id:base64key" (each key 32 bytes)
//   primaryEnv   RUNTIME_SECRETS_PRIMARY id of the key new writes seal under
//   legacyKeyEnv RUNTIME_SECRETS_KEY     base64 of the legacy single key
//
// All empty ⇒ (nil, nil): the feature is disabled. keysEnv empty but legacyKeyEnv
// set ⇒ the M2 back-compat ring {v1: key}, primary and legacy v1. keysEnv set ⇒ a
// multi-key ring; primaryEnv must name a ring entry, and a non-empty legacyKeyEnv
// must match exactly one entry's bytes (that id becomes the version-less decrypt
// key). Any malformed input is an error (runtimed turns it into a fatal startup).
func ParseKeyring(keysEnv, primaryEnv, legacyKeyEnv string) (*Keyring, error) {
	if keysEnv == "" && legacyKeyEnv == "" {
		return nil, nil
	}
	if keysEnv == "" {
		key, err := decodeKey(legacyKeyEnv)
		if err != nil {
			return nil, fmt.Errorf("identity: RUNTIME_SECRETS_KEY: %w", err)
		}
		c, err := NewCipher(key)
		if err != nil {
			return nil, err
		}
		return NewKeyring(map[string]*Cipher{"v1": c}, "v1", "v1")
	}

	ciphers := map[string]*Cipher{}
	rawKeys := map[string][]byte{}
	for _, pair := range strings.Split(keysEnv, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		id, b64, ok := strings.Cut(pair, ":")
		if !ok || id == "" {
			return nil, fmt.Errorf("identity: RUNTIME_SECRETS_KEYS entry %q must be id:base64", pair)
		}
		if _, dup := ciphers[id]; dup {
			return nil, fmt.Errorf("identity: duplicate key id %q in RUNTIME_SECRETS_KEYS", id)
		}
		key, err := decodeKey(b64)
		if err != nil {
			return nil, fmt.Errorf("identity: key %q: %w", id, err)
		}
		c, err := NewCipher(key)
		if err != nil {
			return nil, fmt.Errorf("identity: key %q: %w", id, err)
		}
		ciphers[id] = c
		rawKeys[id] = key
	}
	if primaryEnv == "" {
		return nil, errors.New("identity: RUNTIME_SECRETS_PRIMARY is required when RUNTIME_SECRETS_KEYS is set")
	}

	legacyID := ""
	if legacyKeyEnv != "" {
		lk, err := decodeKey(legacyKeyEnv)
		if err != nil {
			return nil, fmt.Errorf("identity: RUNTIME_SECRETS_KEY: %w", err)
		}
		for id, k := range rawKeys {
			if bytes.Equal(k, lk) {
				legacyID = id
				break
			}
		}
		if legacyID == "" {
			return nil, errors.New("identity: RUNTIME_SECRETS_KEY does not match any key in RUNTIME_SECRETS_KEYS")
		}
	}
	return NewKeyring(ciphers, primaryEnv, legacyID)
}

// decodeKey base64-decodes a 32-byte AES key (length validated by NewCipher; this
// surfaces a clear decode error first).
func decodeKey(b64 string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("not valid base64: %w", err)
	}
	return key, nil
}
```

- [ ] **Step 4: Run the parse tests**

Run: `go test ./internal/identity/ -run TestParseKeyring`
Expected: PASS.

- [ ] **Step 5: Build**

Run: `go build ./...`
Expected: clean.

- [ ] **Step 6: Commit**

```bash
git add internal/identity/keyring.go internal/identity/keyring_test.go
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "feat(identity): ParseKeyring builds a keyring from RUNTIME_SECRETS_* env

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 4: Broker switches to Keyring + threads (tenant, name)

This is the atomic signature change. `Broker` holds a `*Keyring`; `NewBroker(store, *Keyring)`; `SecretsFor`/`SetSecret` pass `(tenant, name)`. The three call sites — `broker_test.go`, `cmd/runtimed/main.go`, `test/secrets_e2e_test.go` — are updated in the same commit so the tree stays green. `Rotate` lands in Task 5.

**Files:**
- Modify: `internal/identity/broker.go`
- Modify: `internal/identity/broker_test.go`
- Modify: `cmd/runtimed/main.go` (`buildSecretBroker`)
- Modify: `test/secrets_e2e_test.go` (construction only)

- [ ] **Step 1: Update the broker unit tests to a keyring-backed broker**

Replace `internal/identity/broker_test.go` with:

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
	kr, err := NewKeyring(map[string]*Cipher{"v1": c}, "v1", "v1")
	if err != nil {
		t.Fatal(err)
	}
	return NewBroker(fs, kr)
}

func TestBroker_SetThenSecretsFor(t *testing.T) {
	fs := &fakeSecretStore{}
	b := newTestBroker(t, fs)
	ctx := context.Background()

	if err := b.SetSecret(ctx, "alpha", "OPENAI_API_KEY", "sk-xyz"); err != nil {
		t.Fatal(err)
	}
	if string(fs.put["OPENAI_API_KEY"]) == "sk-xyz" {
		t.Fatal("SetSecret stored plaintext, expected ciphertext")
	}
	if fs.put["OPENAI_API_KEY"][0] != 0x01 {
		t.Fatal("SetSecret did not store a new-format (0x01) blob")
	}

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
	fs := &fakeSecretStore{}
	b := newTestBroker(t, fs)
	ctx := context.Background()

	// One validly-sealed secret (correct tenant+name AAD)...
	if err := b.SetSecret(ctx, "alpha", "GOOD", "sk-good"); err != nil {
		t.Fatal(err)
	}
	// ...alongside a corrupt row. The whole resolution must fail closed:
	// the good secret must NOT survive in a partial map.
	fs.loaded = []EncryptedSecret{
		{Name: "GOOD", ValueEnc: fs.put["GOOD"]},
		{Name: "BAD", ValueEnc: []byte("not-valid-ciphertext")},
	}
	got, err := b.SecretsFor(ctx, "alpha")
	if err == nil {
		t.Fatal("SecretsFor must error (fail closed) on undecryptable row")
	}
	if got != nil {
		t.Fatalf("fail-closed must return a nil map, got partial: %v", got)
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

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/identity/ -run TestBroker`
Expected: FAIL — `NewBroker`/field type mismatch (`*Keyring` not yet accepted).

- [ ] **Step 3: Switch the Broker to a Keyring**

In `internal/identity/broker.go`, replace the `Broker` struct, `NewBroker`, `SecretsFor`, and `SetSecret` (lines 17-59) with:

```go
// Broker is the single place where the Keyring meets storage. It seals on write
// and opens on read; the control plane sees it only through the SecretBroker
// (read) and SecretAdmin (write) interfaces it satisfies.
type Broker struct {
	store   secretStore
	keyring *Keyring
}

// NewBroker pairs a store with a keyring.
func NewBroker(store secretStore, keyring *Keyring) *Broker {
	return &Broker{store: store, keyring: keyring}
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
		pt, err := b.keyring.Open(tenant, e.Name, e.ValueEnc)
		if err != nil {
			return nil, fmt.Errorf("identity: decrypt secret %q for tenant %q: %w", e.Name, tenant, err)
		}
		out[e.Name] = string(pt)
	}
	return out, nil
}

// SetSecret seals the plaintext under the primary key (binding tenant+name) and
// persists it (UPSERT).
func (b *Broker) SetSecret(ctx context.Context, tenant, name, plaintext string) error {
	enc, err := b.keyring.Seal(tenant, name, []byte(plaintext))
	if err != nil {
		return fmt.Errorf("identity: seal secret %q for tenant %q: %w", name, tenant, err)
	}
	if err := b.store.PutSecret(ctx, tenant, name, enc); err != nil {
		return fmt.Errorf("identity: store secret %q for tenant %q: %w", name, tenant, err)
	}
	return nil
}
```

(`ListSecretNames` and `DeleteSecret` pass-throughs at the bottom of the file are unchanged.)

- [ ] **Step 4: Update `buildSecretBroker` in runtimed to build a keyring**

In `cmd/runtimed/main.go`, replace the entire `buildSecretBroker` function (lines 274-292) with:

```go
// buildSecretBroker constructs a secret broker from the keyring env vars over the
// identity store. Returns nil when no key is configured (feature disabled,
// backward-compatible). Any malformed config is an operator error and is fatal.
//
//	RUNTIME_SECRETS_KEYS    "id:base64key,id:base64key" (each key 32 bytes)
//	RUNTIME_SECRETS_PRIMARY id new writes seal under (required when KEYS is set)
//	RUNTIME_SECRETS_KEY     legacy single key; also names the version-less decrypt key
func buildSecretBroker(idStore *identity.Store) *identity.Broker {
	kr, err := identity.ParseKeyring(
		os.Getenv("RUNTIME_SECRETS_KEYS"),
		os.Getenv("RUNTIME_SECRETS_PRIMARY"),
		os.Getenv("RUNTIME_SECRETS_KEY"),
	)
	if err != nil {
		slog.Error("secrets keyring invalid", "err", err)
		os.Exit(1)
	}
	if kr == nil {
		slog.Info("secrets brokering disabled: no secrets key configured")
		return nil
	}
	slog.Info("secrets brokering enabled", "keys", kr.NumKeys(), "primary", kr.PrimaryID())
	return identity.NewBroker(idStore, kr)
}
```

Remove the now-unused `"encoding/base64"` import from `cmd/runtimed/main.go` (the `go build` in Step 6 will flag it if missed).

- [ ] **Step 5: Update the E2E test construction to build a keyring**

In `test/secrets_e2e_test.go`, replace the cipher/broker construction (lines 83-91) with:

```go
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	cipher, err := identity.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	kr, err := identity.NewKeyring(map[string]*identity.Cipher{"v1": cipher}, "v1", "v1")
	if err != nil {
		t.Fatal(err)
	}
	broker := identity.NewBroker(st, kr)
```

- [ ] **Step 6: Build and run hermetic tests**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS (broker tests green; runtimed + E2E compile).

- [ ] **Step 7: Run the E2E integration test (construction still works end-to-end)**

Run: `go test -tags integration ./test/ -run TestSecretsE2E_PerTenantInjection`
Expected: PASS (or `SKIP` if Postgres is unreachable — if skipped, note it and continue).

- [ ] **Step 8: Commit**

```bash
git add internal/identity/broker.go internal/identity/broker_test.go cmd/runtimed/main.go test/secrets_e2e_test.go
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "feat(identity): Broker uses a Keyring; thread (tenant,name) into seal/open

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 5: Broker.Rotate + RotateStats

**Files:**
- Modify: `internal/identity/broker.go` (add `RotateStats`, `Rotate`; import `log/slog`)
- Modify: `internal/identity/broker_test.go` (add rotate tests)

`Rotate` loads every row, opens it (old key / legacy / current primary), re-seals under the current primary with AAD, and writes it back. One bad row is counted and skipped — the pass continues (asymmetry vs. fail-closed spawn).

- [ ] **Step 1: Write the failing rotate tests**

Append to `internal/identity/broker_test.go`:

```go
// rotateFakeStore records writes back so Rotate's output can be inspected.
type rotateFakeStore struct {
	rows map[string][]byte
}

func (f *rotateFakeStore) PutSecret(_ context.Context, _, name string, enc []byte) error {
	f.rows[name] = enc
	return nil
}
func (f *rotateFakeStore) ListSecretNames(_ context.Context, _ string) ([]SecretMeta, error) {
	return nil, nil
}
func (f *rotateFakeStore) DeleteSecret(_ context.Context, _, name string) error {
	delete(f.rows, name)
	return nil
}
func (f *rotateFakeStore) LoadSecrets(_ context.Context, _ string) ([]EncryptedSecret, error) {
	out := make([]EncryptedSecret, 0, len(f.rows))
	for n, v := range f.rows {
		out = append(out, EncryptedSecret{Name: n, ValueEnc: v})
	}
	return out, nil
}

func twoKeyBroker(t *testing.T, fs secretStore) *Broker {
	t.Helper()
	cOld, _ := NewCipher(key32())
	nk := key32()
	nk[0] ^= 0xff
	cNew, _ := NewCipher(nk)
	kr, err := NewKeyring(map[string]*Cipher{"v1": cOld, "v2": cNew}, "v2", "v1")
	if err != nil {
		t.Fatal(err)
	}
	return NewBroker(fs, kr)
}

func TestBroker_RotateMovesRowsToPrimary(t *testing.T) {
	ctx := context.Background()
	fs := &rotateFakeStore{rows: map[string][]byte{}}

	// Seed a v1 row by sealing with a v1-primary broker over the SAME store.
	cOld, _ := NewCipher(key32())
	krOld, _ := NewKeyring(map[string]*Cipher{"v1": cOld}, "v1", "v1")
	seed := NewBroker(fs, krOld)
	if err := seed.SetSecret(ctx, "alpha", "K1", "val1"); err != nil {
		t.Fatal(err)
	}
	if fs.rows["K1"][2] != 'v' || fs.rows["K1"][3] != '1' {
		t.Fatalf("seed row not v1: id=%q", fs.rows["K1"][2:4])
	}

	b := twoKeyBroker(t, fs)
	st, err := b.Rotate(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if st.Total != 1 || st.Rotated != 1 || st.Failed != 0 {
		t.Fatalf("stats = %+v, want total=1 rotated=1 failed=0", st)
	}
	if string(fs.rows["K1"][2:4]) != "v2" {
		t.Fatalf("row not migrated to primary v2: id=%q", fs.rows["K1"][2:4])
	}
	got, err := b.SecretsFor(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if got["K1"] != "val1" {
		t.Fatalf("post-rotate decrypt mismatch: %q", got["K1"])
	}
}

func TestBroker_RotateIsolatesBadRow(t *testing.T) {
	ctx := context.Background()
	fs := &rotateFakeStore{rows: map[string][]byte{}}
	b := twoKeyBroker(t, fs)
	if err := b.SetSecret(ctx, "alpha", "GOOD1", "a"); err != nil {
		t.Fatal(err)
	}
	if err := b.SetSecret(ctx, "alpha", "GOOD2", "b"); err != nil {
		t.Fatal(err)
	}
	fs.rows["BAD"] = []byte{0x01, 0x02, 'x', 'y'} // unparseable / unknown id

	st, err := b.Rotate(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if st.Total != 3 || st.Rotated != 2 || st.Failed != 1 {
		t.Fatalf("stats = %+v, want total=3 rotated=2 failed=1", st)
	}
}

func TestBroker_RotateIdempotent(t *testing.T) {
	ctx := context.Background()
	fs := &rotateFakeStore{rows: map[string][]byte{}}
	b := twoKeyBroker(t, fs)
	if err := b.SetSecret(ctx, "alpha", "K", "v"); err != nil {
		t.Fatal(err)
	}
	first, _ := b.Rotate(ctx, "alpha")
	second, err := b.Rotate(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if second.Failed != 0 || second.Rotated != first.Rotated {
		t.Fatalf("second rotate not clean: %+v", second)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/identity/ -run TestBroker_Rotate`
Expected: FAIL — `b.Rotate undefined`.

- [ ] **Step 3: Implement `Rotate`**

In `internal/identity/broker.go`, add `"log/slog"` to the import block, and append:

```go
// RotateStats reports the outcome of a re-encrypt pass. It carries no secret
// values — only counts and the tenant.
type RotateStats struct {
	Tenant  string `json:"tenant"`
	Total   int    `json:"total"`
	Rotated int    `json:"rotated"`
	Failed  int    `json:"failed"`
}

// Rotate re-encrypts every secret of a tenant under the current primary key,
// binding (tenant, name) as AAD. It is idempotent and re-runnable. Unlike the
// fail-closed spawn path, one undecryptable row is counted as failed and skipped
// so a single corrupt row cannot block migrating the rest; the row name (never
// the value) is logged.
func (b *Broker) Rotate(ctx context.Context, tenant string) (RotateStats, error) {
	enc, err := b.store.LoadSecrets(ctx, tenant)
	if err != nil {
		return RotateStats{Tenant: tenant}, err
	}
	st := RotateStats{Tenant: tenant, Total: len(enc)}
	for _, e := range enc {
		pt, err := b.keyring.Open(tenant, e.Name, e.ValueEnc)
		if err != nil {
			st.Failed++
			slog.Error("rotate: open failed", "tenant", tenant, "name", e.Name, "err", err)
			continue
		}
		nb, err := b.keyring.Seal(tenant, e.Name, pt)
		if err != nil {
			st.Failed++
			slog.Error("rotate: seal failed", "tenant", tenant, "name", e.Name, "err", err)
			continue
		}
		if err := b.store.PutSecret(ctx, tenant, e.Name, nb); err != nil {
			st.Failed++
			slog.Error("rotate: store failed", "tenant", tenant, "name", e.Name, "err", err)
			continue
		}
		st.Rotated++
	}
	return st, nil
}
```

- [ ] **Step 4: Run the rotate tests**

Run: `go test ./internal/identity/`
Expected: PASS (all identity unit tests).

- [ ] **Step 5: Build + vet**

Run: `go build ./... && go vet ./...`
Expected: clean.

- [ ] **Step 6: Commit**

```bash
git add internal/identity/broker.go internal/identity/broker_test.go
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "feat(identity): Broker.Rotate re-encrypts a tenant under the primary key

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 6: `/admin/secrets/rotate` endpoint

**Files:**
- Modify: `controlplane/admin.go` (extend `SecretAdmin`, `AdminStore`; add route)
- Modify: `controlplane/admin_test.go`

The handler: admin-only; 503 if no broker; superuser with no `tenant` body fans out over `ListTenants`; otherwise `effectiveTenant`. Always returns a JSON array of `RotateStats`.

- [ ] **Step 1: Inspect the existing admin test setup**

Run: `sed -n '1,60p' controlplane/admin_test.go`
Expected: shows the package's fake store + principal-context helpers. Reuse the existing fake-`AdminStore` and request helpers; if the fake does not yet implement `ListTenants`/a `SecretAdmin`, add them in Step 3's test.

- [ ] **Step 2: Write the failing handler tests**

Append to `controlplane/admin_test.go` (adapt the helper names — `newAdminRequest`, the fake store type, principal injection — to those already present in the file; the assertions below are what matter):

```go
// fakeRotator records which tenants were rotated.
type fakeRotator struct {
	rotated []string
}

func (f *fakeRotator) SetSecret(context.Context, string, string, string) error { return nil }
func (f *fakeRotator) ListSecretNames(context.Context, string) ([]identity.SecretMeta, error) {
	return nil, nil
}
func (f *fakeRotator) DeleteSecret(context.Context, string, string) error { return nil }
func (f *fakeRotator) RotateSecrets(_ context.Context, tenant string) (identity.RotateStats, error) {
	f.rotated = append(f.rotated, tenant)
	return identity.RotateStats{Tenant: tenant, Total: 1, Rotated: 1}, nil
}

func TestRotate_RequiresAdmin(t *testing.T) {
	mux := http.NewServeMux()
	RegisterSecretAdmin(mux, newFakeAdminStore(), &fakeRotator{})
	// viewer principal → 403
	rr := doAdmin(t, mux, "POST", "/admin/secrets/rotate", "", principalViewer())
	if rr.Code != http.StatusForbidden {
		t.Fatalf("viewer rotate = %d, want 403", rr.Code)
	}
}

func TestRotate_NoBroker503(t *testing.T) {
	mux := http.NewServeMux()
	RegisterSecretAdmin(mux, newFakeAdminStore(), nil)
	rr := doAdmin(t, mux, "POST", "/admin/secrets/rotate", "", principalAdmin("alpha"))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil-broker rotate = %d, want 503", rr.Code)
	}
}

func TestRotate_NonSuperuserScopedToOwnTenant(t *testing.T) {
	mux := http.NewServeMux()
	fr := &fakeRotator{}
	RegisterSecretAdmin(mux, newFakeAdminStore(), fr)
	rr := doAdmin(t, mux, "POST", "/admin/secrets/rotate", `{"tenant":"beta"}`, principalAdmin("alpha"))
	if rr.Code != http.StatusOK {
		t.Fatalf("admin rotate = %d, want 200", rr.Code)
	}
	if len(fr.rotated) != 1 || fr.rotated[0] != "alpha" {
		t.Fatalf("non-superuser rotated %v, want [alpha] (body tenant ignored)", fr.rotated)
	}
}

func TestRotate_SuperuserAllTenants(t *testing.T) {
	mux := http.NewServeMux()
	fr := &fakeRotator{}
	store := newFakeAdminStore()
	store.tenants = []identity.TenantRow{{ID: "alpha"}, {ID: "beta"}}
	RegisterSecretAdmin(mux, store, fr)
	rr := doAdmin(t, mux, "POST", "/admin/secrets/rotate", "", principalSuperuser())
	if rr.Code != http.StatusOK {
		t.Fatalf("superuser rotate = %d, want 200", rr.Code)
	}
	if len(fr.rotated) != 2 {
		t.Fatalf("superuser all-tenants rotated %v, want 2 tenants", fr.rotated)
	}
}
```

> If the existing test file lacks `newFakeAdminStore`, `doAdmin`, `principalAdmin/Viewer/Superuser` helpers, use whatever equivalents the file already defines (read it in Step 1). The fake `AdminStore` must gain a `tenants []identity.TenantRow` field and a `ListTenants` method returning it.

- [ ] **Step 3: Run to verify failure**

Run: `go test ./controlplane/ -run TestRotate`
Expected: FAIL — `RotateSecrets` not in `SecretAdmin` / `ListTenants` not in `AdminStore` / route 404.

- [ ] **Step 4: Extend the interfaces and add the route**

In `controlplane/admin.go`:

Add `ListTenants` to the `AdminStore` interface (after `ListKeys`):

```go
	ListTenants(ctx context.Context) ([]identity.TenantRow, error)
```

Add `RotateSecrets` to the `SecretAdmin` interface (after `DeleteSecret`):

```go
	RotateSecrets(ctx context.Context, tenant string) (identity.RotateStats, error)
```

Inside `RegisterSecretAdmin`, after the `DELETE /admin/secrets/{name}` handler, add:

```go
	mux.HandleFunc("POST /admin/secrets/rotate", func(w http.ResponseWriter, r *http.Request) {
		p, ok := requireAdmin(w, r)
		if !ok {
			return
		}
		if sa == nil {
			http.Error(w, "secrets not configured", http.StatusServiceUnavailable)
			return
		}
		var body struct{ Tenant string }
		if !decode(w, r, &body) {
			return
		}
		// Superuser with no explicit tenant rotates every tenant.
		if p.Superuser && body.Tenant == "" {
			trs, err := store.ListTenants(r.Context())
			if err != nil {
				serverError(w, "list tenants", err)
				return
			}
			out := make([]identity.RotateStats, 0, len(trs))
			for _, tr := range trs {
				st, err := sa.RotateSecrets(r.Context(), tr.ID)
				if err != nil {
					serverError(w, "rotate secrets", err)
					return
				}
				out = append(out, st)
			}
			writeJSON(w, http.StatusOK, out)
			return
		}
		tenant, ok := effectiveTenant(w, r, store, p, body.Tenant)
		if !ok {
			return
		}
		st, err := sa.RotateSecrets(r.Context(), tenant)
		if err != nil {
			serverError(w, "rotate secrets", err)
			return
		}
		writeJSON(w, http.StatusOK, []identity.RotateStats{st})
	})
```

- [ ] **Step 5: Run the handler tests**

Run: `go test ./controlplane/ -run TestRotate`
Expected: PASS.

- [ ] **Step 6: Full hermetic suite + build**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS (note: `*identity.Broker` already has `RotateSecrets`? No — the broker method is named `Rotate`; the interface method is `RotateSecrets`. Add the adapter in Step 7 before this passes for runtimed wiring — but hermetic `go test ./...` does not exercise runtimed's interface satisfaction unless something assigns it. If `go build ./...` fails because `*identity.Broker` does not satisfy `SecretAdmin`, proceed to Step 7 now, then re-run.)

- [ ] **Step 7: Make `*identity.Broker` satisfy `SecretAdmin.RotateSecrets`**

The interface method is `RotateSecrets`; the broker method is `Rotate`. Add a thin alias to `internal/identity/broker.go` so the broker satisfies the control-plane interface without renaming the natural method:

```go
// RotateSecrets satisfies controlplane.SecretAdmin; it is an alias for Rotate.
func (b *Broker) RotateSecrets(ctx context.Context, tenant string) (RotateStats, error) {
	return b.Rotate(ctx, tenant)
}
```

Re-run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add controlplane/admin.go controlplane/admin_test.go internal/identity/broker.go
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "feat(controlplane): POST /admin/secrets/rotate (superuser all-tenants fan-out)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 7: `runtimectl admin secret rotate`

**Files:**
- Modify: `cmd/runtimectl/main.go`

- [ ] **Step 1: Add the `secret rotate` case**

In `cmd/runtimectl/main.go`, in `runAdmin`'s switch, after the `"secret rm"` case add:

```go
	case "secret rotate":
		// admin secret rotate [--tenant t]
		tenant := flagValue(args[2:], "--tenant", "")
		out := mustAdminPost(base, "/admin/secrets/rotate", map[string]string{"tenant": tenant})
		var stats []struct {
			Tenant  string `json:"tenant"`
			Total   int    `json:"total"`
			Rotated int    `json:"rotated"`
			Failed  int    `json:"failed"`
		}
		if err := json.Unmarshal(out, &stats); err != nil {
			fmt.Fprintf(os.Stderr, "rotate: bad response: %s\n", out)
			os.Exit(1)
		}
		failed := 0
		for _, s := range stats {
			fmt.Printf("tenant %s: total=%d rotated=%d failed=%d\n", s.Tenant, s.Total, s.Rotated, s.Failed)
			failed += s.Failed
		}
		if failed > 0 {
			os.Exit(1)
		}
```

Update `adminUsage` to include `secret rotate [--tenant t]`:

```go
func adminUsage() {
	fmt.Fprintln(os.Stderr, "usage: runtimectl admin <tenant create <id> [--name n]|user add <subject> --role r [--tenant t]|user ls|key create --role r [--label l] [--tenant t]|key ls|key revoke <id>|secret set <name> <value> [--tenant t]|secret ls|secret rm <name>|secret rotate [--tenant t]>")
	os.Exit(2)
}
```

- [ ] **Step 2: Build + vet**

Run: `go build ./... && go vet ./...`
Expected: clean.

- [ ] **Step 3: Manual smoke (compile-level) of the CLI dispatch**

Run: `go run ./cmd/runtimectl admin 2>&1 | head -1`
Expected: the usage line, now including `secret rotate [--tenant t]`.

- [ ] **Step 4: Commit**

```bash
git add cmd/runtimectl/main.go
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "feat(runtimectl): admin secret rotate

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 8: Integration — Rotate over real Postgres

**Files:**
- Modify: `internal/identity/secrets_store_test.go`

- [ ] **Step 1: Add the rotate integration test**

Append to `internal/identity/secrets_store_test.go` (it already has `//go:build integration` and `freshStore`):

```go
func TestBroker_RotateOverPostgres(t *testing.T) {
	ctx := context.Background()
	s, db := freshStore(t)
	defer db.Close()
	if err := s.CreateTenant(ctx, "alpha", "A"); err != nil {
		t.Fatal(err)
	}

	cOld, _ := NewCipher(key32())
	nk := key32()
	nk[0] ^= 0xff
	cNew, _ := NewCipher(nk)

	// Seed a hand-written LEGACY row (version-less: nonce||ct, nil AAD) under the
	// old key, plus a v1 new-format row.
	legacyBlob, err := cOld.Seal([]byte("sk-legacy"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutSecret(ctx, "alpha", "LEG", legacyBlob); err != nil {
		t.Fatal(err)
	}
	krOld, _ := NewKeyring(map[string]*Cipher{"v1": cOld}, "v1", "v1")
	seed := NewBroker(s, krOld)
	if err := seed.SetSecret(ctx, "alpha", "NEWK", "sk-new"); err != nil {
		t.Fatal(err)
	}

	// Rotate under a ring whose primary is v2 (legacy v1 still present).
	krBoth, _ := NewKeyring(map[string]*Cipher{"v1": cOld, "v2": cNew}, "v2", "v1")
	b := NewBroker(s, krBoth)
	st, err := b.Rotate(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if st.Total != 2 || st.Rotated != 2 || st.Failed != 0 {
		t.Fatalf("rotate stats = %+v, want total=2 rotated=2 failed=0", st)
	}

	// Every stored row must now be v2 new-format and decrypt to the originals
	// under a ring with ONLY the new key (proves the old key is retireable).
	krNew, _ := NewKeyring(map[string]*Cipher{"v2": cNew}, "v2", "")
	retired := NewBroker(s, krNew)
	got, err := retired.SecretsFor(ctx, "alpha")
	if err != nil {
		t.Fatalf("post-rotate read under new-only ring failed: %v", err)
	}
	if got["LEG"] != "sk-legacy" || got["NEWK"] != "sk-new" {
		t.Fatalf("post-rotate values wrong: %+v", got)
	}
	enc, _ := s.LoadSecrets(ctx, "alpha")
	for _, e := range enc {
		if len(e.ValueEnc) < 4 || e.ValueEnc[0] != 0x01 || string(e.ValueEnc[2:2+int(e.ValueEnc[1])]) != "v2" {
			t.Fatalf("row %q not migrated to v2 new-format", e.Name)
		}
	}
}
```

- [ ] **Step 2: Run the identity integration suite**

Run: `go test -tags integration ./internal/identity/`
Expected: PASS (existing CRUD/cascade tests + the new rotate test). If Postgres is unreachable, the suite skips/fails on connect — start Postgres.app and retry.

- [ ] **Step 3: Commit**

```bash
git add internal/identity/secrets_store_test.go
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "test(identity): Rotate over Postgres incl. legacy row, retire old key

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 9: E2E — rotation across the spawn path

**Files:**
- Modify: `test/secrets_e2e_test.go`

Prove rotation end-to-end: seal under an old primary, spawn (env shows value), add a new primary + rotate, spawn (still shows value), retire the old key, spawn (still works).

- [ ] **Step 1: Add the rotation E2E test**

Append to `test/secrets_e2e_test.go` (reuses `spawnAndWaitEnv`, `dsn`):

```go
func TestSecretsE2E_KeyRotation(t *testing.T) {
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
	if err := st.CreateTenant(ctx, "alpha", "A"); err != nil {
		t.Fatal(err)
	}

	mkKey := func(seed byte) []byte {
		k := make([]byte, 32)
		for i := range k {
			k[i] = seed + byte(i)
		}
		return k
	}
	cOld, _ := identity.NewCipher(mkKey(1))
	cNew, _ := identity.NewCipher(mkKey(100))

	// Phase 1: seal under an old-only ring.
	krOld, _ := identity.NewKeyring(map[string]*identity.Cipher{"v1": cOld}, "v1", "v1")
	if err := identity.NewBroker(st, krOld).SetSecret(ctx, "alpha", "OPENAI_API_KEY", "sk-rot"); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	mkAgent := func(out string) config.AgentConfig {
		return config.AgentConfig{
			ID:         "agent-alpha",
			ListenAddr: "127.0.0.1:0",
			Tenant:     "alpha",
			Command:    []string{"sh", "-c", "env > " + out},
		}
	}
	spawnWith := func(broker controlplane.SecretBroker, out string) string {
		cfg := &config.Config{Agents: []config.AgentConfig{mkAgent(out)}}
		reg := controlplane.NewRegistry(cfg, "./agentd", dsn)
		reg.SetBroker(broker)
		ap, ok := reg.Get("agent-alpha")
		if !ok {
			t.Fatal("agent-alpha not found")
		}
		return spawnAndWaitEnv(t, ap, out)
	}

	env1 := spawnWith(identity.NewBroker(st, krOld), filepath.Join(dir, "p1.env"))
	if !strings.Contains(env1, "OPENAI_API_KEY=sk-rot") {
		t.Fatalf("phase1 missing secret:\n%s", env1)
	}

	// Phase 2: add a new primary (v2), keep v1 as legacy, rotate.
	krBoth, _ := identity.NewKeyring(map[string]*identity.Cipher{"v1": cOld, "v2": cNew}, "v2", "v1")
	rstat, err := identity.NewBroker(st, krBoth).Rotate(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if rstat.Rotated != 1 || rstat.Failed != 0 {
		t.Fatalf("rotate stats = %+v", rstat)
	}
	env2 := spawnWith(identity.NewBroker(st, krBoth), filepath.Join(dir, "p2.env"))
	if !strings.Contains(env2, "OPENAI_API_KEY=sk-rot") {
		t.Fatalf("phase2 (post-rotate) missing secret:\n%s", env2)
	}

	// Phase 3: retire the old key entirely (v2-only ring) and prove spawn works.
	krNew, _ := identity.NewKeyring(map[string]*identity.Cipher{"v2": cNew}, "v2", "")
	env3 := spawnWith(identity.NewBroker(st, krNew), filepath.Join(dir, "p3.env"))
	if !strings.Contains(env3, "OPENAI_API_KEY=sk-rot") {
		t.Fatalf("phase3 (old key retired) missing secret:\n%s", env3)
	}
}
```

- [ ] **Step 2: Run both E2E tests**

Run: `go test -tags integration ./test/`
Expected: PASS (`TestSecretsE2E_PerTenantInjection` + `TestSecretsE2E_KeyRotation`).

- [ ] **Step 3: Commit**

```bash
git add test/secrets_e2e_test.go
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "test(e2e): key rotation across the spawn path (seed, rotate, retire)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 10: Documentation

**Files:**
- Modify: `README.md`
- Modify: `ROADMAP.md`
- Modify: `docs/images/project-layout.mmd` (+ regenerate `.png` if `mmdc` is available)
- Modify: `~/.claude/projects/-Users-sausheong-projects-runtime/memory/runtime-platform-project.md`

- [ ] **Step 1: README — secrets sub-section, env table, CLI table**

In `README.md`, find the existing "Per-tenant secrets" sub-section (added in M2). Extend it with the keyring + rotation workflow:

```markdown
**Key rotation.** The secrets master key is a *keyring* of one or more 32-byte
keys, one designated primary. Configure it with:

- `RUNTIME_SECRETS_KEYS` — `id:base64key,id:base64key` (each key is base64 of 32
  bytes; ids are operator-chosen, e.g. `v1`, `2026q2`).
- `RUNTIME_SECRETS_PRIMARY` — the id new secrets are sealed under.
- `RUNTIME_SECRETS_KEY` — the legacy single key (still honored). On its own it is
  the keyring `{v1: key}`. Alongside `RUNTIME_SECRETS_KEYS` it names the key that
  decrypts pre-rotation (version-less) rows; its bytes must match a keyring entry.

Each secret blob is self-describing (it carries the id of the key that sealed it)
and is AAD-bound to its `(tenant, name)`, so a ciphertext cannot be swapped to
another row. To rotate:

1. Add a new key to `RUNTIME_SECRETS_KEYS`, set `RUNTIME_SECRETS_PRIMARY` to it,
   keep the old key in the ring, and restart runtimed. New writes use the new key
   immediately; existing rows still decrypt under the old key.
2. Run `runtimectl admin secret rotate` (superuser: all tenants; or
   `--tenant <t>`). This re-encrypts every secret under the new primary. It is
   idempotent and re-runnable; it exits non-zero if any row failed.
3. Once rotation reports 0 failures, remove the old key from
   `RUNTIME_SECRETS_KEYS` and restart. The old key is now retired.
```

In the README env-var table, add rows (keep the existing `RUNTIME_SECRETS_KEY` row, noting it is the legacy/back-compat key):

```markdown
| `RUNTIME_SECRETS_KEYS` | Keyring: `id:base64key,id:base64key` (each 32 bytes). Enables multi-key rotation. | _unset_ |
| `RUNTIME_SECRETS_PRIMARY` | Keyring id new secrets are sealed under. Required when `RUNTIME_SECRETS_KEYS` is set. | _unset_ |
```

In the README CLI table, add:

```markdown
| `runtimectl admin secret rotate [--tenant t]` | Re-encrypt secrets under the current primary key (superuser: all tenants). |
```

- [ ] **Step 2: ROADMAP — mark M3 done, move remaining items**

In `ROADMAP.md`, update the `**Current state:**` header to mention M3, and in §B3 add a "Third milestone DONE" paragraph mirroring the M2 one:

```markdown
   **Third milestone DONE (merged to `master`, 2026-06-09):** secrets key
   rotation. The master key is now a keyring (`RUNTIME_SECRETS_KEYS` +
   `RUNTIME_SECRETS_PRIMARY`; the legacy `RUNTIME_SECRETS_KEY` is the back-compat
   single key). Ciphertext blobs are self-describing (versioned `0x01` prefix +
   embedded key id) and AAD-bound to `(tenant, name)` to defeat DB row swaps. An
   explicit, idempotent `runtimectl admin secret rotate` re-encrypts a tenant
   (superuser: all tenants) under the primary so retired keys can be dropped.
   Legacy M2 rows decrypt transparently until rotated. Spec/plan:
   `docs/superpowers/{specs,plans}/2026-06-09-identity-m3-secrets-key-rotation*`.
```

Update the "Remaining Identity work" list to drop key rotation + AAD binding (now done), leaving: per-tenant keys, fine-grained/custom RBAC, cross-tenant users + self-service, admin console UI, local password accounts, console CSRF.

- [ ] **Step 3: Layout diagram**

In `docs/images/project-layout.mmd`, update the `ident` node description to mention the keyring, e.g. change `Cipher, Broker` to `Cipher, Keyring, Broker`.

If `mmdc` is on PATH, regenerate the PNG:

Run: `command -v mmdc && mmdc -i docs/images/project-layout.mmd -o docs/images/project-layout.png -t neutral -b white -s 3 || echo "mmdc not available; skipping PNG regen"`
Expected: PNG regenerated, or a clear skip message (don't fail the task if `mmdc` is absent).

- [ ] **Step 4: Project memory**

In `~/.claude/projects/-Users-sausheong-projects-runtime/memory/runtime-platform-project.md`, add a short Identity M3 paragraph after the M2 one (keyring, self-describing AAD-bound blobs, `rotate` CLI, legacy back-compat, spec/plan paths). Keep it factual and concise.

- [ ] **Step 5: Final full verification**

Run:
```bash
go build ./... && go vet ./... && go test ./...
go test -tags integration ./internal/identity/
go test -tags integration ./test/
```
Expected: all PASS.

- [ ] **Step 6: Commit**

```bash
git add README.md ROADMAP.md docs/images/project-layout.mmd docs/images/project-layout.png
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "docs(identity): document secrets key rotation (README, ROADMAP, layout)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

(The memory file lives outside the repo; it is not part of this commit.)

---

## Final review (after all tasks)

Dispatch a holistic code reviewer over the whole branch diff (`git diff master...feat/identity-m3-key-rotation`). Focus areas, given M1/M2 lessons:

- **Legacy detection robustness** — confirm the try-v1-then-legacy fallback in `Keyring.Open` cannot misclassify, and that a corrupt new-format row still fails closed at spawn.
- **Fail-closed vs. fail-open asymmetry** — `SecretsFor` aborts the whole tenant on one bad row; `Rotate` isolates per-row. Confirm both are intentional and correct.
- **Typed-nil interface trap** — the runtimed wiring (`secretAdmin`/`reg.SetBroker`) already guards against a nil `*identity.Broker` becoming a non-nil interface; confirm the keyring change didn't reintroduce it.
- **No secret leakage** — grep the diff for any path that could log/return a key or value; rotate errors must carry name+tenant only.
- **Cross-milestone interaction** — does the per-spawn decrypt cost change (it shouldn't; same one decrypt per row)? Does anything new run in the supervisor hot path?

Then proceed to `superpowers:finishing-a-development-branch`.

---

## Self-Review (plan vs. spec)

- **Spec coverage:** keyring + self-describing blob (T2), key-ID prefix (T2), AAD binding (T1+T2), single structured env var + legacy shim (T3), `buildSecretBroker` keyring (T4), `Rotate` + per-row isolation (T5), `/admin/secrets/rotate` + superuser all-tenants (T6), CLI `secret rotate` (T7), integration rotate + retire (T8), E2E rotation across spawn (T9), docs incl. limitations/retire workflow (T10). All spec sections map to a task.
- **Deviation from spec (deliberate, documented here):** (1) `Keyring.Open` uses **try-v1-then-fall-back-to-legacy** rather than a pure leading-byte switch — closes a ~1/256 legacy-row data-loss hazard the spec's simpler phrasing missed; GCM auth makes the fallback safe. (2) No `Broker.RotateAll`; the admin handler enumerates tenants via `AdminStore.ListTenants` and loops `RotateSecrets` (simpler, the store already owns the tenant list). (3) `*identity.Broker` exposes both `Rotate` (natural name) and a thin `RotateSecrets` alias (satisfies the control-plane interface).
- **Type consistency:** `NewKeyring(map[string]*Cipher, primaryID, legacyID)`, `Keyring.Seal/Open(tenant, name, …)`, `Keyring.PrimaryID()/NumKeys()`, `ParseKeyring(keysEnv, primaryEnv, legacyKeyEnv) (*Keyring, error)`, `NewBroker(store, *Keyring)`, `Broker.Rotate(ctx, tenant) (RotateStats, error)`, `RotateStats{Tenant,Total,Rotated,Failed}`, `SecretAdmin.RotateSecrets`, `AdminStore.ListTenants` — used consistently across tasks.
- **Placeholders:** none — every code step shows complete code; test-helper adaptation in T6 is explicitly bounded to names already in the file (read in T6 Step 1).
