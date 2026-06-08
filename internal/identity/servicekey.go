package identity

import (
	"crypto/rand"
	"encoding/hex"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// MintedKey is a freshly created service key. Plaintext is returned to the
// caller exactly once; only Hash is persisted.
type MintedKey struct {
	ID        string // "svk-<16 hex>"
	Plaintext string // "svk-<id>.<secret>" — shown once
	Hash      string // bcrypt hash of the secret
}

// MintServiceKey generates a new service key with a random id and secret.
func MintServiceKey() (MintedKey, error) {
	idBytes := make([]byte, 8)
	if _, err := rand.Read(idBytes); err != nil {
		return MintedKey{}, err
	}
	secretBytes := make([]byte, 24)
	if _, err := rand.Read(secretBytes); err != nil {
		return MintedKey{}, err
	}
	id := "svk-" + hex.EncodeToString(idBytes)
	secret := hex.EncodeToString(secretBytes)
	hash, err := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.DefaultCost)
	if err != nil {
		return MintedKey{}, err
	}
	return MintedKey{ID: id, Plaintext: id + "." + secret, Hash: string(hash)}, nil
}

// ParseServiceKey splits "svk-<id>.<secret>" into id ("svk-<id>") and secret.
// ok is false if the string is not a well-formed service key.
func ParseServiceKey(s string) (id, secret string, ok bool) {
	if !strings.HasPrefix(s, "svk-") {
		return "", "", false
	}
	dot := strings.IndexByte(s, '.')
	if dot < 0 {
		return "", "", false
	}
	id, secret = s[:dot], s[dot+1:]
	if id == "svk-" || secret == "" {
		return "", "", false
	}
	return id, secret, true
}

// VerifyKey reports whether secret matches the stored bcrypt hash. bcrypt's
// comparison is constant-time with respect to the secret.
func VerifyKey(hash, secret string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(secret)) == nil
}
