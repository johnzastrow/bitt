package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
)

// CSRF protection via the double-submit cookie pattern.
//
// Tier 1 rule 12: cookie-session state changes carry CSRF defense. SameSite=Lax
// already blocks the cross-site POST case in current browsers, but it is a
// single control, and a same-site subdomain or an older client defeats it. The
// token is the second layer.

const (
	// CSRFCookieName holds the token readable by the form renderer.
	CSRFCookieName = "bitt_csrf"
	// CSRFFieldName is the hidden form field carrying the token back.
	CSRFFieldName = "csrf_token"
)

// EnsureCSRFToken returns the request's CSRF token, minting and setting one if
// the request does not already carry it.
func EnsureCSRFToken(w http.ResponseWriter, r *http.Request, secure bool) string {
	if c, err := r.Cookie(CSRFCookieName); err == nil && len(c.Value) >= 20 {
		return c.Value
	}

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		// Failing closed here would make every page unreachable. Returning an
		// empty token instead causes CheckCSRF to reject the subsequent POST,
		// which is the safe direction.
		return ""
	}
	token := base64.RawURLEncoding.EncodeToString(buf)

	http.SetCookie(w, &http.Cookie{
		Name:  CSRFCookieName,
		Value: token,
		Path:  "/",
		// Readable by the server on the next request; not HttpOnly is
		// unnecessary here since only the server reads it, so keep it HttpOnly.
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
	return token
}

// CheckCSRF compares the submitted form token against the cookie in constant
// time. An absent cookie or field fails.
func CheckCSRF(r *http.Request) bool {
	cookie, err := r.Cookie(CSRFCookieName)
	if err != nil || cookie.Value == "" {
		return false
	}
	submitted := r.PostFormValue(CSRFFieldName)
	if submitted == "" {
		submitted = r.Header.Get("X-CSRF-Token")
	}
	if submitted == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(submitted)) == 1
}
