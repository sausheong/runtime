package browser

import (
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// ExtractText renders an HTML document to clean, readable plain text: script,
// style, noscript, and nav/footer chrome are dropped; text nodes are joined and
// runs of whitespace collapsed to single spaces, with block elements separated
// by newlines. Malformed HTML never panics (the tokenizer is lenient); on a
// parse failure the raw input is returned with whitespace collapsed.
func ExtractText(htmlStr string) string {
	doc, err := html.Parse(strings.NewReader(htmlStr))
	if err != nil {
		return collapseWS(htmlStr)
	}
	var b strings.Builder
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.DataAtom {
			case atom.Script, atom.Style, atom.Noscript, atom.Nav, atom.Footer, atom.Head:
				return // skip the subtree
			}
		}
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		// Block-level boundary → newline so paragraphs don't run together.
		if n.Type == html.ElementNode && isBlock(n.DataAtom) {
			b.WriteString("\n")
		}
	}
	walk(doc)
	return collapseWS(b.String())
}

func isBlock(a atom.Atom) bool {
	switch a {
	case atom.P, atom.Div, atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6,
		atom.Li, atom.Br, atom.Tr, atom.Section, atom.Article, atom.Header:
		return true
	}
	return false
}

// collapseWS trims each line and collapses interior whitespace runs to single
// spaces, dropping blank lines.
func collapseWS(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		ln = strings.Join(strings.Fields(ln), " ")
		if ln != "" {
			out = append(out, ln)
		}
	}
	return strings.Join(out, "\n")
}
