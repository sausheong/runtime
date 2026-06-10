package sandbox

import "testing"

func TestConfinePath(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"data.csv", "/workspace/data.csv", true},
		{"sub/dir/f.txt", "/workspace/sub/dir/f.txt", true},
		{"/workspace/f.txt", "/workspace/f.txt", true},
		{"/workspace/sub/../f.txt", "/workspace/f.txt", true},
		{"", "", false},
		{"..", "", false},
		{"../etc/passwd", "", false},
		{"/etc/passwd", "", false},
		{"/workspace/../etc/passwd", "", false},
		{"sub/../../etc", "", false},
		{"/workspacefake/f.txt", "", false}, // prefix trick
	}
	for _, c := range cases {
		got, err := confinePath(c.in)
		if c.ok && (err != nil || got != c.want) {
			t.Errorf("confinePath(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
		if !c.ok && err == nil {
			t.Errorf("confinePath(%q) = %q; want error", c.in, got)
		}
	}
}
