package browser

import "testing"

func TestValidateNavURL(t *testing.T) {
	cases := []struct {
		url     string
		wantErr bool
	}{
		{"https://example.com", false},
		{"http://example.com/path?q=1", false},
		{"ftp://example.com", true},
		{"file:///etc/passwd", true},
		{"javascript:alert(1)", true},
		{"", true},
		{"not a url", true},
	}
	for _, c := range cases {
		err := validateNavURL(c.url)
		if (err != nil) != c.wantErr {
			t.Fatalf("validateNavURL(%q) err=%v wantErr=%v", c.url, err, c.wantErr)
		}
	}
}
