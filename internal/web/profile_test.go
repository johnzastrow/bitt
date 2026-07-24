package web

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// Matches the account completeSetup creates.
const (
	testPassword = "correct-horse-battery"
	testEmail    = "jane@example.com"
)

// uploadAvatar posts a multipart form with the given bytes as the picture.
func (h *harness) uploadAvatar(t *testing.T, body []byte, filename string) (*http.Response, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("csrf_token", h.csrfToken("/profile"))
	part, err := mw.CreateFormFile("avatar", filename)
	if err != nil {
		t.Fatalf("create part: %v", err)
	}
	if _, err := part.Write(body); err != nil {
		t.Fatalf("write part: %v", err)
	}
	_ = mw.Close()

	req, err := http.NewRequest(http.MethodPost, h.server.URL+"/profile/avatar", &buf)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp, string(out)
}

func samplePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 90, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return buf.Bytes()
}

// The header links to the profile, so the settings are reachable by clicking
// your own name, which is where people look for them.
func TestHeaderLinksToProfile(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	_, body := h.get("/")
	if !strings.Contains(body, `href="/profile"`) {
		t.Errorf("the header does not link to the profile: %s", truncate(body))
	}

	_, body = h.get("/profile")
	for _, want := range []string{"Your profile", "Picture", "Your details", "Password"} {
		if !strings.Contains(body, want) {
			t.Errorf("the profile page has no %q section: %s", want, truncate(body))
		}
	}
	// Notification preferences are deferred to Phase 5; offering switches that
	// deliver nothing would be a promise the app cannot keep.
	if strings.Contains(body, "Notification") {
		t.Error("the profile offers notification settings, which nothing delivers yet")
	}
}

func TestProfileUpdatesDetails(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	_, body := h.post("/profile/details", url.Values{
		"csrf_token":       {h.csrfToken("/profile")},
		"display_name":     {"Jane Renamed"},
		"email":            {"renamed@example.com"},
		"current_password": {testPassword},
	})
	if !strings.Contains(body, "details are saved") {
		t.Fatalf("details were not saved: %s", truncate(body))
	}

	u, err := h.db.GetUser(t.Context(), 1)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if u.DisplayName != "Jane Renamed" || u.Email != "renamed@example.com" {
		t.Errorf("stored %q / %q", u.DisplayName, u.Email)
	}
	// The folded address moves with it, or the new address cannot sign in.
	if _, err := h.db.GetUserByEmail(t.Context(), "RENAMED@EXAMPLE.COM"); err != nil {
		t.Errorf("the new address does not resolve case-insensitively: %v", err)
	}
}

// The email is the login identity, so changing it needs the password. Without
// this, an unattended session is a full account takeover.
func TestProfileDetailsRequireTheCurrentPassword(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	_, body := h.post("/profile/details", url.Values{
		"csrf_token":       {h.csrfToken("/profile")},
		"display_name":     {"Attacker"},
		"email":            {"attacker@example.com"},
		"current_password": {"not-the-password"},
	})
	if !strings.Contains(body, "password is not correct") {
		t.Errorf("a wrong password was accepted: %s", truncate(body))
	}

	u, _ := h.db.GetUser(t.Context(), 1)
	if u.Email == "attacker@example.com" {
		t.Error("the email changed despite a wrong password")
	}
}

func TestProfileRejectsADuplicateEmail(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	h.addUser("taken@example.com", "Someone", false)

	_, body := h.post("/profile/details", url.Values{
		"csrf_token":       {h.csrfToken("/profile")},
		"display_name":     {"Admin"},
		"email":            {"TAKEN@example.com"},
		"current_password": {testPassword},
	})
	if !strings.Contains(body, "already uses that email") {
		t.Errorf("a duplicate address was accepted: %s", truncate(body))
	}
}

// A password change ends every other session but keeps this one.
func TestPasswordChangeEndsOtherSessions(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	// A second signed-in device for the same account.
	other := h.newClient()
	other.loginAs(testEmail, testPassword)
	if _, body := other.get("/"); strings.Contains(body, "Sign in") {
		t.Fatal("the second client did not sign in")
	}

	_, body := h.post("/profile/password", url.Values{
		"csrf_token":       {h.csrfToken("/profile")},
		"current_password": {testPassword},
		"new_password":     {"an-entirely-different-password"},
		"confirm_password": {"an-entirely-different-password"},
	})
	if !strings.Contains(body, "password is changed") {
		t.Fatalf("the password was not changed: %s", truncate(body))
	}
	if !strings.Contains(body, "signed out") {
		t.Errorf("the confirmation does not mention the other device: %s", truncate(body))
	}

	// The other device is out.
	if _, body := other.get("/profile"); !strings.Contains(body, "Sign in") {
		t.Error("the other session survived the password change")
	}
	// This one is still in.
	if _, body := h.get("/profile"); strings.Contains(body, "Sign in") {
		t.Error("the session that made the change was signed out")
	}
	// And the new password works.
	fresh := h.newClient()
	fresh.loginAs(testEmail, "an-entirely-different-password")
	if _, body := fresh.get("/"); strings.Contains(body, "Sign in") {
		t.Error("the new password does not work")
	}
}

