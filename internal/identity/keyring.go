package identity

import (
	"errors"
	"fmt"
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

func aadFor(tenant, name string) []byte {
	return []byte(tenant + "\x00" + name)
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
