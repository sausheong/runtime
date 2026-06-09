package identity

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// formatV1 marks a new-format secret blob: 0x01 | keyIDLen(1) | keyID | nonce | ct.
// A blob whose first byte is not formatV1 is a legacy M2 row (nonce || ct, nil AAD).
const formatV1 byte = 0x01

// Keyring composes single-key Ciphers into a versioned, multi-key blob format.
// New blobs are sealed under the primary key and carry the key's id; opens select
// the key by the id embedded in the blob and bind (tenant, name) as AAD. A blob
// with no formatV1 prefix is opened with the optional legacy key and nil AAD.
type Keyring struct {
	ciphers   map[string]*Cipher
	primaryID string
	legacyID  string // "" when no legacy (version-less) decrypt key is configured
}

// NewKeyring validates that ciphers is non-empty, primaryID is present, and
// legacyID (if non-empty) is present.
func NewKeyring(ciphers map[string]*Cipher, primaryID, legacyID string) (*Keyring, error) {
	if len(ciphers) == 0 {
		return nil, errors.New("identity: keyring needs at least one key")
	}
	for id := range ciphers {
		if id == "" || len(id) > 255 {
			return nil, fmt.Errorf("identity: key id must be 1-255 bytes, got %d", len(id))
		}
	}
	if _, ok := ciphers[primaryID]; !ok {
		return nil, fmt.Errorf("identity: primary key id %q not in keyring", primaryID)
	}
	if legacyID != "" {
		if _, ok := ciphers[legacyID]; !ok {
			return nil, fmt.Errorf("identity: legacy key id %q not in keyring", legacyID)
		}
	}
	return &Keyring{ciphers: ciphers, primaryID: primaryID, legacyID: legacyID}, nil
}

// PrimaryID is the id new blobs are sealed under.
func (k *Keyring) PrimaryID() string { return k.primaryID }

// NumKeys is the number of keys in the ring (for startup logging).
func (k *Keyring) NumKeys() int { return len(k.ciphers) }

// aadFor builds the AAD bound into a secret's seal: a length-prefixed encoding of
// (tenant, name) so distinct pairs can never collide (a plain separator would let
// e.g. ("a\x00b","c") and ("a","b\x00c") share AAD). Length-prefixing makes the
// binding unambiguous regardless of the bytes in either field.
func aadFor(tenant, name string) []byte {
	out := make([]byte, 0, 8+len(tenant)+len(name))
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(len(tenant)))
	out = append(out, l[:]...)
	out = append(out, tenant...)
	binary.BigEndian.PutUint32(l[:], uint32(len(name)))
	out = append(out, l[:]...)
	out = append(out, name...)
	return out
}

// Seal produces a new-format blob under the primary key, binding (tenant, name).
func (k *Keyring) Seal(tenant, name string, plaintext []byte) ([]byte, error) {
	c := k.ciphers[k.primaryID]
	body, err := c.Seal(plaintext, aadFor(tenant, name))
	if err != nil {
		return nil, err
	}
	id := []byte(k.primaryID)
	out := make([]byte, 0, 2+len(id)+len(body))
	out = append(out, formatV1, byte(len(id)))
	out = append(out, id...)
	out = append(out, body...)
	return out, nil
}

// Open reverses Seal. New-format blobs select the key by embedded id and bind
// (tenant, name) as AAD; on any new-format failure it falls back to the legacy
// path (this rescues a genuine legacy blob whose random nonce starts with the
// formatV1 byte — GCM auth makes a wrong-path success impossible). Version-less
// blobs go straight to the legacy key with nil AAD.
func (k *Keyring) Open(tenant, name string, blob []byte) ([]byte, error) {
	if len(blob) > 0 && blob[0] == formatV1 {
		pt, err := k.openV1(tenant, name, blob)
		if err == nil {
			return pt, nil
		}
		if k.legacyID != "" {
			if lpt, lerr := k.openLegacy(blob); lerr == nil {
				return lpt, nil
			}
		}
		return nil, err
	}
	return k.openLegacy(blob)
}

