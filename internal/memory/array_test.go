package memory

import (
	"reflect"
	"testing"
)

func TestTextArray_RoundTrip(t *testing.T) {
	cases := [][]string{
		nil,
		{},
		{"a"},
		{"a", "b", "c"},
		{"has space", `has"quote`, `has\backslash`, "has,comma"},
	}
	for _, in := range cases {
		v, err := textArray(in).Value()
		if err != nil {
			t.Fatalf("value(%v): %v", in, err)
		}
		var out textArray
		if err := out.Scan(v.(string)); err != nil {
			t.Fatalf("scan(%q): %v", v, err)
		}
		want := in
		if len(in) == 0 {
			want = nil
		}
		if !reflect.DeepEqual([]string(out), want) {
			t.Fatalf("round-trip: in=%v got=%v", in, []string(out))
		}
	}
}
