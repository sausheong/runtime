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
