package console

import (
	"net/http"

	"github.com/sausheong/runtime/internal/identity"
)

// setTenantCookie records the console's selected tenant. HttpOnly + SameSite=Lax,
// Path=/ so the authenticator sees it on every request. It is only a hint: the
// authenticator honors it solely when it names one of the subject's memberships.
func setTenantCookie(w http.ResponseWriter, tenant string) {
	http.SetCookie(w, &http.Cookie{
		Name: identity.TenantCookieName, Value: tenant,
		Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
}

// clearTenantCookie expires the selected-tenant cookie (logout → next login re-prompts).
func clearTenantCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: identity.TenantCookieName, Value: "",
		Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
}
