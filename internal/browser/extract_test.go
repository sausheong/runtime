package browser

import (
	"strings"
	"testing"
)

func TestExtractText(t *testing.T) {
	html := `<html><head><title>T</title><style>.x{color:red}</style>
	<script>var a=1;</script></head>
	<body><nav>menu menu</nav><main><h1>Hello</h1><p>World  of   text.</p>
	<p>Second line.</p></main><footer>foot</footer></body></html>`
	got := ExtractText(html)
	if strings.Contains(got, "var a=1") || strings.Contains(got, "color:red") {
		t.Fatalf("script/style leaked into extract: %q", got)
	}
	if !strings.Contains(got, "Hello") || !strings.Contains(got, "World of text.") {
		t.Fatalf("expected body text, got %q", got)
	}
	// Whitespace collapsed: no run of 2+ spaces.
	if strings.Contains(got, "  ") {
		t.Fatalf("whitespace not collapsed: %q", got)
	}
}

func TestExtractMalformedHTMLNoPanic(t *testing.T) {
	// Must not panic on broken markup.
	_ = ExtractText("<div><p>unclosed <b>bold")
	_ = ExtractText("")
	_ = ExtractText("plain text no tags")
}
