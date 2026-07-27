package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sausheong/runtime/controlplane"
	"github.com/sausheong/runtime/internal/config"
	"github.com/sausheong/runtime/internal/eval"
	"github.com/sausheong/runtime/internal/identity"
)

// The signed-in top bar must be the same menu on every page. It was not: the
// markup was copy-pasted into seven templates, and four of them (observability,
// agent, session, eval-run) had gone stale without "Switch tenant" — so the menu
// gained and lost an item as you navigated, and the one page whose whole purpose
// is switching tenants was the hardest to reach.
//
// The fix was to delete the duplication (templates/_topbar.html), but a shared
// partial only prevents drift while every page actually uses it. Nothing stops
// the next page from pasting its own header, which is exactly how this started.
// So the assertion is made against RENDERED output from the real handler, not
// against the template source: a page that hand-rolls a top bar fails here.

var (
	navHeaderRe = regexp.MustCompile(`(?s)<header class="topbar">.*?</header>`)
	// Matches the brand anchor too, which carries a class before its href.
	navLinkRe = regexp.MustCompile(`<a [^>]*?href="(/ui[^"]*)"`)
)

// wantNavLinks is the menu, in order. Kept here rather than derived from the
// template so that changing the menu is a deliberate two-file edit.
var wantNavLinks = []string{"/ui", "/ui", "/ui/observability", "/ui/onboarding", "/ui/select-tenant"}

// navOf extracts the top bar's link targets from a rendered page. The first
// hit is the brand link (also /ui), which is part of the bar's identity.
func navOf(t *testing.T, h http.Handler, r *http.Request) []string {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("%s: want 200 got %d", r.URL.Path, w.Code)
	}
	header := navHeaderRe.FindString(w.Body.String())
	if header == "" {
		t.Fatalf("%s: no <header class=\"topbar\"> in the rendered page", r.URL.Path)
	}
	var out []string
	for _, m := range navLinkRe.FindAllStringSubmatch(header, -1) {
		out = append(out, m[1])
	}
	return out
}

// multiTenantAdmin reports two memberships for every subject. fakeAdmin2 keys
// users by subject, so it can only ever hold one; with one membership GET
// /ui/select-tenant short-circuits to a redirect and never renders the page
// whose nav this test exists to check.
type multiTenantAdmin struct{ *fakeAdmin2 }

func (m multiTenantAdmin) UsersBySubject(context.Context, string) ([]identity.UserRow, error) {
	return []identity.UserRow{
		{TenantID: "t1", Subject: "admin@example.com", Role: identity.RoleAdmin},
		{TenantID: "t2", Subject: "admin@example.com", Role: identity.RoleViewer},
	}, nil
}

// navConsole builds a console with every optional section wired, plus a
// registry and an eval run, so all seven signed-in pages render for one
// principal.
func navConsole(t *testing.T) http.Handler {
	t.Helper()
	admin := multiTenantAdmin{&fakeAdmin2{}}

	es := eval.NewMemStore()
	ctx := context.Background()
	_ = es.CreateRun(ctx, eval.Run{RunID: "run-nav", Tenant: "t1", SetName: "s", AgentID: "a", Status: eval.StatusPending})
	now := time.Now().UTC()
	_, _ = es.ClaimRun(ctx, "run-nav", "seed", now, now.Add(time.Minute))
	_, _ = es.FinishRunClaimed(ctx, "run-nav", "seed", eval.StatusCompleted, 0, 0, 0, 0, "")

	reg := controlplane.NewRegistry(&config.Config{Agents: []config.AgentConfig{
		{ID: "a", Name: "A", Model: "m", ListenAddr: "127.0.0.1:9101", Tenant: "t1"},
	}}, "./agentd", "dsn")

	return Handler(reg, nil, OIDCConfig{}, &Onboarding{
		Upstreams: &fakeUpstreamStore2{}, Mutator: &fakeMut2{},
		Admin: admin, Secrets: &fakeSec2{}, EvalStore: es,
	})
}

func TestNav_IdenticalOnEverySignedInPage(t *testing.T) {
	h := navConsole(t)
	pages := []string{
		"/ui",
		"/ui/observability",
		"/ui/onboarding",
		"/ui/select-tenant",
		"/ui/agents/a",
		"/ui/agents/a/sessions/sess-1",
		"/ui/observability/eval-runs/run-nav",
	}
	for _, p := range pages {
		got := navOf(t, h, adminReqAs("GET", p, "admin@example.com", nil))
		if len(got) != len(wantNavLinks) {
			t.Errorf("%s: nav has %d links %v, want %d %v", p, len(got), got, len(wantNavLinks), wantNavLinks)
			continue
		}
		for i := range got {
			if got[i] != wantNavLinks[i] {
				t.Errorf("%s: nav[%d] = %q, want %q (full: %v)", p, i, got[i], wantNavLinks[i], got)
			}
		}
	}
}

// A page must mark itself current, and must mark only itself. A child page (an
// agent, a session, an eval run) is not any nav destination, so it marks none:
// aria-current="page" on a link that leads somewhere else is a false statement
// to a screen-reader user about where they are.
func TestNav_AriaCurrentMarksTheRightLink(t *testing.T) {
	h := navConsole(t)
	cases := []struct{ path, wantCurrent string }{
		{"/ui", "/ui"},
		{"/ui/observability", "/ui/observability"},
		{"/ui/onboarding", "/ui/onboarding"},
		{"/ui/select-tenant", "/ui/select-tenant"},
		{"/ui/agents/a", ""},
		{"/ui/agents/a/sessions/sess-1", ""},
		{"/ui/observability/eval-runs/run-nav", ""},
	}
	currentRe := regexp.MustCompile(`<a href="([^"]*)" aria-current="page"`)
	for _, c := range cases {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, adminReqAs("GET", c.path, "admin@example.com", nil))
		header := navHeaderRe.FindString(w.Body.String())
		ms := currentRe.FindAllStringSubmatch(header, -1)
		if c.wantCurrent == "" {
			if len(ms) != 0 {
				t.Errorf("%s: child page marks %q as current; it is not a nav destination", c.path, ms[0][1])
			}
			continue
		}
		if len(ms) != 1 {
			t.Errorf("%s: want exactly 1 aria-current link, got %d", c.path, len(ms))
			continue
		}
		if ms[0][1] != c.wantCurrent {
			t.Errorf("%s: aria-current on %q, want %q", c.path, ms[0][1], c.wantCurrent)
		}
	}
}

// The partial is only load-bearing if the pages actually include it. Asserting
// on the template source catches a reintroduced hand-rolled header at the point
// it is written, with a message that says what to do instead.
func TestNav_TemplatesUseTheSharedPartial(t *testing.T) {
	ents, err := assets.ReadDir("templates")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		name := e.Name()
		// landing.html is the signed-out front door: its bar is a brand plus a
		// single sign-in button, deliberately not the operator menu.
		if name == "_topbar.html" || name == "landing.html" {
			continue
		}
		b, err := assets.ReadFile("templates/" + name)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		if !strings.Contains(src, `<header class="topbar">`) && !strings.Contains(src, `{{template "topbar"`) {
			continue // not a full page (a fragment or a partial)
		}
		if strings.Contains(src, `<header class="topbar">`) {
			t.Errorf("templates/%s hand-rolls <header class=\"topbar\">; use {{template \"topbar\" \"<section>\"}} "+
				"so the menu cannot drift from the other pages", name)
		}
	}
}
