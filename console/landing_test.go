package console

import (
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
)

// landingBody renders the public landing page (OIDC on, the production shape).
func landingBody(t *testing.T) string {
	t.Helper()
	h := Handler(testReg(t), nil, OIDCConfig{
		Enabled:     true,
		AuthCodeURL: func(state string) string { return "https://idp.example/authorize?state=" + state },
	}, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 200 {
		t.Fatalf("GET /: code=%d want 200", rec.Code)
	}
	return rec.Body.String()
}

// The landing page is the only surface that describes Runtime to someone who
// has not signed in, which makes it the one most tempted to overstate. Two
// claims on it are not matters of copy taste:
//
//   - Runtime is PRE-RELEASE. README.md and runtime.md both say so.
//   - Runtime has NO LICENCE. runtime.md states outright that it "should not be
//     described as open source" until one is added.
//
// A page that drops either is not merely off-brand, it is wrong in a way that
// could lead someone to depend on this. Assert both survive.
func TestLanding_DisclosesPreReleaseAndLicence(t *testing.T) {
	// Collapse runs of whitespace: the copy is wrapped for readability in the
	// template, so a phrase can straddle a newline. Matching raw HTML made this
	// test fail on a reflow that changed nothing a reader sees.
	body := strings.Join(strings.Fields(landingBody(t)), " ")
	for _, want := range []string{"pre-release", "does not yet include a licence"} {
		if !strings.Contains(body, want) {
			t.Errorf("landing page no longer discloses %q; this is a factual claim, not copy", want)
		}
	}
	// "open source" may appear only inside the disclaimer that Runtime is NOT
	// open source. A bare positive use is the failure this guards.
	if strings.Contains(body, "open source") &&
		!strings.Contains(body, "should not be described as open source") {
		t.Error("landing page uses \"open source\" outside the disclaimer; there is no licence")
	}
}

// The tagged-release version is stated on the page and lives in README.md. They
// drift the moment someone tags a release and updates only one. Read the README
// rather than hardcoding, so the next release makes this fail loudly in one
// place instead of shipping a stale claim to the public front door.
func TestLanding_VersionMatchesREADME(t *testing.T) {
	readme, err := os.ReadFile("../README.md")
	if err != nil {
		t.Skipf("README.md unreadable (%v); nothing to cross-check", err)
	}
	m := regexp.MustCompile(`latest tagged release is ` + "`" + `(v[0-9][^` + "`" + `]*)` + "`").
		FindSubmatch(readme)
	if m == nil {
		t.Skip("README.md no longer states a tagged release in the expected form")
	}
	version := string(m[1])
	if !strings.Contains(landingBody(t), version) {
		t.Errorf("landing page does not name %s, the tagged release README.md declares; "+
			"the public page is claiming a different version from the docs", version)
	}
}

// Air-gap is a hard constraint (PRODUCT.md): the deployment has no route to the
// public internet, so every asset must come from the binary. A landing page is
// where a font CDN or an analytics tag gets added by reflex, and the failure is
// silent — it renders fine on a laptop with internet and breaks in the
// deployment the product exists for.
func TestLanding_NoExternalSubresources(t *testing.T) {
	body := landingBody(t)
	// src/href on a loadable subresource. The Google sign-in LINK is a
	// navigation target, not a subresource, so only these attributes are checked.
	re := regexp.MustCompile(`(?i)<(?:script|link|img|iframe|source|video|audio)\b[^>]*\b(?:src|href)="([^"]+)"`)
	for _, m := range re.FindAllStringSubmatch(body, -1) {
		u := m[1]
		if strings.HasPrefix(u, "//") || strings.Contains(u, "://") {
			t.Errorf("landing page loads an external subresource %q; the deployment is air-gapped", u)
		}
	}
	// @import and url() inside any inline style would bypass the check above.
	if strings.Contains(body, "@import") {
		t.Error("landing page uses @import; stylesheets must be served from the binary")
	}
}

// The hero is a drenched dark field, and every rule that makes text legible on
// it is an override of a light-theme rule from the shared console stylesheet.
// That is a cascade fight, and the first attempt lost it silently: `.navbtn
// -onDark` (0,1,0) was outranked by the base `.topbar nav a` (0,1,1), so the
// sign-in label kept its light-theme colour and measured 2.23:1 on plum. It
// looked plausible in a screenshot; only measuring caught it.
//
// A Go test cannot compute the cascade, but it can assert the shape of the fix:
// each on-dark override must be scoped under .topbar-invert so it outranks the
// base rule. Rewriting one back to a bare class fails here.
func TestLanding_OnDarkOverridesOutrankTheBaseTopbar(t *testing.T) {
	css, err := assets.ReadFile("static/style.css")
	if err != nil {
		t.Fatalf("read stylesheet: %v", err)
	}
	src := string(css)
	// Every selector block mentioning navbtn-onDark must also carry the
	// .topbar-invert scope, in the same selector.
	for _, line := range strings.Split(src, "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, ".navbtn-onDark") || !strings.HasSuffix(line, "{") {
			continue
		}
		if !strings.Contains(line, ".topbar-invert") {
			t.Errorf("selector %q is not scoped under .topbar-invert, so the base "+
				"`.topbar nav a` rule (0,1,1) outranks it and the label reverts to the "+
				"light-theme colour on the dark hero", line)
		}
	}
	// And the class must actually be used, or the assertion above is vacuous.
	if !strings.Contains(src, ".topbar-invert nav .navbtn-onDark") {
		t.Error("no scoped .navbtn-onDark rule found; the on-dark sign-in button is unstyled")
	}
}

// The drenched field belongs to the landing page alone. It is applied by a body
// class, and the console's own pages must never pick it up: a plum operator
// console would be a spectacular regression from one stray selector.
func TestLanding_DrenchedStylingIsScopedToTheLandingBody(t *testing.T) {
	css, err := assets.ReadFile("static/style.css")
	if err != nil {
		t.Fatalf("read stylesheet: %v", err)
	}
	for _, line := range strings.Split(string(css), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasSuffix(line, "{") || !strings.Contains(line, "--lp-ink") {
			continue
		}
		t.Errorf("unexpected selector %q referencing the landing ink", line)
	}
	// The only page that sets the landing body class is the landing template.
	for _, f := range []string{"overview.html", "observability.html", "onboarding.html",
		"agent.html", "session.html", "eval-run.html", "select-tenant.html"} {
		b, err := assets.ReadFile("templates/" + f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if strings.Contains(string(b), "on-landing") || strings.Contains(string(b), "topbar-invert") {
			t.Errorf("templates/%s uses landing-only styling; the drenched hero is for / alone", f)
		}
	}
}
