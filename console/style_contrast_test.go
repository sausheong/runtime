package console

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The console's palette is authored in OKLCH, which no Go stdlib package parses
// and which `getComputedStyle` in a browser returns unresolved. Every accessible
// -contrast claim in DESIGN.md therefore rests on a conversion, and a conversion
// nobody runs in CI is a claim nobody checks. These tests convert
// OKLCH -> OKLab -> linear sRGB -> WCAG relative luminance and assert the two
// floors that actually bit us:
//
//   - 4.5:1 for text, against the DARKEST surface the text can land on. Measuring
//     against the lightest surface flatters every token; .subtitle sits directly
//     on --bg, not on --surface.
//   - 3.0:1 (WCAG 1.4.11) for the boundary of an interactive control. This is the
//     one that regressed silently: --border-strong measured 1.64:1 against its own
//     fill, so the design system was less legible than the browser default it
//     replaced.

// oklchToLinearSRGB converts an OKLCH triple to linear-light sRGB.
func oklchToLinearSRGB(l, c, hDeg float64) [3]float64 {
	h := hDeg * math.Pi / 180
	a, b := c*math.Cos(h), c*math.Sin(h)
	lc := math.Pow(l+0.3963377774*a+0.2158037573*b, 3)
	mc := math.Pow(l-0.1055613458*a-0.0638541728*b, 3)
	sc := math.Pow(l-0.0894841775*a-1.2914855480*b, 3)
	return [3]float64{
		4.0767416621*lc - 3.3077115913*mc + 0.2309699292*sc,
		-1.2684380046*lc + 2.6097574011*mc - 0.3413193965*sc,
		-0.0041960863*lc - 0.7034186147*mc + 1.7076147010*sc,
	}
}

func relLuminance(rgb [3]float64) float64 {
	return 0.2126*rgb[0] + 0.7152*rgb[1] + 0.0722*rgb[2]
}

func contrastRatio(a, b [3]float64) float64 {
	x, y := relLuminance(a), relLuminance(b)
	if y > x {
		x, y = y, x
	}
	return (x + 0.05) / (y + 0.05)
}

// inSRGBGamut reports whether a colour survives the trip to a screen unclipped.
// An out-of-gamut value renders as something other than what was computed, so
// its measured ratio would be fiction.
func inSRGBGamut(rgb [3]float64) bool {
	for _, c := range rgb {
		if c < -0.001 || c > 1.001 {
			return false
		}
	}
	return true
}

var oklchRe = regexp.MustCompile(`oklch\(\s*([0-9.]+)\s+([0-9.]+)\s+([0-9.]+)`)

// tokens parses the :root custom properties out of the real stylesheet, so the
// test cannot drift from the shipped values.
func tokens(t *testing.T) map[string][3]float64 {
	t.Helper()
	css, err := assets.ReadFile("static/style.css")
	if err != nil {
		t.Fatalf("read stylesheet: %v", err)
	}
	out := map[string][3]float64{}
	for _, line := range strings.Split(string(css), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "--") {
			continue
		}
		name, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		m := oklchRe.FindStringSubmatch(val)
		if m == nil {
			continue // shadows, radii, font stacks, and the /alpha shadow colours
		}
		l, _ := strconv.ParseFloat(m[1], 64)
		c, _ := strconv.ParseFloat(m[2], 64)
		h, _ := strconv.ParseFloat(m[3], 64)
		out[strings.TrimSpace(name)] = oklchToLinearSRGB(l, c, h)
	}
	if len(out) < 15 {
		t.Fatalf("parsed only %d OKLCH tokens; the stylesheet format probably changed", len(out))
	}
	return out
}

// Calibration: if the converter is wrong, every other assertion here is
// meaningless. White-on-black is exactly 21:1 by definition.
func TestContrast_ConverterIsCalibrated(t *testing.T) {
	white := oklchToLinearSRGB(1, 0, 0)
	black := oklchToLinearSRGB(0, 0, 0)
	if got := contrastRatio(white, black); math.Abs(got-21) > 0.05 {
		t.Fatalf("white/black should be 21:1, got %.2f — converter is wrong", got)
	}
}

func TestContrast_TextClearsAAOnEverySurface(t *testing.T) {
	tk := tokens(t)
	surfaces := []string{"--bg", "--surface", "--surface-sunk"}
	// Every token used as text, and the floor it must clear on the worst surface.
	text := []string{"--text", "--muted", "--muted-strong", "--accent", "--accent-hover",
		"--success-text", "--danger", "--warning-text"}

	for _, name := range text {
		fg, ok := tk[name]
		if !ok {
			t.Fatalf("token %s missing from the stylesheet", name)
		}
		worst, worstOn := math.Inf(1), ""
		for _, s := range surfaces {
			if r := contrastRatio(fg, tk[s]); r < worst {
				worst, worstOn = r, s
			}
		}
		if worst < 4.5 {
			t.Errorf("%s: %.2f:1 on %s, below the 4.5:1 AA floor", name, worst, worstOn)
		}
	}
}

// WCAG 1.4.11: the boundary of a user-interface component needs 3.0:1. This is
// distinct from the decorative --border, which is deliberately faint.
func TestContrast_ControlBoundariesClearNonTextFloor(t *testing.T) {
	tk := tokens(t)
	surfaces := []string{"--bg", "--surface", "--surface-sunk"}
	controls := []string{"--border-strong", "--accent", "--danger-control"}

	for _, name := range controls {
		c, ok := tk[name]
		if !ok {
			t.Fatalf("token %s missing from the stylesheet", name)
		}
		worst, worstOn := math.Inf(1), ""
		for _, s := range surfaces {
			if r := contrastRatio(c, tk[s]); r < worst {
				worst, worstOn = r, s
			}
		}
		if worst < 3.0 {
			t.Errorf("%s: %.2f:1 on %s, below the 3.0:1 non-text floor (WCAG 1.4.11). "+
				"A control's border is the only thing marking where a field begins.",
				name, worst, worstOn)
		}
	}
}

// Badge text sits on its own tint, not on a page surface.
func TestContrast_BadgeTextOnItsOwnTint(t *testing.T) {
	tk := tokens(t)
	pairs := [][2]string{
		{"--success-text", "--success-bg"},
		{"--danger", "--danger-soft"},
		{"--warning-text", "--warning-bg"},
		{"--accent", "--accent-soft"},
	}
	for _, p := range pairs {
		fg, bg := tk[p[0]], tk[p[1]]
		if r := contrastRatio(fg, bg); r < 4.5 {
			t.Errorf("%s on %s: %.2f:1, below 4.5:1", p[0], p[1], r)
		}
	}
}

// A filled button's label must be legible on the fill.
func TestContrast_ButtonLabelOnAccentFill(t *testing.T) {
	tk := tokens(t)
	for _, fill := range []string{"--accent", "--accent-hover"} {
		if r := contrastRatio(tk["--surface"], tk[fill]); r < 4.5 {
			t.Errorf("--surface on %s: %.2f:1, below 4.5:1", fill, r)
		}
	}
}

// An out-of-gamut OKLCH value is clipped by the browser, so it renders as a
// different colour than the one every ratio above was computed from.
func TestContrast_AllTokensAreInSRGBGamut(t *testing.T) {
	for name, rgb := range tokens(t) {
		if !inSRGBGamut(rgb) {
			t.Errorf("%s is outside the sRGB gamut %v; it will be clipped on screen "+
				"and its measured contrast will not match what renders", name, rgb)
		}
	}
}
