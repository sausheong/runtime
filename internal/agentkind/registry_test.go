package agentkind

import "testing"

func TestGetKnownKinds(t *testing.T) {
	for _, k := range []string{"", "testagent", "nutrition"} {
		if _, ok := Get(k); !ok {
			t.Errorf("kind %q: expected a builder", k)
		}
	}
	if _, ok := Get("does-not-exist"); ok {
		t.Error("unknown kind should not resolve")
	}
}
