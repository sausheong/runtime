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
		{"by e-number", "E621", true, "621"},
		{"by e-number lowercase no prefix", "621", true, "621"},
		{"by name", "Monosodium L- glutamate", true, "621"},
		{"by colloquial msg", "MSG", true, "621"},
		{"by colloquial vitamin c", "vitamin c", true, "ascorbic"},
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
		})
	}
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}
