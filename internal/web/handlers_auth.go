package web

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/johnzastrow/bitt/internal/auth"
	"github.com/johnzastrow/bitt/internal/store"
	"github.com/johnzastrow/bitt/internal/web/views"
)

// ---------------------------------------------------------------------------
// First-run setup (AUTH-03)
// ---------------------------------------------------------------------------

func (s *Server) getSetup(w http.ResponseWriter, r *http.Request) {
	if !s.setupPending(r.Context()) {
		// The screen locks permanently once used.
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, views.Setup(s.page(w, r, "Setup"), s.cfg.DefaultTimezone))
}

func (s *Server) postSetup(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		redirectWith(w, r, "/setup", "err", "Could not read that form.")
		return
	}
	if !auth.CheckCSRF(r) {
		redirectWith(w, r, "/setup", "err", "Your session expired. Please try again.")
		return
	}
	if !s.setupPending(r.Context()) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	name := strings.TrimSpace(r.PostFormValue("display_name"))
	email := strings.TrimSpace(r.PostFormValue("email"))
	password := r.PostFormValue("password")
	timezone := strings.TrimSpace(r.PostFormValue("timezone"))
	if timezone == "" {
		timezone = s.cfg.DefaultTimezone
	}

	if name == "" || !looksLikeEmail(email) {
		redirectWith(w, r, "/setup", "err", "Please provide a name and a valid email address.")
		return
	}
	if _, err := time.LoadLocation(timezone); err != nil {
		redirectWith(w, r, "/setup", "err", "That is not a recognized timezone.")
		return
	}
	if err := auth.ValidatePassword(password); err != nil {
		redirectWith(w, r, "/setup", "err", err.Error())
		return
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	admin, err := s.store.CompleteSetup(r.Context(), store.User{
		Email:        email,
		DisplayName:  name,
		PasswordHash: hash,
		IsAdmin:      true,
	}, timezone)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			// Another request won the race, or setup already happened.
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		s.serverError(w, r, err)
		return
	}

	s.log.Info("first-run setup completed", "user_id", admin.ID, "timezone", timezone)

	if err := s.sessions.Issue(r.Context(), w, admin.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	redirectWith(w, r, "/", "ok", "Welcome to BitTabby.")
}

// ---------------------------------------------------------------------------
// Login and logout (AUTH-02)
// ---------------------------------------------------------------------------

func (s *Server) getLogin(w http.ResponseWriter, r *http.Request) {
	if s.setupPending(r.Context()) {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	if _, err := s.sessions.Resolve(r); err == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, views.Login(s.page(w, r, "Sign in")))
}

func (s *Server) postLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		redirectWith(w, r, "/login", "err", "Could not read that form.")
		return
	}
	if !auth.CheckCSRF(r) {
		redirectWith(w, r, "/login", "err", "Your session expired. Please try again.")
		return
	}

	email := strings.TrimSpace(r.PostFormValue("email"))
	password := r.PostFormValue("password")

	// One message for every failure mode, so the response never distinguishes
	// "no such account" from "wrong password".
	const generic = "That email and password combination is not correct."

	user, err := s.store.GetUserByEmail(r.Context(), email)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Spend comparable time so timing does not reveal registration.
			auth.DummyVerify(password)
			redirectWith(w, r, "/login", "err", generic)
			return
		}
		s.serverError(w, r, err)
		return
	}

	if !user.Active() {
		auth.DummyVerify(password)
		redirectWith(w, r, "/login", "err", generic)
		return
	}

	if err := auth.VerifyPassword(password, user.PasswordHash); err != nil {
		s.log.Warn("failed login", "email_domain", emailDomain(email), "remote", clientIP(r))
		redirectWith(w, r, "/login", "err", generic)
		return
	}

	if err := s.sessions.Issue(r.Context(), w, user.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.log.Info("login", "user_id", user.ID)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) postLogout(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if !auth.CheckCSRF(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if err := s.sessions.Revoke(r.Context(), w, r); err != nil {
		s.serverError(w, r, err)
		return
	}
	redirectWith(w, r, "/login", "ok", "Signed out.")
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

// looksLikeEmail is a deliberately loose structural check. Deliverability is
// proven by sending mail, not by a regex, and an over-strict pattern rejects
// valid addresses.
func looksLikeEmail(s string) bool {
	if len(s) < 3 || len(s) > 254 {
		return false
	}
	at := strings.LastIndex(s, "@")
	if at <= 0 || at == len(s)-1 {
		return false
	}
	if strings.ContainsAny(s, " \t\r\n") {
		return false
	}
	return strings.Contains(s[at+1:], ".")
}

// emailDomain returns just the domain, so a failed-login log line carries
// useful signal without recording the full address.
func emailDomain(email string) string {
	if at := strings.LastIndex(email, "@"); at >= 0 && at < len(email)-1 {
		return email[at+1:]
	}
	return "unknown"
}

// clientIP reports the peer address. Proxy headers are deliberately ignored:
// they are trivially forged, and on a household deployment the peer address is
// the honest value.
func clientIP(r *http.Request) string {
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	return host
}
