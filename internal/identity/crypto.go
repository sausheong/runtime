package identity

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

// Cipher seals and opens secret values with AES-256-GCM. Each Seal prepends a
// fresh random 12-byte nonce to the returned ciphertext; Open expects that
// layout. The 32-byte key comes from the operator (RUNTIME_SECRETS_KEY).
type Cipher struct {
	aead cipher.AEAD
}

// NewCipher builds a Cipher from a 32-byte (AES-256) key. Any other length is an
// error — a misconfigured key must be caught at startup, not at spawn time.
func NewCipher(key []byte) (*Cipher, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("identity: secrets key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Cipher{aead: aead}, nil
}

// Seal returns nonce || GCM(plaintext). The plaintext may be any bytes.
func (c *Cipher) Seal(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return c.aead.Seal(nonce, nonce, plaintext, nil), nil
}

// Open reverses Seal. It errors on a too-short input or any authentication
// failure (tampering / wrong key).
func (c *Cipher) Open(blob []byte) ([]byte, error) {
	ns := c.aead.NonceSize()
	if len(blob) < ns {
		return nil, errors.New("identity: ciphertext shorter than nonce")
	}
	nonce, ct := blob[:ns], blob[ns:]
	return c.aead.Open(nil, nonce, ct, nil)
}
