package identity

import (
	"bytes"
	"testing"
)

func mkCipher(t *testing.T, seed byte) *Cipher {
	t.Helper()
	k := make([]byte, 32)
	for i := range k {
		k[i] = seed + byte(i)
	}
	c, err := NewCipher(k)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestKeyring_SealOpenRoundTrip(t *testing.T) {
	kr, err := NewKeyring(map[string]*Cipher{"v1": mkCipher(t, 1)}, "v1", "v1")
	if err != nil {
		t.Fatal(err)
	}
	blob, err := kr.Seal("alpha", "OPENAI_API_KEY", []byte("sk-xyz"))
	if err != nil {
		t.Fatal(err)
	}
	if blob[0] != 0x01 {
		t.Fatalf("new blob must start with 0x01, got 0x%02x", blob[0])
	}
	idLen := int(blob[1])
	if string(blob[2:2+idLen]) != "v1" {
		t.Fatalf("embedded key id = %q, want v1", blob[2:2+idLen])
	}
	got, err := kr.Open("alpha", "OPENAI_API_KEY", blob)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "sk-xyz" {
		t.Fatalf("round-trip mismatch: %q", got)
	}
}

func TestKeyring_OpenWrongTenantOrNameFails(t *testing.T) {
	kr, _ := NewKeyring(map[string]*Cipher{"v1": mkCipher(t, 1)}, "v1", "v1")
	blob, _ := kr.Seal("alpha", "K", []byte("v"))
	if _, err := kr.Open("beta", "K", blob); err == nil {
		t.Fatal("open with wrong tenant succeeded (AAD not bound)")
	}
	if _, err := kr.Open("alpha", "OTHER", blob); err == nil {
		t.Fatal("open with wrong name succeeded (AAD not bound)")
	}
}

func TestKeyring_MixedKeyVersions(t *testing.T) {
	cOld, cNew := mkCipher(t, 1), mkCipher(t, 9)
	krOld, _ := NewKeyring(map[string]*Cipher{"v1": cOld}, "v1", "v1")
	blobV1, _ := krOld.Seal("alpha", "K", []byte("old"))

	// Primary moves to v2; v1 still present for decrypt.
	krBoth, _ := NewKeyring(map[string]*Cipher{"v1": cOld, "v2": cNew}, "v2", "v1")
	got, err := krBoth.Open("alpha", "K", blobV1)
	if err != nil {
		t.Fatalf("v1 blob must still open under a v2-primary ring: %v", err)
	}
	if string(got) != "old" {
		t.Fatalf("mixed-version mismatch: %q", got)
	}
	blobV2, _ := krBoth.Seal("alpha", "K", []byte("new"))
	if int(blobV2[1]) != 2 || string(blobV2[2:2+2]) != "v2" {
		t.Fatalf("new seal must use primary v2, got id %q", blobV2[2:2+int(blobV2[1])])
	}
}

func TestKeyring_UnknownKeyID(t *testing.T) {
	cOld := mkCipher(t, 1)
	krOld, _ := NewKeyring(map[string]*Cipher{"v1": cOld}, "v1", "v1")
	blob, _ := krOld.Seal("alpha", "K", []byte("x"))
	// A ring without v1 (and no legacy) cannot open the v1 blob.
	krNew, _ := NewKeyring(map[string]*Cipher{"v2": mkCipher(t, 9)}, "v2", "")
	if _, err := krNew.Open("alpha", "K", blob); err == nil {
		t.Fatal("open with unknown key id succeeded, want error")
	}
}

func TestKeyring_LegacyVersionlessBlob(t *testing.T) {
	cLegacy := mkCipher(t, 1)
	kr, _ := NewKeyring(map[string]*Cipher{"v1": cLegacy}, "v1", "v1")
	// A legacy M2 blob is exactly Cipher.Seal(pt, nil): nonce||ct, no 0x01 prefix.
	legacy, _ := cLegacy.Seal([]byte("sk-legacy"), nil)
	got, err := kr.Open("alpha", "K", legacy)
	if err != nil {
		t.Fatalf("legacy blob must open via the version-less path: %v", err)
	}
	if string(got) != "sk-legacy" {
		t.Fatalf("legacy decrypt mismatch: %q", got)
	}
}

func TestKeyring_LegacyBlobWithLeading01Byte(t *testing.T) {
	cLegacy := mkCipher(t, 1)
	kr, _ := NewKeyring(map[string]*Cipher{"v1": cLegacy}, "v1", "v1")
	// Find a legacy blob whose random nonce happens to start with 0x01, which
	// would otherwise be misdetected as new-format. Open must fall back.
	var legacy []byte
	for i := 0; i < 100000; i++ {
		b, _ := cLegacy.Seal([]byte("sk-collide"), nil)
		if b[0] == 0x01 {
			legacy = b
			break
		}
	}
	if legacy == nil {
		t.Skip("no 0x01-leading nonce in 100k tries (astronomically unlikely)")
	}
	got, err := kr.Open("alpha", "K", legacy)
	if err != nil {
		t.Fatalf("legacy blob with 0x01 nonce must open via fallback: %v", err)
	}
	if string(got) != "sk-collide" {
		t.Fatalf("fallback decrypt mismatch: %q", got)
	}
}

func TestKeyring_NoLegacyKeyRejectsVersionlessBlob(t *testing.T) {
	cLegacy := mkCipher(t, 1)
	legacy, _ := cLegacy.Seal([]byte("x"), nil)
	// Ring with no legacy id cannot open a version-less blob.
	kr, _ := NewKeyring(map[string]*Cipher{"v2": mkCipher(t, 9)}, "v2", "")
	if _, err := kr.Open("alpha", "K", legacy); err == nil {
		t.Fatal("version-less blob opened without a legacy key, want error")
	}
}

func TestNewKeyring_Validation(t *testing.T) {
	c := mkCipher(t, 1)
	if _, err := NewKeyring(map[string]*Cipher{}, "v1", ""); err == nil {
		t.Fatal("empty ring accepted")
	}
	if _, err := NewKeyring(map[string]*Cipher{"v1": c}, "v2", ""); err == nil {
		t.Fatal("primary not in ring accepted")
	}
	if _, err := NewKeyring(map[string]*Cipher{"v1": c}, "v1", "v9"); err == nil {
		t.Fatal("legacy id not in ring accepted")
	}
	kr, err := NewKeyring(map[string]*Cipher{"v1": c}, "v1", "v1")
	if err != nil {
		t.Fatal(err)
	}
	if kr.PrimaryID() != "v1" || kr.NumKeys() != 1 {
		t.Fatalf("accessor mismatch: primary=%q n=%d", kr.PrimaryID(), kr.NumKeys())
	}
	_ = bytes.Equal // keep import if unused above
}
