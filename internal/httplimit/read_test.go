package httplimit

import (
	"strings"
	"testing"
)

func TestReadAllBoundaryAndOverflow(t *testing.T) {
	got, err := ReadAll(strings.NewReader("1234"), 4)
	if err != nil || string(got) != "1234" {
		t.Fatalf("boundary read=%q err=%v", got, err)
	}
	if _, err := ReadAll(strings.NewReader("12345"), 4); err == nil {
		t.Fatal("oversized response accepted")
	}
}

func TestDecodeJSONIsBounded(t *testing.T) {
	var out map[string]string
	if err := DecodeJSON(strings.NewReader(`{"ok":"yes"}`), 12, &out); err != nil {
		t.Fatal(err)
	}
	if out["ok"] != "yes" {
		t.Fatalf("decoded=%v", out)
	}
	if err := DecodeJSON(strings.NewReader(`{"padding":"xxxxxxxx"}`), 8, &out); err == nil {
		t.Fatal("oversized JSON accepted")
	}
}
