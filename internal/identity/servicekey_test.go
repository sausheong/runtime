package identity

import (
	"strings"
	"testing"
)

func TestMintServiceKey_ShapeAndParse(t *testing.T) {
	mk, err := MintServiceKey()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(mk.ID, "svk-") {
		t.Errorf("id %q must start with svk-", mk.ID)
	}
	if !strings.HasPrefix(mk.Plaintext, mk.ID+".") {
		t.Errorf("plaintext %q must start with id+'.'", mk.Plaintext)
	}
	if mk.Hash == "" || mk.Hash == mk.Plaintext {
		t.Errorf("hash must be set and not equal plaintext")
	}
	id, secret, ok := ParseServiceKey(mk.Plaintext)
	if !ok || id != mk.ID || secret == "" {
		t.Fatalf("ParseServiceKey(%q) = %q,%q,%v", mk.Plaintext, id, secret, ok)
	}
}

func TestParseServiceKey_Rejects(t *testing.T) {
	for _, s := range []string{"", "nope", "svk-only", "bearer xyz", "svk-.empty"} {
		if _, _, ok := ParseServiceKey(s); ok {
			t.Errorf("ParseServiceKey(%q) should fail", s)
		}
	}
}

func TestVerifyKey(t *testing.T) {
	mk, err := MintServiceKey()
	if err != nil {
		t.Fatal(err)
	}
	_, secret, _ := ParseServiceKey(mk.Plaintext)
	if !VerifyKey(mk.Hash, secret) {
		t.Error("VerifyKey should accept the correct secret")
	}
	if VerifyKey(mk.Hash, secret+"x") {
		t.Error("VerifyKey must reject a wrong secret")
	}
	if VerifyKey("not-a-bcrypt-hash", secret) {
		t.Error("VerifyKey must reject a malformed/tampered hash")
	}
}
