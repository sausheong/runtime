package identity

import (
	"context"
	"time"
)

// SecretMeta is the list read model: name + type + timestamps, plus non-secret
// oauth fields for oauth2 creds. It NEVER carries a secret value or client_secret.
type SecretMeta struct {
	Name      string      `json:"name"`
	Type      string      `json:"type"`
	OAuth2    *OAuth2Meta `json:"oauth2,omitempty"`
	CreatedAt time.Time   `json:"created_at"`
	UpdatedAt time.Time   `json:"updated_at"`
}

// EncryptedSecret is the broker-facing read model: name + ciphertext only.
type EncryptedSecret struct {
	Name     string
	ValueEnc []byte
}

// PutSecret inserts or overwrites a tenant's secret with its type. valueEnc is
// opaque ciphertext (the store never sees plaintext). UPSERT bumps updated_at.
func (s *Store) PutSecret(ctx context.Context, tenantID, name string, valueEnc []byte, credType string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO secrets (tenant_id, name, value_enc, type) VALUES ($1,$2,$3,$4)
		 ON CONFLICT (tenant_id, name)
		 DO UPDATE SET value_enc=EXCLUDED.value_enc, type=EXCLUDED.type, updated_at=now()`,
		tenantID, name, valueEnc, credType)
	return err
}

// ListSecretNames returns names + type + timestamps for a tenant. value_enc is
// never selected, so a value cannot leak through a listing. Non-secret oauth
// fields are filled by the broker (which holds the keyring), not here.
func (s *Store) ListSecretNames(ctx context.Context, tenantID string) ([]SecretMeta, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, type, created_at, updated_at FROM secrets WHERE tenant_id=$1 ORDER BY name`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SecretMeta
	for rows.Next() {
		var m SecretMeta
		if err := rows.Scan(&m.Name, &m.Type, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// LoadSecret returns one secret's ciphertext + type (for the broker to decrypt).
func (s *Store) LoadSecret(ctx context.Context, tenantID, name string) (EncryptedSecret, string, error) {
	var e EncryptedSecret
	var credType string
	err := s.db.QueryRowContext(ctx,
		`SELECT name, value_enc, type FROM secrets WHERE tenant_id=$1 AND name=$2`,
		tenantID, name).Scan(&e.Name, &e.ValueEnc, &credType)
	return e, credType, err
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
