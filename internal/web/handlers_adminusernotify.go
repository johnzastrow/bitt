package web

import (
	"net/http"
	"strings"

	"github.com/johnzastrow/bitt/internal/auth"
	"github.com/johnzastrow/bitt/internal/notify"
)

// REM-03: an administrator may edit another account's NOTIFICATION settings.
//
// The gap this closes: two accounts held a retired ntfy topic, so their
// notifications published successfully into a void. Nobody but those two people
// could fix it, because notification settings lived only on a person's own
// profile -- and an administrator watching the whole instance had no way to
// correct a setting they could plainly see was wrong.
//
// Deliberately narrow. Topic and the two channel toggles, nothing else. An
// administrator resetting somebody's password or changing their email is a
// different feature with a different risk profile, and it must not arrive as a
// side effect of this one.

func (s *Server) postAdminUserNotify(w http.ResponseWriter, r *http.Request) {
	actor := userFrom(r.Context())
	if !auth.CheckCSRF(r) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	id, ok := pathID(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}

	target, err := s.store.GetUser(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	topic := strings.TrimSpace(r.PostFormValue("ntfy_topic"))
	// Same validation the person's own profile applies. A topic an
	// administrator sets must be one the owner could have set themselves.
	if topic != "" && !notify.ValidTopic(topic) {
		redirectWith(w, r, "/admin/users", "err",
			"That ntfy topic is not valid: letters, numbers, dashes and underscores only.")
		return
	}
	email := r.PostFormValue("notify_email") == "1"
	ntfy := r.PostFormValue("notify_ntfy") == "1"

	if err := s.store.SetNotifyPrefs(r.Context(), target.ID, topic, email, ntfy); err != nil {
		s.serverError(w, r, err)
		return
	}

	// Both ids, deliberately. Editing another person's settings is exactly the
	// action an audit trail exists for, and "who changed whose" is the question
	// it has to answer.
	s.log.Info("admin changed notification settings for another account",
		"actor_user_id", actor.ID, "target_user_id", target.ID,
		"ntfy_topic_set", topic != "", "notify_email", email, "notify_ntfy", ntfy)

	redirectWith(w, r, "/admin/users", "ok",
		"Notification settings updated for "+target.DisplayName+".")
}
