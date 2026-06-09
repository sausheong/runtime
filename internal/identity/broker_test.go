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
