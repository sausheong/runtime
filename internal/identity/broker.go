package identity

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync/atomic"
)

// secretStore is the slice of *Store the Broker needs. Declared as an interface
// so the broker is unit-testable without Postgres.
type secretStore interface {
	PutSecret(ctx context.Context, tenantID, name string, valueEnc []byte, credType string) error
	ListSecretNames(ctx context.Context, tenantID string) ([]SecretMeta, error)
	DeleteSecret(ctx context.Context, tenantID, name string) error
	LoadSecrets(ctx context.Context, tenantID string) ([]EncryptedSecret, error)
	LoadSecret(ctx context.Context, tenantID, name string) (EncryptedSecret, string, error)
}

// Broker is the single place where the Keyring meets storage. It seals on write
// and opens on read; the control plane sees it only through the SecretBroker
// (read) and SecretAdmin (write) interfaces it satisfies.
type Broker struct {
	store   secretStore
	keyring *Keyring
	gen     atomic.Uint64
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
	if err := b.store.PutSecret(ctx, tenant, name, enc, CredTypeStatic); err != nil {
		return fmt.Errorf("identity: store secret %q for tenant %q: %w", name, tenant, err)
	}
	b.gen.Add(1)
	return nil
}

// ListSecretNames passes through to the store (names + metadata, no values).
func (b *Broker) ListSecretNames(ctx context.Context, tenant string) ([]SecretMeta, error) {
	return b.store.ListSecretNames(ctx, tenant)
}

// DeleteSecret passes through to the store and bumps the generation so a cached
// TokenSource for the removed credential is discarded live.
func (b *Broker) DeleteSecret(ctx context.Context, tenant, name string) error {
	if err := b.store.DeleteSecret(ctx, tenant, name); err != nil {
		return err
	}
	b.gen.Add(1)
	return nil
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
		_, ct, terr := b.store.LoadSecret(ctx, tenant, e.Name)
		if terr != nil {
			ct = CredTypeStatic
		}
		if err := b.store.PutSecret(ctx, tenant, e.Name, nb, ct); err != nil {
			st.Failed++
			slog.Error("rotate: store failed", "tenant", tenant, "name", e.Name, "err", err)
			continue
		}
		st.Rotated++
	}
	return st, nil
}

// RotateSecrets satisfies controlplane.SecretAdmin; it is an alias for Rotate.
func (b *Broker) RotateSecrets(ctx context.Context, tenant string) (RotateStats, error) {
	return b.Rotate(ctx, tenant)
}

// SetOAuth2 seals an oauth2 client_credentials config as JSON under the keyring
// (binding tenant+name, exactly like a static value) and persists it with
// type=oauth2_client_credentials. The client_secret never leaves the broker in
// plaintext. Bumps the generation so a cached TokenSource is rebuilt live.
func (b *Broker) SetOAuth2(ctx context.Context, tenant, name string, cfg OAuth2Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("identity: marshal oauth2 config %q: %w", name, err)
	}
	enc, err := b.keyring.Seal(tenant, name, raw)
	if err != nil {
		return fmt.Errorf("identity: seal oauth2 config %q for tenant %q: %w", name, tenant, err)
	}
	if err := b.store.PutSecret(ctx, tenant, name, enc, CredTypeOAuth2); err != nil {
		return fmt.Errorf("identity: store oauth2 config %q for tenant %q: %w", name, tenant, err)
	}
	b.gen.Add(1)
	return nil
}

// CredType returns the credential type for (tenant, name): CredTypeStatic or
// CredTypeOAuth2. Absent secret ⇒ the store's error.
func (b *Broker) CredType(ctx context.Context, tenant, name string) (string, error) {
	_, ct, err := b.store.LoadSecret(ctx, tenant, name)
	if err != nil {
		return "", err
	}
	if ct == "" {
		ct = CredTypeStatic
	}
	return ct, nil
}

// OAuth2ConfigFor decrypts and returns the oauth2 config for (tenant, name).
// Used only inside the gateway process to build a TokenSource — never surfaced
// to an API. Errors if the secret is not an oauth2 credential.
func (b *Broker) OAuth2ConfigFor(ctx context.Context, tenant, name string) (OAuth2Config, error) {
	e, ct, err := b.store.LoadSecret(ctx, tenant, name)
	if err != nil {
		return OAuth2Config{}, err
	}
	if ct != CredTypeOAuth2 {
		return OAuth2Config{}, fmt.Errorf("identity: secret %q is not an oauth2 credential", name)
	}
	pt, err := b.keyring.Open(tenant, e.Name, e.ValueEnc)
	if err != nil {
		return OAuth2Config{}, fmt.Errorf("identity: decrypt oauth2 config %q for tenant %q: %w", name, tenant, err)
	}
	var cfg OAuth2Config
	if err := json.Unmarshal(pt, &cfg); err != nil {
		return OAuth2Config{}, fmt.Errorf("identity: unmarshal oauth2 config %q: %w", name, err)
	}
	return cfg, nil
}

// Generation increments on every secret write/rotate/delete. A cached oauth2
// TokenSource keys on it to rebuild after a rotation without a restart.
func (b *Broker) Generation() uint64 { return b.gen.Load() }

// ListSecrets returns the enriched read model: type for every secret and, for
// oauth2 creds, the NON-SECRET fields (token_url/client_id/scopes/audience).
// client_secret is never included. A decrypt failure on one oauth2 row omits
// its oauth fields (still lists name+type) rather than failing the whole list.
func (b *Broker) ListSecrets(ctx context.Context, tenant string) ([]SecretMeta, error) {
	metas, err := b.store.ListSecretNames(ctx, tenant)
	if err != nil {
		return nil, err
	}
	for i := range metas {
		if metas[i].Type != CredTypeOAuth2 {
			continue
		}
		cfg, cerr := b.OAuth2ConfigFor(ctx, tenant, metas[i].Name)
		if cerr != nil {
			continue // list name+type without oauth detail rather than fail
		}
		metas[i].OAuth2 = &OAuth2Meta{
			TokenURL: cfg.TokenURL, ClientID: cfg.ClientID,
			Scopes: cfg.Scopes, Audience: cfg.Audience,
		}
	}
	return metas, nil
}
