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
