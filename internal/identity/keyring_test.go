package identity

import (
	"encoding/base64"
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

func TestKeyring_AADNoSeparatorCollision(t *testing.T) {
	kr, _ := NewKeyring(map[string]*Cipher{"v1": mkCipher(t, 1)}, "v1", "v1")
	// Two (tenant,name) pairs that would share AAD under a plain "\x00" separator.
	blob, err := kr.Seal("a\x00b", "c", []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kr.Open("a", "b\x00c", blob); err == nil {
		t.Fatal("AAD collision: blob sealed for (\"a\\x00b\",\"c\") opened for (\"a\",\"b\\x00c\")")
	}
	// Sanity: it still opens for its own exact pair.
	got, err := kr.Open("a\x00b", "c", blob)
	if err != nil || string(got) != "secret" {
		t.Fatalf("round-trip under exact pair failed: got=%q err=%v", got, err)
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
}

func TestNewKeyring_RejectsOverlongKeyID(t *testing.T) {
	long := make([]byte, 256)
	for i := range long {
		long[i] = 'a'
	}
	if _, err := NewKeyring(map[string]*Cipher{string(long): mkCipher(t, 1)}, string(long), ""); err == nil {
		t.Fatal("NewKeyring accepted a 256-byte key id, want error")
	}
}

func b64key(seed byte) string {
	k := make([]byte, 32)
	for i := range k {
		k[i] = seed + byte(i)
	}
	return base64.StdEncoding.EncodeToString(k)
}

func TestParseKeyring_AllEmptyDisabled(t *testing.T) {
	kr, err := ParseKeyring("", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if kr != nil {
		t.Fatal("all-empty config must yield nil keyring (feature disabled)")
	}
}

func TestParseKeyring_LegacySingleKey(t *testing.T) {
	kr, err := ParseKeyring("", "", b64key(1))
	if err != nil {
		t.Fatal(err)
	}
	if kr.PrimaryID() != "v1" || kr.NumKeys() != 1 || kr.legacyID != "v1" {
		t.Fatalf("legacy single-key mismatch: primary=%q n=%d legacy=%q", kr.PrimaryID(), kr.NumKeys(), kr.legacyID)
	}
}

func TestParseKeyring_MultiKeyPrimary(t *testing.T) {
	kr, err := ParseKeyring("v1:"+b64key(1)+",v2:"+b64key(9), "v2", "")
	if err != nil {
		t.Fatal(err)
	}
	if kr.PrimaryID() != "v2" || kr.NumKeys() != 2 || kr.legacyID != "" {
		t.Fatalf("multi-key mismatch: primary=%q n=%d legacy=%q", kr.PrimaryID(), kr.NumKeys(), kr.legacyID)
	}
}

func TestParseKeyring_LegacyKeyNamesRingEntry(t *testing.T) {
	// RUNTIME_SECRETS_KEY equals the v1 entry's bytes ⇒ v1 is the legacy id.
	kr, err := ParseKeyring("v1:"+b64key(1)+",v2:"+b64key(9), "v2", b64key(1))
	if err != nil {
		t.Fatal(err)
	}
	if kr.legacyID != "v1" {
		t.Fatalf("legacy id = %q, want v1", kr.legacyID)
	}
}

func TestParseKeyring_Errors(t *testing.T) {
	cases := []struct {
		name, keys, primary, legacy string
	}{
		{"bad base64", "v1:!!!notb64", "v1", ""},
		{"short key", "v1:" + base64.StdEncoding.EncodeToString(make([]byte, 16)), "v1", ""},
		{"dup id", "v1:" + b64key(1) + ",v1:" + b64key(9), "v1", ""},
		{"no colon", "v1" + b64key(1), "v1", ""},
		{"primary missing", "v1:" + b64key(1), "", ""},
		{"primary not in ring", "v1:" + b64key(1), "v9", ""},
		{"legacy not in ring", "v1:" + b64key(1), "v1", b64key(7)},
	}
	for _, c := range cases {
		if _, err := ParseKeyring(c.keys, c.primary, c.legacy); err == nil {
			t.Errorf("%s: expected error, got nil", c.name)
		}
	}
}
