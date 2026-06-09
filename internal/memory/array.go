package memory

import (
	"database/sql/driver"
	"fmt"
	"strings"
)

// textArray adapts a Go []string to a Postgres text[] for both binding (Value)
// and scanning (Scan), so the package needs no third-party array dependency
// beyond the pgx stdlib driver already in use.
type textArray []string

// Value renders the slice as a Postgres array literal. Elements are quoted and
// internal quotes/backslashes escaped. nil/empty ⇒ empty array literal.
func (a textArray) Value() (driver.Value, error) {
	if len(a) == 0 {
		return "{}", nil
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, s := range a {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('"')
		b.WriteString(strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String(), nil
}

// Scan parses a Postgres text[] rendering ({a,b,"c d"}) back into a []string.
func (a *textArray) Scan(src any) error {
	if src == nil {
		*a = nil
		return nil
	}
	var s string
	switch v := src.(type) {
	case string:
		s = v
	case []byte:
		s = string(v)
	default:
		return fmt.Errorf("textArray: unsupported scan type %T", src)
	}
	*a = parsePGTextArray(s)
	return nil
}

// parsePGTextArray parses {a,b,"c, d"} into elements, handling quotes and
// backslash escapes. Returns nil for the empty array.
func parsePGTextArray(s string) []string {
	s = strings.TrimSpace(s)
	if len(s) < 2 || s[0] != '{' || s[len(s)-1] != '}' {
		return nil
	}
	inner := s[1 : len(s)-1]
	if inner == "" {
		return nil
	}
	var out []string
	var cur strings.Builder
	inQuotes := false
	escaped := false
	flush := func() { out = append(out, cur.String()); cur.Reset() }
	for i := 0; i < len(inner); i++ {
		c := inner[i]
		switch {
		case escaped:
			cur.WriteByte(c)
			escaped = false
		case c == '\\':
			escaped = true
		case c == '"':
			inQuotes = !inQuotes
		case c == ',' && !inQuotes:
			flush()
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return out
}