func (k *Keyring) openV1(tenant, name string, blob []byte) ([]byte, error) {
	if len(blob) < 2 {
		return nil, errors.New("identity: truncated v1 secret header")
	}
	idLen := int(blob[1])
	if idLen == 0 || len(blob) < 2+idLen {
		return nil, errors.New("identity: corrupt v1 secret key id")
	}
	id := string(blob[2 : 2+idLen])
	c, ok := k.ciphers[id]
	if !ok {
		return nil, fmt.Errorf("identity: unknown key id %q", id)
	}
	return c.Open(blob[2+idLen:], aadFor(tenant, name))
}

func (k *Keyring) openLegacy(blob []byte) ([]byte, error) {
	if k.legacyID == "" {
		return nil, errors.New("identity: version-less secret but no legacy key configured")
	}
	return k.ciphers[k.legacyID].Open(blob, nil)
}

// ParseKeyring builds a Keyring from operator env values:
//
//	keysEnv      RUNTIME_SECRETS_KEYS    "id:base64key,id:base64key" (each key 32 bytes)
//	primaryEnv   RUNTIME_SECRETS_PRIMARY id of the key new writes seal under
//	legacyKeyEnv RUNTIME_SECRETS_KEY     base64 of the legacy single key
//
// All empty ⇒ (nil, nil): the feature is disabled. keysEnv empty but legacyKeyEnv
// set ⇒ the M2 back-compat ring {v1: key}, primary and legacy v1. keysEnv set ⇒ a
// multi-key ring; primaryEnv must name a ring entry, and a non-empty legacyKeyEnv
// must match exactly one entry's bytes (that id becomes the version-less decrypt
// key). Any malformed input is an error (runtimed turns it into a fatal startup).
func ParseKeyring(keysEnv, primaryEnv, legacyKeyEnv string) (*Keyring, error) {
	if keysEnv == "" && legacyKeyEnv == "" {
		return nil, nil
	}
	if keysEnv == "" {
		key, err := decodeKey(legacyKeyEnv)
		if err != nil {
			return nil, fmt.Errorf("identity: RUNTIME_SECRETS_KEY: %w", err)
		}
		c, err := NewCipher(key)
		if err != nil {
			return nil, err
		}
		return NewKeyring(map[string]*Cipher{"v1": c}, "v1", "v1")
	}

	ciphers := map[string]*Cipher{}
	rawKeys := map[string][]byte{}
	for _, pair := range strings.Split(keysEnv, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		id, b64, ok := strings.Cut(pair, ":")
		if !ok || id == "" {
			return nil, fmt.Errorf("identity: RUNTIME_SECRETS_KEYS entry %q must be id:base64", pair)
		}
		if _, dup := ciphers[id]; dup {
			return nil, fmt.Errorf("identity: duplicate key id %q in RUNTIME_SECRETS_KEYS", id)
		}
		key, err := decodeKey(b64)
		if err != nil {
			return nil, fmt.Errorf("identity: key %q: %w", id, err)
		}
		c, err := NewCipher(key)
		if err != nil {
			return nil, fmt.Errorf("identity: key %q: %w", id, err)
		}
		ciphers[id] = c
		rawKeys[id] = key
	}
	if primaryEnv == "" {
		return nil, errors.New("identity: RUNTIME_SECRETS_PRIMARY is required when RUNTIME_SECRETS_KEYS is set")
	}

	legacyID := ""
	if legacyKeyEnv != "" {
		lk, err := decodeKey(legacyKeyEnv)
		if err != nil {
			return nil, fmt.Errorf("identity: RUNTIME_SECRETS_KEY: %w", err)
		}
		for id, k := range rawKeys {
			if bytes.Equal(k, lk) {
				legacyID = id
				break
			}
		}
		if legacyID == "" {
			return nil, errors.New("identity: RUNTIME_SECRETS_KEY does not match any key in RUNTIME_SECRETS_KEYS")
		}
	}
	return NewKeyring(ciphers, primaryEnv, legacyID)
}

// decodeKey base64-decodes a 32-byte AES key (length validated by NewCipher; this
// surfaces a clear decode error first).
func decodeKey(b64 string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("not valid base64: %w", err)
	}
	return key, nil
}
