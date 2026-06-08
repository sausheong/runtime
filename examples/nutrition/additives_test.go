package nutrition

import (
	"strings"
	"testing"
)

func TestResolveAdditive(t *testing.T) {
	idx := newAdditiveIndex()
	cases := []struct {
		name, query string
		wantFound   bool
		wantInName  string
	}{
		// wantInName asserts WHICH entry resolved (a substring of the formatted
		// output), so a bug that resolved to the wrong permitted additive is caught.
		{"by e-number", "E621", true, "Monosodium"},
		{"by e-number lowercase no prefix", "621", true, "Monosodium"},
		{"by name", "Monosodium L- glutamate", true, "E621"},
		{"by colloquial msg", "MSG", true, "E621"},
		// "vitamin c" matches the table's own "Vitamin C" row via the alias index
		// BEFORE the colloquial→ascorbic fallback — matching the Python order.
		{"by name vitamin c", "vitamin c", true, "Vitamin C"},
		// colloquial fallback: "baking soda" has no table row, only the map entry.
		{"by colloquial baking soda", "baking soda", true, "Sodium hydrogen carbonate"},
		{"miss", "unobtainium", false, "Not found"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := idx.format(idx.resolve(c.query, ""), c.query)
			if c.wantFound && !containsFold(out, "Permitted") {
				t.Errorf("resolve(%q): want permitted, got %q", c.query, out)
			}
			if !c.wantFound && !containsFold(out, "not found") {
				t.Errorf("resolve(%q): want not-found, got %q", c.query, out)
			}
			if c.wantInName != "" && !containsFold(out, c.wantInName) {
				t.Errorf("resolve(%q): want %q in output, got %q", c.query, c.wantInName, out)
			}
		})
	}
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}
