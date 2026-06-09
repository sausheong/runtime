package memory

import (
	"regexp"
	"testing"
	"time"
)

var idRe = regexp.MustCompile(`^mem_\d{4}-\d{2}-\d{2}_[0-9a-f]{8}$`)

func TestGenerateID_Shape(t *testing.T) {
	id := generateID(time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC))
	if !idRe.MatchString(id) {
		t.Fatalf("id %q does not match %s", id, idRe)
	}
}

func TestGenerateID_Unique(t *testing.T) {
	now := time.Now()
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := generateID(now)
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}
