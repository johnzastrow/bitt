package web

import (
	"errors"
	"net/http"

	"github.com/johnzastrow/bitt/internal/auth"
	"github.com/johnzastrow/bitt/internal/store"
)

// requireReconcile guards every bank reconciliation route (RECON-01), checked
// on each request rather than trusted from the menu:
//
//   - not an administrator: 404, as for any admin route;
//   - an administrator without Can reconcile: 403, since they can already see
//     on the People screen that the feature exists.
//
// The permission is read from the account loaded for this request, so
// removing it takes effect on the very next request.
func (s *Server) requireReconcile(next http.Handler) http.Handler {
	return s.requireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := userFrom(r.Context())
		if !user.MayReconcile() {
			s.log.Warn("reconciliation route denied",
				"path", r.URL.Path, "user_id", user.ID, "remote", clientIP(r))
			http.Error(w, "You do not have the Can reconcile permission.", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	}))
}

// postAdminUserReconcile grants or removes Can reconcile on one account. Any
// administrator may do it, to any administrator, themselves included; the
// store refuses a non-administrator. Every change is logged with both ids.
func (s *Server) postAdminUserReconcile(w http.ResponseWriter, r *http.Request) {
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

	var on bool
	switch r.PostFormValue("can_reconcile") {
	case "true":
		on = true
	case "false":
	default:
		redirectWith(w, r, "/admin/users", "err", "Could not read that form.")
		return
	}

	err := s.store.SetCanReconcile(r.Context(), id, on)
	switch {
	case errors.Is(err, store.ErrNotAdmin):
		redirectWith(w, r, "/admin/users", "err",
			"Only an administrator can be allowed to reconcile.")
		return
	case errors.Is(err, store.ErrNotFound):
		http.NotFound(w, r)
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}

	s.log.Info("reconcile permission changed",
		"target_user_id", id, "can_reconcile", on, "by_user_id", actor.ID)
	if on {
		redirectWith(w, r, "/admin/users", "ok",
			"They can now reconcile bank statements: Reconciliation is in their account menu.")
		return
	}
	redirectWith(w, r, "/admin/users", "ok", "They can no longer reconcile bank statements.")
}
