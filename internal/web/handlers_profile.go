package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/johnzastrow/bitt/internal/auth"
	"github.com/johnzastrow/bitt/internal/avatar"
	"github.com/johnzastrow/bitt/internal/notify"
	"github.com/johnzastrow/bitt/internal/store"
	"github.com/johnzastrow/bitt/internal/web/views"
)

// displayNameMax bounds the stored name. Long enough for any real name,
// short enough that the header cannot be pushed apart by one.
const displayNameMax = 80

// ---------------------------------------------------------------------------
// Upload rate limiting
// ---------------------------------------------------------------------------

// avatarLimit is how many uploads one account may make per window.
//
// This is the only route in the application that does meaningful CPU work on
// request: decoding an image is orders of magnitude more expensive than any
// other handler, and an authenticated user can ask for it in a loop. The rest
// of the app needs no limiter because the rest of the app does a few indexed
// queries. Rather than introduce a general framework for one endpoint, this is
// a small fixed-window counter over the one route that wants it.
const (
	avatarLimit  = 10
	avatarWindow = time.Minute
)

type rateEntry struct {
	count int
	reset time.Time
}

// allowAvatarUpload reports whether the user may upload again now.
//
// Fixed window rather than a token bucket: the failure mode of a fixed window
// is allowing up to twice the rate across a boundary, which for "ten avatar
// uploads a minute" is of no consequence, and it costs one map entry per user
// instead of a background refill.
func (s *Server) allowAvatarUpload(userID int64) bool {
	now := time.Now()
	v, _ := s.avatarRate.LoadOrStore(userID, &rateEntry{})
	e := v.(*rateEntry)

	s.avatarRateMu.Lock()
	defer s.avatarRateMu.Unlock()
	if now.After(e.reset) {
		e.count, e.reset = 0, now.Add(avatarWindow)
	}
	if e.count >= avatarLimit {
		return false
	}
	e.count++
	return true
}

// ---------------------------------------------------------------------------
// Profile
// ---------------------------------------------------------------------------

// getProfile renders the account's own settings.
func (s *Server) getProfile(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	nc := s.notifyConfig(r.Context())
	s.render(w, r, http.StatusOK, views.Profile(s.page(w, r, "Your profile"), views.ProfileData{
		User: *user,
		// The effective configuration, so a channel an administrator switched on
		// through the interface shows as available here without a restart.
		EmailEnabled: nc.EmailEnabled(),
		NtfyEnabled:  nc.NtfyEnabled(),
	}))
}

// postProfileDetails changes the display name and email.
//
// The email is the login identity, so this asks for the current password. That
// is not ceremony: an unattended session that could silently move the address
// to one the attacker controls is a full account takeover, since password
// recovery would follow the new address.
func (s *Server) postProfileDetails(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	if !s.profileFormOK(w, r) {
		return
	}

	name := strings.TrimSpace(r.PostFormValue("display_name"))
	if name == "" {
		redirectWith(w, r, "/profile", "err", "A name is required.")
		return
	}
	if len(name) > displayNameMax {
		name = name[:displayNameMax]
	}

	email := strings.TrimSpace(r.PostFormValue("email"))
	// looksLikeEmail, the same check setup and admin creation use, rejects
	// control characters including CR/LF. The email is the stored login
	// identity and will feed a mail sender's headers in Phase 5, so a weaker
	// "contains an @" check here would let a CRLF-laden address through and set
	// up header injection downstream.
	if !looksLikeEmail(email) {
		redirectWith(w, r, "/profile", "err", "That is not a valid email address.")
		return
	}

	// Re-authenticate before touching the login identity.
	if err := auth.VerifyPassword(r.PostFormValue("current_password"), user.PasswordHash); err != nil {
		s.log.Warn("profile change rejected: wrong password", "user_id", user.ID)
		redirectWith(w, r, "/profile", "err", "That password is not correct.")
		return
	}

	updated, err := s.store.UpdateProfile(r.Context(), user.ID, name, email)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			redirectWith(w, r, "/profile", "err", "Another account already uses that email address.")
			return
		}
		s.serverError(w, r, err)
		return
	}

	s.log.Info("profile updated", "user_id", user.ID,
		"email_changed", !strings.EqualFold(updated.Email, user.Email))
	redirectWith(w, r, "/profile", "ok", "Your details are saved.")
}

