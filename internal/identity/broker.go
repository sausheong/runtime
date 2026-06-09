package identity

import (
	"context"
	"fmt"
	"log/slog"
)

// secretStore is the slice of *Store the Broker needs. Declared as an interface
// so the broker is unit-testable without Postgres.
type secretStore interface {
	PutSecret(ctx context.Context, tenantID, name string, valueEnc []byte) error
	ListSecretNames(ctx context.Context, tenantID string) ([]SecretMeta, error)
	DeleteSecret(ctx context.Context, tenantID, name string) error
	LoadSecrets(ctx context.Context, tenantID string) ([]EncryptedSecret, error)
}

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

// ListSecretNames passes through to the store (names + metadata, no values).
func (b *Broker) ListSecretNames(ctx context.Context, tenant string) ([]SecretMeta, error) {
	return b.store.ListSecretNames(ctx, tenant)
}

// DeleteSecret passes through to the store.
func (b *Broker) DeleteSecret(ctx context.Context, tenant, name string) error {
	return b.store.DeleteSecret(ctx, tenant, name)
}

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
