package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/johnzastrow/bitt/internal/auth"
	"github.com/johnzastrow/bitt/internal/store"
	"github.com/johnzastrow/bitt/internal/web/views"
)

// requireAdmin wraps a handler so only administrators reach it (AUTH-04).
// It answers 404 rather than 403, so the existence of admin routes is not
// confirmed to a non-admin who goes looking.
func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return s.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := userFrom(r.Context())
		if user == nil || !user.IsAdmin {
			s.log.Warn("admin route denied",
				"path", r.URL.Path, "user_id", userID(user), "remote", clientIP(r))
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	}))
}

func userID(u *store.User) int64 {
	if u == nil {
		return 0
	}
	return u.ID
}

func (s *Server) getAdminUsers(w http.ResponseWriter, r *http.Request) {
	rows, err := s.adminRows(r)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, views.AdminUsers(s.page(w, r, "People"), rows))
}

// adminRows lists accounts and marks which ones cannot be deactivated.
func (s *Server) adminRows(r *http.Request) ([]views.AdminUser, error) {
	me := userFrom(r.Context())

	users, err := s.store.ListUsers(r.Context())
	if err != nil {
		return nil, err
	}

	activeAdmins := 0
	for _, u := range users {
		if u.IsAdmin && u.Active() {
			activeAdmins++
		}
	}

	rows := make([]views.AdminUser, 0, len(users))
	for _, u := range users {
		rows = append(rows, views.AdminUser{
			User:      u,
			Self:      me != nil && me.ID == u.ID,
			LastAdmin: u.IsAdmin && u.Active() && activeAdmins <= 1,
		})
	}
	return rows, nil
}

func (s *Server) postAdminUser(w http.ResponseWriter, r *http.Request) {
	actor := userFrom(r.Context())

	if err := r.ParseForm(); err != nil {
		redirectWith(w, r, "/admin/users", "err", "Could not read that form.")
		return
	}
	if !auth.CheckCSRF(r) {
		redirectWith(w, r, "/admin/users", "err", "Your session expired. Please try again.")
		return
	}

	name := strings.TrimSpace(r.PostFormValue("display_name"))
	email := strings.TrimSpace(r.PostFormValue("email"))
	password := r.PostFormValue("password")
	isAdmin := r.PostFormValue("is_admin") == "1"

	if name == "" || len(name) > 120 {
		redirectWith(w, r, "/admin/users", "err", "Please give them a name.")
		return
	}
	if !looksLikeEmail(email) {
		redirectWith(w, r, "/admin/users", "err", "That does not look like an email address.")
		return
	}
	if err := auth.ValidatePassword(password); err != nil {
		redirectWith(w, r, "/admin/users", "err", err.Error())
		return
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	created, err := s.store.CreateUser(r.Context(), store.User{
		Email:        email,
		DisplayName:  name,
		PasswordHash: hash,
		IsAdmin:      isAdmin,
	})
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			redirectWith(w, r, "/admin/users", "err", "An account with that email already exists.")
			return
		}
		s.serverError(w, r, err)
		return
	}

	s.log.Info("account created",
		"new_user_id", created.ID, "is_admin", isAdmin, "by_user_id", actor.ID)
	redirectWith(w, r, "/admin/users", "ok", created.DisplayName+" can now sign in.")
}

func (s *Server) postAdminUserActive(w http.ResponseWriter, r *http.Request) {
	actor := userFrom(r.Context())

	id, ok := pathID(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		redirectWith(w, r, "/admin/users", "err", "Could not read that form.")
		return
	}
	if !auth.CheckCSRF(r) {
		redirectWith(w, r, "/admin/users", "err", "Your session expired. Please try again.")
		return
	}

	active := r.PostFormValue("active") == "true"

	// Deactivating yourself would end your own session mid-request. Refuse it
	// rather than logging the admin out of the instance they are administering.
	if !active && id == actor.ID {
		redirectWith(w, r, "/admin/users", "err", "You cannot deactivate your own account.")
		return
	}

	err := s.store.SetUserActive(r.Context(), id, active)
	switch {
	case errors.Is(err, store.ErrLastAdmin):
		redirectWith(w, r, "/admin/users", "err",
			"That is the only administrator left. Make someone else an admin first.")
		return
	case errors.Is(err, store.ErrNotFound):
		http.NotFound(w, r)
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}

	s.log.Info("account activity changed",
		"target_user_id", id, "active", active, "by_user_id", actor.ID)
	if active {
		redirectWith(w, r, "/admin/users", "ok", "Account reactivated.")
		return
	}
	redirectWith(w, r, "/admin/users", "ok", "Account deactivated and its sessions ended.")
}

// attachableUsers lists accounts that could be added to a tab: active, and not
// already participating.
func (s *Server) attachableUsers(r *http.Request, tabID int64) ([]store.User, error) {
	users, err := s.store.ListUsers(r.Context())
	if err != nil {
		return nil, err
	}
	participants, err := s.store.ListParticipants(r.Context(), tabID)
	if err != nil {
		return nil, err
	}

	onTab := make(map[int64]bool, len(participants))
	for _, p := range participants {
		onTab[p.UserID] = true
	}

	var out []store.User
	for _, u := range users {
		if u.Active() && !onTab[u.ID] {
			out = append(out, u)
		}
	}
	return out, nil
}

// parseIDPath is a small helper for routes carrying a numeric id.
func parseIDPath(raw string) (int64, bool) {
	id, err := strconv.ParseInt(raw, 10, 64)
	return id, err == nil && id > 0
}