func TestPasswordChangeValidation(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	cases := []struct {
		name string
		form url.Values
		want string
	}{
		{"wrong current", url.Values{
			"current_password": {"wrong"}, "new_password": {"a-long-enough-password"},
			"confirm_password": {"a-long-enough-password"}}, "current password is not correct"},
		{"mismatch", url.Values{
			"current_password": {testPassword}, "new_password": {"a-long-enough-password"},
			"confirm_password": {"a-different-long-password"}}, "do not match"},
		{"too short", url.Values{
			"current_password": {testPassword}, "new_password": {"short"},
			"confirm_password": {"short"}}, "12"},
		{"unchanged", url.Values{
			"current_password": {testPassword}, "new_password": {testPassword},
			"confirm_password": {testPassword}}, "same as the current"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			form := url.Values{"csrf_token": {h.csrfToken("/profile")}}
			for k, v := range tc.form {
				form[k] = v
			}
			_, body := h.post("/profile/password", form)
			if !strings.Contains(body, tc.want) {
				t.Errorf("expected %q: %s", tc.want, truncate(body))
			}
		})
	}
}

func TestAvatarUploadAndServe(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	_, body := h.uploadAvatar(t, samplePNG(t, 400, 300), "me.png")
	if !strings.Contains(body, "picture is updated") {
		t.Fatalf("upload failed: %s", truncate(body))
	}

	resp, _ := h.get("/users/1/avatar")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("serving the avatar returned %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "image/png" {
		t.Errorf("Content-Type = %q, want image/png -- the stored bytes are always PNG", got)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" || etag == `""` {
		t.Errorf("no usable ETag: %q", etag)
	}
	// A picture is personal, so it must not land in a shared cache.
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "private") {
		t.Errorf("Cache-Control = %q, want private", cc)
	}

	// The header now shows the image rather than initials.
	_, body = h.get("/")
	if !strings.Contains(body, "/users/1/avatar") {
		t.Errorf("the header does not show the avatar: %s", truncate(body))
	}

	// Removing it goes back to the fallback.
	_, body = h.post("/profile/avatar/remove", url.Values{"csrf_token": {h.csrfToken("/profile")}})
	if !strings.Contains(body, "picture is removed") {
		t.Fatalf("removal failed: %s", truncate(body))
	}
	resp, _ = h.get("/users/1/avatar")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("a removed avatar still serves: %d", resp.StatusCode)
	}
}

// A non-image must be refused, whatever it is named.
func TestAvatarRejectsNonImages(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	_, body := h.uploadAvatar(t, []byte("<svg onload=alert(1)></svg>"), "me.png")
	if !strings.Contains(body, "not a PNG, JPEG, or GIF") {
		t.Errorf("a non-image was accepted: %s", truncate(body))
	}
	resp, _ := h.get("/users/1/avatar")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("something was stored anyway: %d", resp.StatusCode)
	}
}

// The filename is never read, so a traversal attempt is simply irrelevant --
// this pins that it stays irrelevant.
func TestAvatarIgnoresTheFilename(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	_, body := h.uploadAvatar(t, samplePNG(t, 64, 64), "../../../../etc/passwd")
	if !strings.Contains(body, "picture is updated") {
		t.Fatalf("upload failed: %s", truncate(body))
	}
	if resp, _ := h.get("/users/1/avatar"); resp.StatusCode != http.StatusOK {
		t.Errorf("the image did not store: %d", resp.StatusCode)
	}
}

// A signed-out visitor gets nothing, and a missing account looks the same as
// an account with no picture, so the route cannot enumerate accounts.
func TestAvatarRequiresAuthAndDoesNotEnumerate(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	// requireAuth redirects to the sign-in page rather than answering 401, and
	// the client follows it, so the status is 200. What matters is that the
	// body is the login page and not an image.
	anon := h.newClient()
	resp, body := anon.get("/users/1/avatar")
	if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "image/") {
		t.Errorf("a signed-out visitor was served an image (%s)", ct)
	}
	if !strings.Contains(body, "Sign in") {
		t.Errorf("a signed-out visitor was not sent to sign in: %s", truncate(body))
	}

	noPicture, _ := h.get("/users/1/avatar")     // exists, no avatar
	noAccount, _ := h.get("/users/99999/avatar") // does not exist
	if noPicture.StatusCode != noAccount.StatusCode {
		t.Errorf("an account with no picture (%d) is distinguishable from one that "+
			"does not exist (%d)", noPicture.StatusCode, noAccount.StatusCode)
	}
}

func TestAvatarUploadIsRateLimited(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	img := samplePNG(t, 32, 32)
	var limited bool
	for range avatarLimit + 2 {
		_, body := h.uploadAvatar(t, img, "me.png")
		if strings.Contains(body, "Too many uploads") {
			limited = true
			break
		}
	}
	if !limited {
		t.Errorf("no limit applied after %d uploads -- image decoding is the one "+
			"expensive thing an authenticated user can ask for in a loop", avatarLimit+2)
	}
}
