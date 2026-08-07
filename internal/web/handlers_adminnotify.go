package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"net/url"
	"strconv"
	"strings"

	"github.com/johnzastrow/bitt/internal/auth"
	"github.com/johnzastrow/bitt/internal/notify"
	"github.com/johnzastrow/bitt/internal/store"
	"github.com/johnzastrow/bitt/internal/web/views"
)

// The instance notification settings screen: what this deployment delivers
// over, and what its reminders say by default.
//
// It edits the NON-SECRET half only. The SMTP password, the ntfy token, and the
// tick secret come from the environment or a file and are reported here as
// present-or-absent, never displayed and never writable -- see migration 0010
// for the reasoning, which is that a secret in the database needs a key that
// has to come from the environment anyway.
//
// The environment also wins over every field this screen does edit. A setting
// the environment specifies is shown read-only rather than hidden, because an
// administrator whose edit would silently not take needs to see why.

// getAdminNotify renders the settings screen.
func (s *Server) getAdminNotify(w http.ResponseWriter, r *http.Request) {
	inst, err := s.store.GetInstance(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	stored, err := s.store.ListInstanceReminders(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	env := s.cfg.Notify
	effective := s.notifyConfig(r.Context())

	s.render(w, r, http.StatusOK, views.AdminNotify(s.page(w, r, "Notifications"), views.AdminNotifyData{
		Delivery: inst.Delivery,
		Env: views.EnvLocks{
			SMTPHost:     env.SMTPHost != "",
			SMTPUsername: env.SMTPUsername != "",
			EmailFrom:    env.EmailFrom != "",
			NtfyBaseURL:  env.NtfyBaseURL != "",
		},
		Reminders:        stored,
		Effective:        s.defaultReminders(r.Context()),
		RemindersFromEnv: s.cfg.RemindersFromEnv,
		EmailReady:       effective.EmailEnabled(),
		NtfyReady:        effective.NtfyEnabled(),
		TickReady:        env.TickEnabled(),
		SMTPPasswordSet:  env.SMTPPassword != "",
		NtfyTokenSet:     env.NtfyToken != "",
		// A username with no password: the send will reach the server and fail
		// authentication. The username can come from either source, but the
		// password is environment-only and is never merged from storage, so it
		// is read from the environment config rather than the effective one.
		SMTPAuthIncomplete: effective.SMTPUsername != "" && env.SMTPPassword == "",
	}))
}

// postAdminDelivery saves the non-secret delivery settings.
func (s *Server) postAdminDelivery(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	if !s.adminFormOK(w, r) {
		return
	}

	env := s.cfg.Notify
	current, err := s.store.GetInstance(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	// A field the environment owns is rendered read-only, and a read-only input
	// still posts its value -- so the stored value is kept for those rather than
	// taking what came back. Without this, opening the form and saving would
	// copy the environment's settings into the database, and they would silently
	// take over the day someone unset the variable.
	d := store.Delivery{
		SMTPHost:     pick(env.SMTPHost != "", current.Delivery.SMTPHost, r.PostFormValue("smtp_host")),
		SMTPUsername: pick(env.SMTPUsername != "", current.Delivery.SMTPUsername, r.PostFormValue("smtp_username")),
		EmailFrom:    pick(env.EmailFrom != "", current.Delivery.EmailFrom, r.PostFormValue("email_from")),
		NtfyBaseURL:  pick(env.NtfyBaseURL != "", current.Delivery.NtfyBaseURL, r.PostFormValue("ntfy_url")),
	}
	if env.SMTPHost != "" {
		d.SMTPPort = current.Delivery.SMTPPort
	} else if d.SMTPPort, err = parsePort(r.PostFormValue("smtp_port")); err != nil {
		redirectWith(w, r, "/admin/notifications", "err", err.Error())
		return
	}

	if err := validateDelivery(&d); err != nil {
		redirectWith(w, r, "/admin/notifications", "err", err.Error())
		return
	}

	if err := s.store.SetDelivery(r.Context(), d); err != nil {
		s.serverError(w, r, err)
		return
	}

	// Logged without any value that could be sensitive: which channels are now
	// configured, not who they authenticate as.
	s.log.Info("instance delivery settings changed", "user_id", user.ID,
		"smtp_set", d.SMTPHost != "", "ntfy_set", d.NtfyBaseURL != "")
	redirectWith(w, r, "/admin/notifications", "ok", "Delivery settings saved.")
}

// postAdminReminders saves the instance-wide default reminders.
func (s *Server) postAdminReminders(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	if !s.adminFormOK(w, r) {
		return
	}

	// The environment wins, so a post that would edit environment-set reminders
	// is refused rather than saved into a table nothing reads.
	if s.cfg.RemindersFromEnv {
		redirectWith(w, r, "/admin/notifications", "err",
			"The reminders are set in the environment, which wins over anything saved here. Unset BITT_REMINDER_* and restart to edit them in the app.")
		return
	}

	rs, err := parseTabReminders(
		r.PostFormValue("reminder_days"),
		r.PostFormValue("reminder_title"),
		r.PostFormValue("reminder_body"),
	)
	if err != nil {
		redirectWith(w, r, "/admin/notifications", "err", err.Error())
		return
	}

	if err := s.store.SetInstanceReminders(r.Context(), rs); err != nil {
		s.serverError(w, r, err)
		return
	}

	s.log.Info("instance reminders changed", "user_id", user.ID, "count", len(rs))
	if len(rs) == 0 {
		redirectWith(w, r, "/admin/notifications", "ok",
			"Default reminders cleared. The built-in 14/7/1 applies again.")
		return
	}
	redirectWith(w, r, "/admin/notifications", "ok", "Default reminders saved.")
}

// postAdminNotifyTest sends a test notification to the requesting administrator
// over every configured channel their account has a coordinate for.
//
// It exists because the only other way to know delivery works is to wait for a
// real reminder to come due, days out, and discover a broken SMTP setting when a
// payee does not get reminded. This proves the channel end to end now.
//
// It is a pure side effect: it posts no ledger entry and writes no sent-claim,
// so it never touches the balance path and can be run as often as needed without
// affecting real reminders. It ignores the admin's own per-channel reminder
// toggles on purpose -- they explicitly asked to test, so an opt-out from
// reminders should not silence the test -- and sends only over channels that are
// configured and for which the admin has a coordinate (an email address, or an
// ntfy topic set on their profile).
func (s *Server) postAdminNotifyTest(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	if !s.adminFormOK(w, r) {
		return
	}

	notifier := s.notifierFor(r.Context())
	if notifier == nil || !notifier.Enabled() {
		redirectWith(w, r, "/admin/notifications", "err",
			"No delivery channel is configured yet. Set up email or ntfy below first.")
		return
	}

	rcpt := notify.Recipient{Email: user.Email, Topic: user.NtfyTopic}
	msg := notify.Message{
		Title: "BitTabby test notification",
		Body: "This is a test from BitTabby's notification settings. " +
			"If you are reading it, delivery on this channel works.",
	}
	if s.cfg.BaseURL != "" {
		msg.Link = s.cfg.BaseURL
	}

	var okChans, failMsgs []string
	tested := 0
	for _, ch := range []notify.Channel{notify.ChannelEmail, notify.ChannelNtfy} {
		if !notifier.Available(ch, rcpt) {
			continue
		}
		tested++
		if err := notifier.Deliver(r.Context(), ch, rcpt, msg); err != nil {
			s.log.Warn("test notification failed", "user_id", user.ID, "channel", string(ch), "error", err)
			failMsgs = append(failMsgs, string(ch)+" failed: "+trimError(err))
			continue
		}
		s.log.Info("test notification sent", "user_id", user.ID, "channel", string(ch))
		okChans = append(okChans, string(ch))
	}

	switch {
	case tested == 0:
		redirectWith(w, r, "/admin/notifications", "err",
			"Nothing to test: your account has no email address or ntfy topic for the configured channels. Set a topic on your profile to test ntfy.")
	case len(failMsgs) == 0:
		redirectWith(w, r, "/admin/notifications", "ok",
			"Test sent over "+strings.Join(okChans, " and ")+" to you. Check that it arrived.")
	case len(okChans) == 0:
		redirectWith(w, r, "/admin/notifications", "err", strings.Join(failMsgs, "; "))
	default:
		redirectWith(w, r, "/admin/notifications", "ok",
			"Sent over "+strings.Join(okChans, " and ")+". The rest: "+strings.Join(failMsgs, "; "))
	}
}

// trimError renders a delivery error for the admin diagnostic: one line and
// bounded. Delivery errors describe the transport -- a refused connection, a
// server's rejection of a recipient, an ntfy HTTP status -- and the notify
// package builds them without ever including the SMTP password or the ntfy
// token, so they are safe to show the administrator who is debugging their own
// instance, which is the whole point of a test button.
func trimError(err error) string {
	msg := strings.ReplaceAll(strings.TrimSpace(err.Error()), "\n", " ")
	const max = 140
	if len(msg) > max {
		msg = msg[:max] + "..."
	}
	return msg
}

// adminFormOK parses and CSRF-checks an admin settings post.
func (s *Server) adminFormOK(w http.ResponseWriter, r *http.Request) bool {
	if err := r.ParseForm(); err != nil {
		redirectWith(w, r, "/admin/notifications", "err", "Could not read that form.")
		return false
	}
	if !auth.CheckCSRF(r) {
		redirectWith(w, r, "/admin/notifications", "err", "Your session expired. Please try again.")
		return false
	}
	return true
}

// pick returns the kept value when the environment owns the setting, and the
// submitted one otherwise.
func pick(envOwns bool, kept, submitted string) string {
	if envOwns {
		return kept
	}
	return strings.TrimSpace(submitted)
}

// parsePort reads an optional SMTP port. Blank means unset, which lets the
// built-in 587 apply.
func parsePort(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > 65535 {
		return 0, errors.New("The SMTP port must be a number between 1 and 65535, or blank for the default.")
	}
	return n, nil
}

// validateDelivery checks the settings before they are stored, and normalises
// the ntfy URL.
//
// The rules match what config.NotifyConfig.validate enforces on the environment,
// because the two configure the same thing and a value accepted here must not
// be one that would have refused to start.
func validateDelivery(d *store.Delivery) error {
	if len(d.SMTPHost) > 253 || len(d.SMTPUsername) > 320 || len(d.EmailFrom) > 320 {
		return errors.New("One of those values is too long.")
	}
	if d.SMTPHost != "" && strings.ContainsAny(d.SMTPHost, " \t\r\n/") {
		return errors.New("The SMTP server should be a hostname, with no scheme, path, or spaces.")
	}
	// A From address is what an email is sent as, so an invalid one means every
	// send fails later rather than here. It is also the header-injection
	// surface: net/mail rejects a value carrying a newline.
	if d.SMTPHost != "" {
		if d.EmailFrom == "" {
			return errors.New("An SMTP server needs a from address to send as.")
		}
		if _, err := mail.ParseAddress(d.EmailFrom); err != nil {
			return fmt.Errorf("%q is not a valid email address. Use a plain address, or the \"Name <address>\" form.", d.EmailFrom)
		}
	}

	if d.NtfyBaseURL == "" {
		return nil
	}
	d.NtfyBaseURL = strings.TrimRight(d.NtfyBaseURL, "/")
	u, err := url.Parse(d.NtfyBaseURL)
	if err != nil || u.Host == "" {
		return errors.New("The ntfy server must be a URL, like https://ntfy.sh.")
	}
	// https only, matching decision D1: a plaintext push carries the tab name
	// and the amount owed across the network.
	if u.Scheme != "https" {
		return errors.New("The ntfy server must use https. A plain-http push would carry the tab name and the amount in the clear.")
	}
	return nil
}
