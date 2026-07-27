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
	body := landingBody(t)
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
