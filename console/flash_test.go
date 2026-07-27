package console

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// readFlashCookie returns the decoded rt_flash value a handler set, so a test
// can assert on the severity prefix rather than on base64.
func readFlashCookie(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name != "rt_flash" || c.Value == "" {
			continue
		}
		raw, err := base64.RawURLEncoding.DecodeString(c.Value)
		if err != nil {
			t.Fatalf("rt_flash not decodable: %v", err)
		}
		return string(raw)
	}
	return ""
}

// assertErrorFlash checks that a handler bounced the operator back to the
// onboarding page with an error-severity flash, rather than dead-ending on an
// unstyled text/plain page that discards their form input.
func assertErrorFlash(t *testing.T, rec *httptest.ResponseRecorder, wantSubstr string) {
	t.Helper()
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("want 303 redirect back to the page, got %d", rec.Code)
	}
	got := readFlashCookie(t, rec)
	if !strings.HasPrefix(got, flashErrPrefix) {
		t.Errorf("want an error-severity flash (%q prefix), got %q", flashErrPrefix, got)
	}
	if !strings.Contains(got, wantSubstr) {
		t.Errorf("flash should explain the problem (%q); got %q", wantSubstr, got)
	}
}

// flashReq builds an authenticated onboarding GET carrying a preset flash
// cookie, so the render path can be tested without going through a POST.
func flashReq(raw string) *http.Request {
	r := adminReq("GET", "/ui/onboarding", nil)
	r.AddCookie(&http.Cookie{
		Name:  "rt_flash",
		Value: base64.RawURLEncoding.EncodeToString([]byte(raw)),
	})
	return r
}

// A rejected Cedar policy must not be announced in the success style. Before
// the severity split every flashRedirect shared one presentation, and .flash is
// styled with success tokens, so "Policy rejected: ..." rendered green.
func TestFlash_RejectedPolicyIsMarkedAsError(t *testing.T) {
	h, _ := consoleWithPolicies(t)
	form := url.Values{
		"csrf_token": {issuedCSRF(t, h)},
		"name":       {"bad"},
		"cedar_text": {"this is not cedar"},
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminReq("POST", "/ui/onboarding/policies", form))

	got := readFlashCookie(t, rec)
	if got == "" {
		t.Fatalf("expected a flash cookie, got none (status %d)", rec.Code)
	}
	if !strings.HasPrefix(got, flashErrPrefix) {
		t.Errorf("a rejected policy must carry the error prefix %q; got %q", flashErrPrefix, got)
	}
	if !strings.Contains(got, "rejected") {
		t.Errorf("flash should still explain what happened; got %q", got)
	}
}

// A successful action keeps the success presentation.
func TestFlash_SuccessIsMarkedAsOK(t *testing.T) {
	h, _, _ := newTestConsoleWithAdmin()
	form := url.Values{
		"csrf_token": {issuedCSRF(t, h)},
		"subject":    {"someone@acme.com"},
		"role":       {"viewer"},
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminReq("POST", "/ui/onboarding/users", form))

	got := readFlashCookie(t, rec)
	if got == "" {
		t.Fatalf("expected a flash cookie, got none (status %d)", rec.Code)
	}
	if !strings.HasPrefix(got, flashOKPrefix) {
		t.Errorf("a successful action should carry %q; got %q", flashOKPrefix, got)
	}
}

// The rendered page must reach .flash-error for a failure. The class existed in
// the stylesheet but no onboarding code path could produce it.
func TestFlash_ErrorRendersErrorClass(t *testing.T) {
	h, _, _ := newTestConsoleWithAdmin()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, flashReq(flashErrPrefix+"Policy rejected: unexpected token"))
	body := rec.Body.String()

	if !strings.Contains(body, "flash-error") {
		t.Error("a failure flash must render the flash-error class")
	}
	if !strings.Contains(body, "Policy rejected: unexpected token") {
		t.Error("the message text should survive to the page")
	}
	if strings.Contains(body, flashErrPrefix) {
		t.Errorf("the severity prefix is internal and must not be shown; body contained %q", flashErrPrefix)
	}
}

// A success flash must NOT pick up the error class.
func TestFlash_SuccessDoesNotRenderErrorClass(t *testing.T) {
	h, _, _ := newTestConsoleWithAdmin()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, flashReq(flashOKPrefix+"User saved."))
	body := rec.Body.String()

	if strings.Contains(body, "flash-error") {
		t.Error("a success flash must not render the error class")
	}
	if !strings.Contains(body, "User saved.") {
		t.Error("the message text should survive to the page")
	}
}

// Flashes written before this change (no prefix) must still display rather than
// showing a raw marker or vanishing.
func TestFlash_LegacyUnprefixedStillRenders(t *testing.T) {
	h, _, _ := newTestConsoleWithAdmin()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, flashReq("Upstream orders registered."))
	body := rec.Body.String()

	if !strings.Contains(body, "Upstream orders registered.") {
		t.Error("an unprefixed legacy flash should still render its message")
	}
	if strings.Contains(body, "flash-error") {
		t.Error("an unprefixed legacy flash should default to the success presentation")
	}
}

// The one-time key reveal is a third presentation and must survive the split.
func TestFlash_KeyRevealStillWorks(t *testing.T) {
	h, _, _ := newTestConsoleWithAdmin()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, flashReq("key:rt_sk_secretvalue"))
	body := rec.Body.String()

	if !strings.Contains(body, "rt_sk_secretvalue") {
		t.Error("the minted key should render in the key-reveal block")
	}
	if !strings.Contains(body, "key-reveal") {
		t.Error("a minted key must use the copy-it-now treatment, not a generic flash")
	}
}
