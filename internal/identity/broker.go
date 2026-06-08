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
		pt, err := b.cipher.Open(e.ValueEnc, nil)
		if err != nil {
			return nil, fmt.Errorf("identity: decrypt secret %q for tenant %q: %w", e.Name, tenant, err)
		}
		out[e.Name] = string(pt)
	}
	return out, nil
}

// SetSecret seals the plaintext and persists it (UPSERT).
func (b *Broker) SetSecret(ctx context.Context, tenant, name, plaintext string) error {
	enc, err := b.cipher.Seal([]byte(plaintext), nil)
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
