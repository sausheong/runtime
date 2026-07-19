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

	if err := s.PutSecret(ctx, "alpha", "OPENAI_API_KEY", []byte("ENC1"), CredTypeStatic); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSecret(ctx, "alpha", "OPENAI_BASE_URL", []byte("ENC2"), CredTypeStatic); err != nil {
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
	if err := s.PutSecret(ctx, "alpha", "OPENAI_API_KEY", []byte("ENC1b"), CredTypeStatic); err != nil {
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
	if err := s.PutSecret(ctx, "alpha", "K", []byte("X"), CredTypeStatic); err != nil {
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
	if err := s.PutSecret(ctx, "alpha", "LEG", legacyBlob, CredTypeStatic); err != nil {
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
