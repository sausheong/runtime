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

func TestExtractEntityAndNestedSkip(t *testing.T) {
	// HTML entities must decode (relied upon, via x/net/html).
	if got := ExtractText("<p>a &amp; b</p>"); !strings.Contains(got, "a & b") {
		t.Fatalf("entity not decoded: %q", got)
	}
	// A block element nested inside a skipped subtree must be fully dropped,
	// while sibling text outside the skipped subtree survives.
	got := ExtractText(`<div>keep<nav>drop<p>alsoDrop</p></nav>tail</div>`)
	if strings.Contains(got, "drop") || strings.Contains(got, "alsoDrop") {
		t.Fatalf("skipped subtree leaked: %q", got)
	}
	if !strings.Contains(got, "keep") || !strings.Contains(got, "tail") {
		t.Fatalf("non-skipped text lost: %q", got)
	}
}
