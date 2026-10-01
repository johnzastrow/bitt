package web

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// RECON-00: the top bar holds the brand and the account menu, nothing else;
// every navigation item lives behind the avatar.

// topbar returns the <header class="topbar"> element of a page.
func topbar(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, `<header class="topbar">`)
	if start < 0 {
		t.Fatalf("no top bar on the page: %s", truncate(body))
	}
	end := strings.Index(body[start:], "</header>")
	if end < 0 {
		t.Fatal("top bar is not closed")
	}
	return body[start : start+end]
}

// menu returns the account menu's item list.
func menu(t *testing.T, body string) string {
	t.Helper()
	bar := topbar(t, body)
	start := strings.Index(bar, `<nav class="acctmenu-list"`)
	if start < 0 {
		t.Fatalf("no account menu in the top bar: %s", bar)
	}
	end := strings.Index(bar[start:], "</nav>")
	return bar[start : start+end]
}

var menuItemRe = regexp.MustCompile(`class="menuitem"[^>]*>([^<]+)<`)

func menuItems(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	for _, m := range menuItemRe.FindAllStringSubmatch(menu(t, body), -1) {
		out = append(out, m[1])
	}
	return out
}

func TestAccountMenuItemsByRole(t *testing.T) {
	h := newHarness(t)
	h.completeSetup() // Jane, the administrator

	_, body := h.get("/")
	if got, want := strings.Join(menuItems(t, body), ","), "Profile,People,Notifications,Log out"; got != want {
		t.Errorf("administrator menu = %q, want %q", got, want)
	}

	h.addUser("sam@example.com", "Sam", false)
	h.loginAs("sam@example.com", "a-long-enough-password")
	_, body = h.get("/")
	if got, want := strings.Join(menuItems(t, body), ","), "Profile,Log out"; got != want {
		t.Errorf("non-administrator menu = %q, want %q", got, want)
	}
	// The administrator links must not leak anywhere on the page, not merely
	// be missing from the menu.
	for _, href := range []string{`href="/admin/users"`, `href="/admin/notifications"`} {
		if strings.Contains(body, href) {
			t.Errorf("non-administrator page links to %s", href)
		}
	}
}

// The bar itself carries only the brand and the menu: no bare links or forms
// outside the disclosure, which is what kept it narrow on a phone.
func TestTopBarIsBrandAndMenuOnly(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	_, body := h.get("/")
	bar := topbar(t, body)

	outside := bar
	if i := strings.Index(bar, `<details class="acctmenu"`); i >= 0 {
		j := strings.Index(bar, "</details>")
		outside = bar[:i] + bar[j+len("</details>"):]
	} else {
		t.Fatal("no account menu disclosure")
	}
	if n := strings.Count(outside, "<a "); n != 1 {
		t.Errorf("%d links outside the menu, want only the brand: %s", n, outside)
	}
	for _, tag := range []string{"<form", "<button", "<nav"} {
		if strings.Contains(outside, tag) {
			t.Errorf("%s outside the menu: %s", tag, outside)
		}
	}
	// The avatar no longer links straight to the profile; it is the summary.
	if !regexp.MustCompile(`<summary class="who"[^>]*>`).MatchString(bar) {
		t.Error("avatar is not the menu's summary")
	}
	if !strings.Contains(bar, "Jane Provider") {
		t.Error("display name missing from the menu button")
	}
}

// The menu marks the page the person is on, including pages beneath it, and
// marks nothing elsewhere.
func TestAccountMenuMarksCurrentPage(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	cases := []struct{ path, want string }{
		{"/profile", "Profile"},
		{"/admin/users", "People"},
		{"/admin/notifications", "Notifications"},
		{"/", ""},
	}
	current := regexp.MustCompile(`aria-current="page"[^>]*>([^<]+)<`)
	for _, c := range cases {
		resp, body := h.get(c.path)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d", c.path, resp.StatusCode)
		}
		ms := current.FindAllStringSubmatch(menu(t, body), -1)
		switch {
		case c.want == "" && len(ms) != 0:
			t.Errorf("%s: %d items marked current, want none", c.path, len(ms))
		case c.want != "" && (len(ms) != 1 || ms[0][1] != c.want):
			t.Errorf("%s: marked %v, want only %q", c.path, ms, c.want)
		}
	}
}

// Log out moved into the menu but is still a CSRF-protected POST: the menu's
// own token signs out, a missing or forged one does not, and GET does nothing.
func TestMenuLogoutRequiresCSRF(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	_, body := h.get("/")
	m := regexp.MustCompile(`<form method="post" action="/logout">\s*<input type="hidden" name="csrf_token" value="([^"]+)"`).
		FindStringSubmatch(menu(t, body))
	if m == nil {
		t.Fatalf("no logout form with a CSRF token in the menu: %s", menu(t, body))
	}

	h.get("/logout")
	if _, b := h.get("/"); !strings.Contains(b, "Your tabs") {
		t.Fatal("GET /logout signed the account out")
	}
	// A bad token is sent home (the handler redirects rather than 403s), with
	// the session intact.
	for _, tok := range []string{"", "forged-token"} {
		h.post("/logout", url.Values{"csrf_token": {tok}})
		if _, b := h.get("/"); !strings.Contains(b, "Your tabs") {
			t.Fatalf("logout with token %q signed the account out", tok)
		}
	}

	if _, b := h.post("/logout", url.Values{"csrf_token": {m[1]}}); !strings.Contains(b, "Sign in") {
		t.Fatalf("logout from the menu did not sign out: %s", truncate(b))
	}
}

// A display name is rendered as text in the menu button, never as markup.
func TestAccountMenuEscapesDisplayName(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	h.addUser("x@example.com", `<img src=x onerror=alert(1)>`, false)
	h.loginAs("x@example.com", "a-long-enough-password")

	_, body := h.get("/")
	bar := topbar(t, body)
	if strings.Contains(bar, "<img src=x") {
		t.Fatalf("display name rendered as markup: %s", bar)
	}
	if !strings.Contains(bar, "&lt;img src=x onerror=alert(1)&gt;") {
		t.Errorf("display name not shown escaped: %s", bar)
	}
}

// Signed-out pages have no account menu at all.
func TestNoAccountMenuSignedOut(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	h.post("/logout", url.Values{"csrf_token": {h.csrfToken("/")}})
	_, body := h.get("/login")
	if strings.Contains(body, "acctmenu") {
		t.Error("account menu rendered on the sign-in page")
	}
}

// htmx must not inject its indicator <style>: the CSP blocks it and logged an
// error on every page.
func TestHtmxIndicatorStylesDisabled(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{"/setup"} {
		_, body := h.get(path)
		if !strings.Contains(body, `<meta name="htmx-config" content='{"includeIndicatorStyles":false}'>`) {
			t.Errorf("%s lacks the htmx-config meta", path)
		}
	}
	h.completeSetup()
	if _, body := h.get("/"); !strings.Contains(body, `"includeIndicatorStyles":false`) {
		t.Error("signed-in pages lack the htmx-config meta")
	}
}

// A fieldset must be able to shrink to a phone's width (min-content default).
func TestFieldsetCanShrink(t *testing.T) {
	css, err := staticFS.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)\nfieldset \{[^}]*\}`).Find(css)
	if m == nil || !strings.Contains(string(m), "min-width: 0;") {
		t.Errorf("fieldset rule lacks min-width: 0: %s", m)
	}
}
