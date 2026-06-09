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
