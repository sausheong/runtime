package nutrition

import (
	_ "embed"
	"encoding/json"
	"regexp"
	"strings"
)

//go:embed data/sfa_additives.json
var sfaAdditivesJSON []byte

// additive is one row of the SFA permitted-additives table. JSON nulls decode to
// "" automatically for plain string fields.
type additive struct {
	INS        string `json:"ins"`
	ENumber    string `json:"e_number"`
	Name       string `json:"name"`
	NameInRegs string `json:"name_in_regs"`
	Schedule   string `json:"schedule"`
	SFANotes   string `json:"sfa_notes"`
}

// additiveIndex resolves an additive (E-number, INS number, name, or
// colloquialism) to a table entry.
type additiveIndex struct {
	byE     map[string]additive
	byAlias map[string]additive
}

var (
	reParenAny   = regexp.MustCompile(`\(.*\)`)
	reParenInner = regexp.MustCompile(`\([^)]*\)`)
	reStereo     = regexp.MustCompile(`\b[dl]l?\s*\(?\+?\)?-`)
	reNonAlnum   = regexp.MustCompile(`[^a-z0-9 ]`)
	reWhitespace = regexp.MustCompile(`\s+`)
)

// norm normalises an additive name for matching: drop parentheticals, stereo
// markers (L-, DL-, L(+)-), punctuation and extra spaces. Port of Python _norm.
func norm(s string) string {
	s = strings.ReplaceAll(strings.ToLower(s), "en:", "")
	s = reParenInner.ReplaceAllString(s, "") // drop parentheticals, e.g. (L-)
	s = reStereo.ReplaceAllString(s, "")     // drop stereo markers L- DL- L(+)-
	s = strings.ReplaceAll(s, "-", " ")
	s = reNonAlnum.ReplaceAllString(s, "")
	return strings.TrimSpace(reWhitespace.ReplaceAllString(s, " "))
}

// colloquial maps true colloquialisms (absent from the PDF) to the additive's
// formal name, which the alias index resolves. Copied from Python COLLOQUIAL.
var colloquial = map[string]string{
	"msg":                   "monosodium glutamate",
	"soy lecithin":          "lecithin",
	"soya lecithin":         "lecithin",
	"vitamin c":             "ascorbic acid",
	"baking soda":           "sodium bicarbonate",
	"cream of tartar":       "potassium acid tartrate",
	"mono and diglycerides": "mono and diglycerides of fatty acids",
}

// consumerNotes overlays consumer-relevant warnings the PDF's terse Notes column
// lacks, keyed by E-number. Copied from Python CONSUMER_NOTES.
var consumerNotes = map[string]string{
	"102": "Linked to hyperactivity in children in some studies; specific labelling required.",
	"110": "EU requires a hyperactivity warning; SFA permits with disclosure.",
	"211": "Can form benzene when combined with ascorbic acid (Vitamin C) — flag if both present.",
	"621": "MSG — safe at normal dietary intake; some report sensitivity.",
	"951": "Aspartame — must carry a phenylalanine warning for PKU sufferers.",
	"407": "Carrageenan — some contested studies on gut inflammation.",
	"471": "May contain trans fats not separately listed on the label.",
}

// newAdditiveIndex unmarshals the embedded SFA table and builds the lookup maps.
func newAdditiveIndex() *additiveIndex {
	var entries []additive
	if err := json.Unmarshal(sfaAdditivesJSON, &entries); err != nil {
		panic(err)
	}
	idx := &additiveIndex{
		byE:     make(map[string]additive),
		byAlias: make(map[string]additive),
	}
	setDefault := func(m map[string]additive, k string, v additive) {
		if _, ok := m[k]; !ok {
			m[k] = v
		}
	}
	for _, e := range entries {
		// Index by both E-number and INS number; many entries carry their number
		// only in the INS column.
		for _, num := range []string{e.ENumber, e.INS} {
			if num != "" {
				setDefault(idx.byE, strings.ToLower(num), e)
				base := reParenAny.ReplaceAllString(num, "") // base, e.g. '500'
				setDefault(idx.byE, strings.ToLower(base), e)
			}
		}
		for _, n := range []string{e.Name, e.NameInRegs} {
			if n != "" {
				for _, part := range strings.Split(n, "/") {
					setDefault(idx.byAlias, norm(part), e)
				}
			}
		}
	}
	return idx
}

// lookup runs the core resolution against the supplied query without hint fallback.
func (idx *additiveIndex) lookup(query string) *additive {
	n := norm(query)
	compact := strings.ReplaceAll(n, " ", "")
	eKey := compact
	if len(compact) >= 2 && compact[0] == 'e' && compact[1] >= '0' && compact[1] <= '9' {
		eKey = compact[1:]
	}
	if e, ok := idx.byE[eKey]; ok {
		return &e
	}
	if e, ok := idx.byAlias[n]; ok {
		return &e
	}
	if e, ok := idx.byAlias[norm(colloquial[n])]; ok {
		return &e
	}
	return nil
}

// resolve resolves an E-number or additive name to a table entry, or nil. If the
// additive misses and a hint is supplied, the hint is tried as the additive.
// Port of Python _resolve_additive (without the learned-alias part).
func (idx *additiveIndex) resolve(additiveQuery, hint string) *additive {
	if e := idx.lookup(additiveQuery); e != nil {
		return e
	}
	if hint != "" {
		return idx.lookup(hint)
	}
	return nil
}

// format renders a resolved entry into the "permitted / not found" string.
// Port of Python _format_entry.
func (idx *additiveIndex) format(e *additive, raw string) string {
	if e == nil {
		return raw + ": Not found in the SFA permitted-additives list. It may be " +
			"an unpermitted additive, a vitamin/nutrient, or a non-specific " +
			"label term — worth noting rather than assuming it is permitted."
	}
	label := e.Name
	if e.ENumber != "" {
		label = "E" + e.ENumber
	}
	parts := []string{label + " (" + e.Name + "): Permitted by SFA"}
	if e.Schedule != "" {
		parts = append(parts, "under "+e.Schedule)
	}
	tail := "."
	if note, ok := consumerNotes[e.ENumber]; ok {
		tail = ". " + note
	}
	return strings.Join(parts, " ") + tail
}
