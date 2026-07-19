package rheader

import (
	"net/http"
	"testing"
)

func TestConsts(t *testing.T) {
	if Prefix != "X-Runtime-" {
		t.Fatalf("Prefix = %q", Prefix)
	}
	// Every claim header must carry the reserved prefix under canonical casing,
	// so the anti-spoof strip (which deletes by prefix) also removes them.
	for _, h := range []string{User, Tenant, Role} {
		if http.CanonicalHeaderKey(h)[:len(Prefix)] != Prefix {
			t.Fatalf("header %q does not start with %q", h, Prefix)
		}
	}
}
