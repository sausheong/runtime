//go:build integration

package identity

import (
	"context"
	"testing"
)

func TestSecretsStore_CRUD(t *testing.T) {
	ctx := context.Background()
	s, db := freshStore(t)
	defer db.Close()

	if err := s.CreateTenant(ctx, "alpha", "A"); err != nil {
		t.Fatal(err)
	}

	if err := s.PutSecret(ctx, "alpha", "OPENAI_API_KEY", []byte("ENC1")); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSecret(ctx, "alpha", "OPENAI_BASE_URL", []byte("ENC2")); err != nil {
		t.Fatal(err)
	}

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
