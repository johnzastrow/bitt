package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// UI-05. The PWA is shell-only by design: a manifest, icons, and a service
// worker that caches the static frame but no data. These tests pin the
// mechanics a real phone install depends on -- the manifest is served and
// linked, the icons resolve, the worker is served from the root with a scope
// header, and the registration script is wired into the page. The installed
// experience itself cannot be tested here (no home screen), only that every
// part it relies on is present and correct.

func TestManifestServed(t *testing.T) {
	h := newHarness(t)

	resp, body := h.get("/static/manifest.webmanifest")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("manifest status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/manifest+json") {
		t.Errorf("manifest Content-Type = %q, want application/manifest+json", ct)
	}

	var m struct {
		Name       string `json:"name"`
		StartURL   string `json:"start_url"`
		Display    string `json:"display"`
		ThemeColor string `json:"theme_color"`
		Icons      []struct {
			Src     string `json:"src"`
			Sizes   string `json:"sizes"`
			Type    string `json:"type"`
			Purpose string `json:"purpose"`
		} `json:"icons"`
	}
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("manifest is not valid JSON: %v", err)
	}
	if m.Name == "" || m.StartURL != "/" || m.Display != "standalone" {
		t.Errorf("manifest core fields wrong: name=%q start_url=%q display=%q", m.Name, m.StartURL, m.Display)
	}
	// A maskable icon is what keeps the home-screen icon from being cropped to a
	// square on Android; its absence is a silent visual regression.
	var haveMaskable, have192, have512 bool
	for _, ic := range m.Icons {
		if strings.Contains(ic.Purpose, "maskable") {
			haveMaskable = true
		}
		if ic.Sizes == "192x192" {
			have192 = true
		}
		if ic.Sizes == "512x512" {
			have512 = true
		}
	}
	if !have192 || !have512 || !haveMaskable {
		t.Errorf("manifest icons incomplete: 192=%v 512=%v maskable=%v", have192, have512, haveMaskable)
	}
}

func TestManifestIconsResolve(t *testing.T) {
	h := newHarness(t)
	for _, name := range []string{
		"/static/icon-192.png",
		"/static/icon-512.png",
		"/static/icon-maskable-512.png",
		"/static/apple-touch-icon.png",
	} {
		resp, _ := h.get(name)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s status = %d, want 200", name, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "image/png") {
			t.Errorf("%s Content-Type = %q, want image/png", name, ct)
		}
	}
}

func TestServiceWorkerServedFromRoot(t *testing.T) {
	h := newHarness(t)

	// From the root, so the worker's default scope is the whole origin. A worker
	// under /static/ would control only /static/ and never intercept a
	// navigation.
	resp, body := h.get("/sw.js")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/sw.js status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/javascript") {
		t.Errorf("/sw.js Content-Type = %q, want application/javascript", ct)
	}
	if got := resp.Header.Get("Service-Worker-Allowed"); got != "/" {
		t.Errorf("/sw.js Service-Worker-Allowed = %q, want /", got)
	}
	// The worker must not be cached, or an update check could miss new bytes.
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
		t.Errorf("/sw.js Cache-Control = %q, want no-cache", cc)
	}
	// It reads its cache version from its own URL query and must never cache a
	// navigation -- the two lines that keep a balance from being served stale.
	if !strings.Contains(body, `searchParams.get("v")`) {
		t.Error("/sw.js does not derive its cache version from the ?v= query")
	}
	if !strings.Contains(body, `req.mode === "navigate"`) {
		t.Error("/sw.js does not special-case navigations (network-first)")
	}
}

func TestServiceWorkerNeverCachesTabs(t *testing.T) {
	// The load-bearing constraint of a shell-only PWA: no path under /tabs is
	// ever cached. If this string appears, someone has started caching data and
	// a stale balance is now possible (LEDGER-03).
	h := newHarness(t)
	_, body := h.get("/sw.js")
	if strings.Contains(body, "/tabs") {
		t.Error("/sw.js references /tabs -- a shell-only worker must not cache data")
	}
}

func TestLayoutLinksPWA(t *testing.T) {
	// The page must link the manifest, advertise a theme colour, expose the
	// asset digest, and load the registration script. Any page will do; the
	// login screen renders without a session.
	h := newHarness(t)
	resp, body := h.get("/login")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/login status = %d, want 200", resp.StatusCode)
	}
	for _, want := range []string{
		`rel="manifest"`,
		`name="theme-color"`,
		`name="bittabby-asset-version"`,
		`sw-register.js`,
		`rel="apple-touch-icon"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("layout head missing %q", want)
		}
	}
}

func TestServiceWorkerVersionTracksAssets(t *testing.T) {
	// The registration meta must carry the same digest every static URL carries,
	// so a deploy that changes an asset re-registers the worker and rolls its
	// precache. A mismatch here is exactly the stale-asset bug 0.5.2 fixed.
	h := newHarness(t)
	_, body := h.get("/login")
	want := `content="` + AssetVersion() + `"`
	if !strings.Contains(body, want) {
		t.Errorf("layout does not expose the current asset digest %q", AssetVersion())
	}
}
