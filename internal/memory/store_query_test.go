package memory

import (
	"strings"
	"testing"
)

// Guard: the live-set query MUST filter by actor_id, or the isolation invariant
// silently breaks. Cheap string check, no DB.
func TestLiveSelect_FiltersActor(t *testing.T) {
	if !strings.Contains(liveSelect, "actor_id = $2") {
		t.Fatalf("liveSelect missing actor_id filter:\n%s", liveSelect)
	}
}

// Guard: liveSelect (the memory{list,get} projection) must be fact-only, or
// episode rows leak into the fact tools. Cheap string check, no DB.
func TestLiveSelect_FactOnly(t *testing.T) {
	if !strings.Contains(liveSelect, "e.kind = 'fact'") {
		t.Fatalf("liveSelect must filter kind='fact':\n%s", liveSelect)
	}
}