// postProfilePassword changes the password and ends every other session.
func (s *Server) postProfilePassword(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	if !s.profileFormOK(w, r) {
		return
	}

	if err := auth.VerifyPassword(r.PostFormValue("current_password"), user.PasswordHash); err != nil {
		s.log.Warn("password change rejected: wrong current password", "user_id", user.ID)
		redirectWith(w, r, "/profile", "err", "Your current password is not correct.")
		return
	}

	next := r.PostFormValue("new_password")
	if next == r.PostFormValue("current_password") {
		redirectWith(w, r, "/profile", "err", "The new password is the same as the current one.")
		return
	}
	if err := auth.ValidatePassword(next); err != nil {
		redirectWith(w, r, "/profile", "err", err.Error())
		return
	}
	if next != r.PostFormValue("confirm_password") {
		redirectWith(w, r, "/profile", "err", "The two new passwords do not match.")
		return
	}

	hash, err := auth.HashPassword(next)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := s.store.UpdatePasswordHash(r.Context(), user.ID, hash); err != nil {
		s.serverError(w, r, err)
		return
	}

	// Changing a password is usually about revoking a device. Do it after the
	// hash is stored, so a failure here cannot leave the old password working
	// while the sessions are already gone.
	ended, err := s.sessions.RevokeOthers(r.Context(), r, user.ID)
	if err != nil {
		s.log.Error("could not end other sessions after a password change",
			"user_id", user.ID, "error", err)
	}

	s.log.Info("password changed", "user_id", user.ID, "sessions_ended", ended)
	msg := "Your password is changed."
	switch {
	case ended == 1:
		msg += " One other signed-in device was signed out."
	case ended > 1:
		msg += " " + plural(ended, "other signed-in device was", "other signed-in devices were") + " signed out."
	}
	redirectWith(w, r, "/profile", "ok", msg)
}

// postProfileAvatar accepts a picture, processes it, and stores the result.
//
// Nothing the uploader sent is stored: internal/avatar decodes and re-encodes,
// so the bytes that land in the database were produced here. The filename is
// never read, which is what makes path traversal structurally impossible rather
// than merely guarded against.
func (s *Server) postProfileAvatar(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())

	if !auth.CheckCSRF(r) {
		redirectWith(w, r, "/profile", "err", "Your session expired. Please try again.")
		return
	}
	if !s.allowAvatarUpload(user.ID) {
		s.log.Warn("avatar upload rate limited", "user_id", user.ID)
		redirectWith(w, r, "/profile", "err", "Too many uploads just now. Try again in a minute.")
		return
	}

	// Bound the body before parsing, so an oversized upload is refused rather
	// than buffered. The extra kilobyte covers the multipart envelope.
	r.Body = http.MaxBytesReader(w, r.Body, avatar.MaxUploadBytes+1024)
	if err := r.ParseMultipartForm(avatar.MaxUploadBytes); err != nil {
		redirectWith(w, r, "/profile", "err", "That image is too large. The limit is 2 MB.")
		return
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()

	file, _, err := r.FormFile("avatar")
	if err != nil {
		redirectWith(w, r, "/profile", "err", "Choose an image to upload.")
		return
	}
	defer func() { _ = file.Close() }()

	png, err := avatar.Process(file)
	if err != nil {
		redirectWith(w, r, "/profile", "err", avatarMessage(err))
		return
	}
	if err := s.store.SetAvatar(r.Context(), user.ID, png, time.Now().UTC()); err != nil {
		s.serverError(w, r, err)
		return
	}

	s.log.Info("avatar set", "user_id", user.ID, "bytes", len(png))
	redirectWith(w, r, "/profile", "ok", "Your picture is updated.")
}

