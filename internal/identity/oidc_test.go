package identity

import (
	"context"
	"errors"
	"testing"
)

// fakeVerifier returns a fixed subject for a known token, error otherwise.
type fakeVerifier struct{ good, sub string }

func (f fakeVerifier) Verify(_ context.Context, raw string) (string, error) {
	if raw == f.good {
		return f.sub, nil
	}
	return "", errors.New("bad token")
}

func TestOIDC_VerifierContract(t *testing.T) {
	v := fakeVerifier{good: "tok123", sub: "alice@corp"}
	sub, err := v.Verify(context.Background(), "tok123")
	if err != nil || sub != "alice@corp" {
		t.Fatalf("got %q,%v want alice@corp,nil", sub, err)
	}
	if _, err := v.Verify(context.Background(), "nope"); err == nil {
		t.Fatal("expected error for bad token")
	}
}

func TestLooksLikeJWT(t *testing.T) {
	if !looksLikeJWT("aaa.bbb.ccc") {
		t.Error("three-segment dotted string should look like a JWT")
	}
	for _, s := range []string{"", "svk-abc.def", "one.two", "nodots"} {
		if looksLikeJWT(s) {
			t.Errorf("%q should not look like a JWT", s)
		}
	}
}
