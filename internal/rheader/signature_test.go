package rheader

import "testing"

// TestPublicKeyForDerivesTheVerificationKey pins the property that makes the
// operator-supplied public key unnecessary: it is a function of the private
// key, so a derived value must satisfy the same pair check a configured one
// does. Requiring both could only ever produce a mismatch.
func TestPublicKeyForDerivesTheVerificationKey(t *testing.T) {
	priv, pub, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	derived, err := PublicKeyFor(priv)
	if err != nil {
		t.Fatal(err)
	}
	if derived != pub {
		t.Fatalf("derived public key %q != generated %q", derived, pub)
	}
	if err := ValidateKeyPair(priv, derived); err != nil {
		t.Fatalf("derived key rejected by ValidateKeyPair: %v", err)
	}
	if _, err := PublicKeyFor("not-a-key"); err == nil {
		t.Fatal("malformed private key accepted")
	}
}