// postProfileAvatarRemove clears the picture.
func (s *Server) postProfileAvatarRemove(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	if !s.profileFormOK(w, r) {
		return
	}
	if err := s.store.ClearAvatar(r.Context(), user.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.log.Info("avatar removed", "user_id", user.ID)
	redirectWith(w, r, "/profile", "ok", "Your picture is removed.")
}

// getAvatar serves an account's picture.
//
// Any signed-in user may fetch any account's avatar: people on a shared tab see
// each other, and the picture carries nothing the tab does not already show.
// Deactivated accounts still serve, because their ledger entries keep their
// author and those rows are still rendered.
func (s *Server) getAvatar(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}

	png, updated, err := s.store.GetAvatar(r.Context(), id)
	if err != nil {
		// No avatar and no such account answer the same way, so the route
		// cannot be used to enumerate accounts.
		http.NotFound(w, r)
		return
	}

	// The content is immutable for a given timestamp, so a conditional request
	// costs one indexed lookup and returns nothing.
	etag := `"` + updated + `"`
	if match := r.Header.Get("If-None-Match"); match == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	h := w.Header()
	// Always PNG: the stored bytes were re-encoded, so there is exactly one
	// type to declare and nosniff makes the declaration binding.
	h.Set("Content-Type", "image/png")
	h.Set("ETag", etag)
	// Private, because it identifies a person and must not sit in a shared
	// cache. must-revalidate keeps a changed picture from lingering.
	h.Set("Cache-Control", "private, max-age=300, must-revalidate")
	_, _ = w.Write(png)
}

// profileFormOK parses the form and checks CSRF, redirecting on failure.
func (s *Server) profileFormOK(w http.ResponseWriter, r *http.Request) bool {
	if err := r.ParseForm(); err != nil {
		redirectWith(w, r, "/profile", "err", "Could not read that form.")
		return false
	}
	if !auth.CheckCSRF(r) {
		redirectWith(w, r, "/profile", "err", "Your session expired. Please try again.")
		return false
	}
	return true
}

// avatarMessage turns a processing failure into something a person can act on.
// The distinctions matter: "too large" and "not an image" call for different
// next steps, and a generic message would hide which applies.
func avatarMessage(err error) string {
	switch {
	case errors.Is(err, avatar.ErrTooLarge):
		return "That image is too large. The limit is 2 MB."
	case errors.Is(err, avatar.ErrUnsupported):
		return "That file is not a PNG, JPEG, or GIF image."
	case errors.Is(err, avatar.ErrTooManyPixel):
		return "That image's dimensions are too large. Try one under 12,000 pixels a side."
	case errors.Is(err, avatar.ErrEmpty):
		return "Choose an image to upload."
	default:
		return "That image could not be read. Try a different file."
	}
}

// plural renders a count with the right verb agreement.
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

// postProfileNotify saves a user's notification preferences: their ntfy topic
// and the per-channel toggles.
func (s *Server) postProfileNotify(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	if !s.profileFormOK(w, r) {
		return
	}

	topic := strings.TrimSpace(r.PostFormValue("ntfy_topic"))
	wantNtfy := r.PostFormValue("notify_ntfy") != ""
	wantEmail := r.PostFormValue("notify_email") != ""

	// A topic is validated to the same strict charset the sender enforces, so a
	// value that could alter a request path or a header never reaches storage.
	if topic != "" && !notify.ValidTopic(topic) {
		redirectWith(w, r, "/profile", "err", "An ntfy topic may use only letters, numbers, dashes, and underscores.")
		return
	}
	// ntfy delivery needs a topic; asking for it without one is a mistake worth
	// naming rather than silently storing an unusable preference.
	if wantNtfy && topic == "" {
		redirectWith(w, r, "/profile", "err", "Choose an ntfy topic to receive push notifications.")
		return
	}

	if err := s.store.SetNotifyPrefs(r.Context(), user.ID, topic, wantEmail, wantNtfy); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.log.Info("notification prefs saved", "user_id", user.ID, "email", wantEmail, "ntfy", wantNtfy)
	redirectWith(w, r, "/profile", "ok", "Notification preferences saved.")
}
